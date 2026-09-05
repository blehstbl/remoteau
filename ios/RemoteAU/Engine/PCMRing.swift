import Foundation
import os

/// Single-producer / single-consumer PCM ring between the network thread and
/// the real-time render callback.
///
/// Real-time rules (mirrors remote-au's ring discipline):
/// - The render callback uses try-lock; if the lock is held it outputs silence
///   and never spins or blocks.
/// - The producer (network thread) may block briefly on the unfair lock; the
///   critical section is only memcpy-sized.
/// - `drain` performs the S16LE→float32 conversion and drift-corrected
///   fractional resampling while holding the lock (pure arithmetic, no
///   allocation).
final class PCMRing {

    private let buf: UnsafeMutablePointer<UInt8>
    private let capacityBytes: Int
    private var bytesPerFrame: Int
    private var lock = os_unfair_lock()
    private var readPos = 0
    private var usedBytes = 0

    // Statistics (updated under the lock, read from anywhere).
    private var droppedFramesCount: UInt64 = 0
    private var overrunEvents: UInt64 = 0
    private var underrunCount: UInt64 = 0
    private var peakUsedBytes: Int = 0

    // Drift state, updated only under the lock (see tryDrainAdvanced).
    private var drift = DriftController(targetFrames: 720, sampleRate: 48000)
    private(set) var lastDrainRatio: Double = 1.0

    init(capacityBytes: Int, bytesPerFrame: Int) {
        self.bytesPerFrame = max(2, bytesPerFrame)
        var cap = max(capacityBytes, self.bytesPerFrame)
        cap -= cap % self.bytesPerFrame
        self.capacityBytes = cap
        self.buf = UnsafeMutablePointer<UInt8>.allocate(capacity: cap)
        self.buf.initialize(repeating: 0, count: cap)
    }

    deinit {
        buf.deallocate()
    }

    // MARK: Producer (network thread)

    /// Appends S16LE PCM. Never fails for the caller; drops oldest on overflow
    /// (bounded buffer discipline) and counts dropped frames.
    func write(_ bytes: UnsafeRawPointer, count: Int) {
        guard count > 0 else { return }
        let n = count - (count % bytesPerFrame)
        guard n > 0 else { return }
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }

        if n > capacityBytes {
            // Too large for the whole ring: keep only the tail.
            let drop = n - capacityBytes
            droppedFramesCount += UInt64(drop / bytesPerFrame)
            writeLocked(bytes + drop, capacityBytes)
            return
        }

        let free = capacityBytes - usedBytes
        if n > free {
            let dropOldest = alignUp(n - free)
            consumeLocked(dropOldest)
            droppedFramesCount += UInt64(dropOldest / bytesPerFrame)
            overrunEvents &+= 1
        }
        writeLocked(bytes, n)
        if usedBytes > peakUsedBytes { peakUsedBytes = usedBytes }
    }

    private func writeLocked(_ bytes: UnsafeRawPointer, _ n: Int) {
        var writePos = (readPos + usedBytes) % capacityBytes
        var src = bytes.assumingMemoryBound(to: UInt8.self)
        var remaining = n
        while remaining > 0 {
            let chunk = min(remaining, capacityBytes - writePos)
            memcpy(buf + writePos, src, chunk)
            writePos = (writePos + chunk) % capacityBytes
            src += chunk
            remaining -= chunk
        }
        usedBytes += n
    }

    private func consumeLocked(_ bytes: Int) {
        let n = min(bytes, usedBytes)
        readPos = (readPos + n) % capacityBytes
        usedBytes -= n
    }

    private func alignUp(_ bytes: Int) -> Int {
        let rem = bytes % bytesPerFrame
        return rem == 0 ? bytes : bytes + bytesPerFrame - rem
    }

    // MARK: Consumer (render callback, real-time)

    /// Full real-time drain used by the render callback: under a single
    /// try-lock it measures the fill, advances the drift controller, and
    /// converts S16LE→deinterleaved float32 with fractional resampling.
    /// Returns produced frames; -1 when the lock was busy (caller fills
    /// silence and must not retry or wait).
    func tryDrainAdvanced(into channels: UnsafeMutablePointer<UnsafeMutablePointer<Float32>>?,
                          channelCount: Int,
                          frameCount: Int,
                          targetFrames: Double,
                          phase: inout Double) -> Int {
        guard frameCount > 0, channelCount > 0 else { return 0 }
        guard os_unfair_lock_trylock(&lock) else {
            return -1
        }
        defer { os_unfair_lock_unlock(&lock) }

        drift.targetFrames = targetFrames
        let ratio = drift.update(fillFrames: Double(usedBytes / bytesPerFrame))
        lastDrainRatio = ratio

        let srcFramesAvailable = usedBytes / bytesPerFrame
        if srcFramesAvailable < 1 {
            underrunCount += 1
            return 0
        }

        var outFrame = 0
        var pos = phase
        while outFrame < frameCount {
            let i = Int(pos)
            let frac = pos - Double(i)
            // Interpolation needs frames i and i+1 to exist.
            guard i + 1 <= srcFramesAvailable - 1 else { break }
            let base = (readPos + i * bytesPerFrame) % capacityBytes
            for ch in 0..<channelCount {
                let cOff = ch * 2
                let s0 = s16At(base + cOff)
                let nextOff = base + cOff + bytesPerFrame
                let s1 = s16At(nextOff % capacityBytes)
                let v = (Double(s0) + (Double(s1) - Double(s0)) * frac) / 32768.0
                channels[ch][outFrame] = Float(v)
            }
            outFrame += 1
            pos += ratio
        }
        phase = pos - floor(pos)
        let consumed = Int(pos - phase) // integer frames consumed
        let consumedBytes = consumed * bytesPerFrame
        if consumedBytes > 0 {
            consumeLocked(consumedBytes)
        }
        return outFrame
    }

    private func s16At(_ bytePos: Int) -> Int16 {
        let b0 = buf[bytePos % capacityBytes]
        let b1 = buf[(bytePos + 1) % capacityBytes]
        return Int16(bitPattern: UInt16(b1) << 8 | UInt16(b0))
    }

    /// Counts an underrun without ever blocking the caller (render-safe).
    func noteUnderrun() {
        if os_unfair_lock_trylock(&lock) {
            underrunCount += 1
            os_unfair_lock_unlock(&lock)
        }
    }

    // MARK: Stats (any thread)

    var queuedFrames: Int {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        return usedBytes / bytesPerFrame
    }

    var droppedFrames: UInt64 {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        return droppedFramesCount
    }

    /// Number of drop-oldest overrun events (distinct from `droppedFrames`,
    /// which counts individual frames dropped).
    var overruns: UInt64 {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        return overrunEvents
    }

    var underruns: UInt64 {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        return underrunCount
    }

    /// Clears the buffer (on reconnect) without releasing memory.
    func reset() {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        readPos = 0
        usedBytes = 0
    }

    /// Re-configures the frame size when the stream format changes (safe: the
    /// render callback's drain runs under the same lock).
    func setBytesPerFrame(_ n: Int) {
        guard n >= 2, n != bytesPerFrame else { return }
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        bytesPerFrame = n
        readPos = 0
        usedBytes = 0
    }

    /// Re-arms the drift controller for a new stream (locked swap).
    func configureDrift(targetFrames: Double, sampleRate: Double) {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        drift = DriftController(targetFrames: targetFrames, sampleRate: sampleRate)
    }

    /// Current drift ratio (for stats).
    var driftRatio: Double {
        os_unfair_lock_lock(&lock)
        defer { os_unfair_lock_unlock(&lock) }
        return lastDrainRatio
    }
}
