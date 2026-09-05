import Foundation

/// Packet-level reordering, gap handling and PCM loss concealment. Lives on
/// the network thread; output is S16LE PCM pushed into the ring.
///
/// Design (Phase 3):
/// - A bounded reorder window holds packets that arrive early (seq >
///   expected) so mild reordering doesn't become loss.
/// - Missing regions are concealed with an exponential-decay hold of the last
///   good sample — simple and safe (Opus mode later brings real PLC).
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
        if nextSeq == nil {
            nextSeq = seq &+ UInt64(frames)
            ensureLastSample()
        } else {
            nextSeq = seq &+ UInt64(frames)
        }
        write(payload)
    }

    private func write(_ payload: ArraySlice<UInt8>) {
        let arr = payload
        arr.withUnsafeBytes { raw in
            if let base = raw.baseAddress {
                sink?(base, raw.count)
            }
        }
        rememberLastSamples(payload)
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
    /// the last good sample per channel.
    private func conceal(frames: Int) {
        guard frames > 0 else { return }
        concealedFrames += UInt64(frames)
        lossPackets += 1
        nudgeLoss(1.0)

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
                    chunk[off] = UInt8(bitPattern: UInt8(v & 0xFF))
                    chunk[off + 1] = UInt8(bitPattern: UInt8((v >> 8) & 0xFF))
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
    }
}
