import Foundation

/// Packet-level reordering, gap handling and PCM loss concealment. Lives on
/// the network thread; output is S16LE PCM pushed into the ring.
///
/// Design (Phase 3 / Phase 5):
/// - A bounded reorder window holds packets that arrive early (seq >
///   expected) so mild reordering doesn't become loss.
/// - Missing regions are concealed with an exponential-decay hold of the last
///   good sample — simple and safe (Opus mode later brings real PLC).
/// - When good audio resumes after concealment, the first frames of the
///   returning payload are crossfaded with the concealment tail so the seam
///   doesn't click.
/// - Capture-clock discontinuities (sender reset/loop, or forward jumps
///   beyond two seconds) are counted and request a controlled re-prime
///   instead of being treated as loss.
/// - Tracks arrival jitter, loss, late and reordered packets for stats and
///   the adaptive target.
final class ReorderBuffer {

    struct Packet {
        var seq: UInt64
        var captureFrame: UInt64
        var payload: [UInt8] // copied, owned
        var frames: Int
    }

    private var nextSeq: UInt64?
    private var future: [UInt64: Packet] = [:]

    // Last good samples per channel, for concealment.
    private var lastSample: [Int16] = []

    let bytesPerFrame: Int
    private let maxWindowPackets: Int
    private let maxGapFrames: Int

    // Statistics (EWMA where noted).
    private(set) var lossPackets: UInt64 = 0
    private(set) var latePackets: UInt64 = 0
    private(set) var reorderedPackets: UInt64 = 0
    private(set) var concealedFrames: UInt64 = 0
    private(set) var packetsSeen: UInt64 = 0
    private(set) var arrivalJitterMs: Double = 0
    private(set) var lossPercent: Double = 0
    private(set) var burstLoss: Int = 0
    private(set) var maxBurstLoss: Int = 0
    private(set) var captureFrameRateHz: Double = 0
    private var lastCaptureFrame: UInt64 = 0
    private var lastCaptureArrival: Double = 0

    // Capture-clock continuity: `expectedCaptureFrame` is where the next good
    // packet's captureFrame should land (previous + frames written). A value
    // behind it, or ahead by more than two seconds, is a discontinuity.
    private(set) var discontinuities: UInt64 = 0
    private(set) var pendingReprime = false
    private var expectedCaptureFrame: UInt64 = 0
    private var captureClockValid = false

    // Last generated concealment frames (S16LE, interleaved per channel),
    // kept so the seam into returning good audio can be crossfaded.
    private let concealTailFrames = 64
    private var concealTail: [Int16] = []

    private var lastArrival: Double = 0
    private var lastFlush: Double = 0
    private let flushHoldSeconds: Double

    /// Called with S16LE PCM (already in seq order). Runs on the network
    /// thread; the implementation must be non-blocking (ring write).
    var sink: ((UnsafeRawPointer, Int) -> Void)?

    /// Sample rate of the stream, set after handshake (used for jitter math).
    var sampleRate: Double = 48000

    init(bytesPerFrame: Int, frameDurationMs: Double, maxWindowPackets: Int = 24) {
        self.bytesPerFrame = bytesPerFrame
        self.maxWindowPackets = maxWindowPackets
        self.maxGapFrames = max(64, Int(2000.0 * frameDurationMs)) // ~2 s cap
        self.flushHoldSeconds = min(0.010, max(0.002, frameDurationMs / 1000.0 * 1.5))
    }

    /// Feeds one received audio packet; writes in seq order into the sink and
    /// conceals gaps.
    func accept(seq: UInt64, captureFrame: UInt64, payload: ArraySlice<UInt8>) {
        let now = ProcessInfo.processInfo.systemUptime
        packetsSeen += 1

        // Inter-arrival jitter (EWMA of |delta - expected|).
        let frames = payload.count / bytesPerFrame
        if lastArrival > 0, frames > 0 {
            let expectedDelta = Double(frames) / max(1.0, sampleRate)
            let deviationMs = abs(now - lastArrival - expectedDelta) * 1000.0
            arrivalJitterMs += 0.1 * (deviationMs - arrivalJitterMs)
        }
        lastArrival = now

        if let expected = nextSeq {
            if seq < expected {
                latePackets += 1
                nudgeLoss(1.0)
                return // stale/duplicate
            }
            if seq > expected {
                reorderedPackets += 1
                if future[seq] == nil, future.count < maxWindowPackets {
                    future[seq] = Packet(seq: seq, captureFrame: captureFrame,
                                         payload: Array(payload), frames: frames)
                }
                nudgeLoss(1.0)
                return
            }
        }

        // seq == expected (or first packet).
        writeConsecutive(seq: seq, captureFrame: captureFrame, payload: payload)
        flushFuture(now: now)
    }

    /// Called periodically from the network loop: after a short hold, flush
    /// the reorder window and conceal anything still missing.
    func tick(now: Double = ProcessInfo.processInfo.systemUptime) {
        guard nextSeq != nil, !future.isEmpty else { return }
        guard now - lastFlush >= flushHoldSeconds else { return }
        lastFlush = now
        flushFuture(now: now)
    }

    private func flushFuture(now: Double) {
        var expected = nextSeq
        guard expected != nil else { return }

        // Write as many consecutive held packets as available.
        while let e = expected, let p = future.removeValue(forKey: e) {
            writeConsecutive(seq: p.seq, captureFrame: p.captureFrame, payload: p.payload[...])
            expected = nextSeq
        }

        // Conceal up to the earliest still-held packet, if any.
        if let minHeld = future.keys.min(), let e = expected, minHeld > e {
            let missing = Int(clamping: minHeld - e)
            conceal(frames: min(missing, maxGapFrames))
            nextSeq = minHeld
            expected = minHeld
            while let p = future.removeValue(forKey: expected!) {
                writeConsecutive(seq: p.seq, captureFrame: p.captureFrame, payload: p.payload[...])
                expected = nextSeq
            }
        }
    }

    private func writeConsecutive(seq: UInt64, captureFrame: UInt64, payload: ArraySlice<UInt8>) {
        let frames = payload.count / bytesPerFrame
        checkCaptureDiscontinuity(captureFrame: captureFrame, frames: frames)
        if nextSeq == nil {
            nextSeq = seq &+ UInt64(frames)
            ensureLastSample()
        } else {
            nextSeq = seq &+ UInt64(frames)
        }
        // A good frame ends any loss burst.
        burstLoss = 0
        sinkWrite(payload)
        trackCaptureFrame(seq, frames)
    }

    private func trackCaptureFrame(_ captureFrame: UInt64, _ frames: Int) {
        let now = ProcessInfo.processInfo.systemUptime
        if lastCaptureArrival > 0 {
            let dt = now - lastCaptureArrival
            if dt > 0.001, captureFrame > lastCaptureFrame {
                let hz = Double(captureFrame - lastCaptureFrame) / dt
                captureFrameRateHz += 0.2 * (hz - captureFrameRateHz)
            }
        }
        lastCaptureFrame = captureFrame &+ UInt64(frames)
        lastCaptureArrival = now
    }

    // MARK: Capture-clock continuity

    /// Detects capture-clock discontinuities in the write path: a capture
    /// frame behind the expected value (sender reset/loop) or ahead by more
    /// than two seconds of frames is a clock jump, not ordinary packet loss.
    /// The event is counted, a controlled re-prime is requested, and the new
    /// value is accepted as the base — the gap is never concealed.
    private func checkCaptureDiscontinuity(captureFrame: UInt64, frames: Int) {
        if captureClockValid {
            if captureFrame < expectedCaptureFrame {
                discontinuities &+= 1
                pendingReprime = true
            } else if Double(captureFrame &- expectedCaptureFrame) > 2.0 * sampleRate {
                discontinuities &+= 1
                pendingReprime = true
            }
        }
        expectedCaptureFrame = captureFrame &+ UInt64(frames)
        captureClockValid = true
    }

    /// Returns true once after a detected capture-clock discontinuity and
    /// clears the pending flag (consumed by housekeeping for a re-prime).
    func consumeReprime() -> Bool {
        guard pendingReprime else { return false }
        pendingReprime = false
        return true
    }

    // MARK: Sink write (crossfade on resumption)

    /// Writes one in-sequence packet downstream. When concealment audio was
    /// just generated, the first frames of the returning real audio are
    /// crossfaded with the concealment tail so the seam doesn't click. Runs
    /// on the network thread.
    private func sinkWrite(_ payload: ArraySlice<UInt8>) {
        if concealTail.isEmpty {
            writeDownstream(Array(payload))
        } else {
            writeDownstream(crossfadedWithTail(Array(payload)))
            concealTail.removeAll()
        }
        // Last-sample memory tracks the real payload, never the blended copy.
        rememberLastSamples(payload)
    }

    /// Pushes PCM bytes into the sink (ring write; non-blocking).
    private func writeDownstream(_ bytes: [UInt8]) {
        bytes.withUnsafeBytes { raw in
            if let base = raw.baseAddress {
                sink?(base, raw.count)
            }
        }
    }

    /// Blends the first frames of `payload` with the saved concealment tail
    /// (linear ramp: payload 0→1, tail 1→0, ending fully on the payload) and
    /// returns the result as a new array; the payload itself is left
    /// untouched.
    private func crossfadedWithTail(_ payload: [UInt8]) -> [UInt8] {
        let channels = max(1, bytesPerFrame / 2)
        let framesInPayload = payload.count / bytesPerFrame
        let tailFrames = concealTail.count / channels
        let n = min(concealTailFrames, tailFrames, framesInPayload)
        guard n > 0 else { return payload }

        var out = payload
        for f in 0..<n {
            let payloadGain = Float(f + 1) / Float(n)
            let tailGain = 1.0 - payloadGain
            for ch in 0..<channels {
                let off = (f * channels + ch) * 2
                let p = Int16(bitPattern: UInt16(payload[off + 1]) << 8 | UInt16(payload[off]))
                let tail = concealTail[f * channels + ch]
                let mixed = Int16(clamping: Int(Float(p) * payloadGain + Float(tail) * tailGain))
                out[off] = UInt8(truncatingIfNeeded: mixed)
                out[off + 1] = UInt8(truncatingIfNeeded: UInt16(bitPattern: mixed) >> 8)
            }
        }
        return out
    }

    private func rememberLastSamples(_ payload: ArraySlice<UInt8>) {
        ensureLastSample()
        let channels = lastSample.count
        let sampleCount = payload.count / 2
        guard sampleCount >= channels else { return }
        let arr = Array(payload)
        let start = (sampleCount - channels) * 2
        for ch in 0..<channels {
            let off = start + ch * 2
            lastSample[ch] = Int16(bitPattern: UInt16(arr[off + 1]) << 8 | UInt16(arr[off]))
        }
    }

    private func ensureLastSample() {
        let channels = max(1, bytesPerFrame / 2)
        if lastSample.count != channels {
            lastSample = Array(repeating: 0, count: channels)
        }
    }

    /// Conceals `frames` frames of missing audio: exponential-decay hold of
    /// the last good sample per channel. The last few generated frames are
    /// remembered so `sinkWrite` can crossfade the resumption seam.
    private func conceal(frames: Int) {
        guard frames > 0 else { return }
        concealedFrames += UInt64(frames)
        lossPackets += 1
        nudgeLoss(1.0)
        burstLoss += 1
        if burstLoss > maxBurstLoss {
            maxBurstLoss = burstLoss
        }

        ensureLastSample()
        let channels = lastSample.count
        let chunkFrames = min(frames, 480)
        var chunk = [UInt8](repeating: 0, count: chunkFrames * bytesPerFrame)

        var remaining = frames
        var decay: Double = 1.0
        while remaining > 0 {
            let n = min(remaining, chunkFrames)
            for f in 0..<n {
                let gain = decay
                for ch in 0..<channels {
                    let v = Int16(clamping: Int(Float(lastSample[ch]) * Float(gain)))
                    let off = (f * channels + ch) * 2
                    chunk[off] = UInt8(truncatingIfNeeded: v)
                    chunk[off + 1] = UInt8(truncatingIfNeeded: UInt16(bitPattern: v) >> 8)
                    concealTail.append(v)
                }
            }
            decay = pow(0.9994, Double(n)) * decay
            chunk.withUnsafeBytes { raw in
                if let base = raw.baseAddress {
                    sink?(base, n * bytesPerFrame)
                }
            }
            remaining -= n
        }

        // Keep only the last few frames of synthetic audio for the resumption
        // crossfade in sinkWrite.
        let keepSamples = concealTailFrames * channels
        if concealTail.count > keepSamples {
            concealTail.removeFirst(concealTail.count - keepSamples)
        }
    }

    private func nudgeLoss(_ amount: Double) {
        lossPercent += 0.05 * (amount - lossPercent)
    }

    /// Clears state (reconnect).
    func reset() {
        nextSeq = nil
        future.removeAll()
        lastSample = []
        lossPercent = 0
        arrivalJitterMs = 0
        lastArrival = 0
        lastFlush = 0
        burstLoss = 0
        maxBurstLoss = 0
        captureFrameRateHz = 0
        lastCaptureFrame = 0
        lastCaptureArrival = 0
        concealTail.removeAll()
        pendingReprime = false
        expectedCaptureFrame = 0
        captureClockValid = false
    }
}
