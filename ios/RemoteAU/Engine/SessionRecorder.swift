import Foundation
import AVFoundation

/// Writes the received PCM stream to a 16-bit WAV in Documents/Recordings.
///
/// Threading:
/// - `append(_:)` is called from the network thread (v1 reorder sink) or the
///   Go media callback (v2). It only copies bytes into a lock-guarded pending
///   buffer and schedules a drain; all file I/O happens on `ioQueue`.
/// - `stop()` flushes pending audio synchronously and returns the file URL.
///
/// Memory bounding:
/// The pending buffer is capped at `maxPendingBytes` (a few seconds of stereo
/// audio). If the producer outruns the disk, `append` drops the *oldest*
/// pending bytes first, so a recording degrades to gaps instead of growing
/// without bound or blocking the caller. AVAudioFile flushes as it writes, so
/// only this in-flight chunk is ever held in memory.
final class SessionRecorder {

    private let ioQueue = DispatchQueue(label: "dev.remoteau.recorder")
    private let lock = NSLock()

    private var pending = Data()
    private var scheduled = false
    private var recording = false

    private var file: AVAudioFile?
    private var format: AVAudioFormat?
    private var url: URL?
    /// Last finalized recording, retained so repeated `stop()` calls are
    /// idempotent.
    private var lastURL: URL?

    /// Hard cap on buffered-but-unwritten bytes (~4 s of 48 kHz stereo S16).
    private let maxPendingBytes: Int

    init(maxPendingBytes: Int = 48_000 * 4 * 4) {
        self.maxPendingBytes = max(4096, maxPendingBytes)
    }

    // MARK: Lifecycle

    /// Starts a new recording. Any previous recording is finalized first.
    func start(sampleRate: Int, channels: Int) throws {
        _ = stop()

        let rate = max(8000, sampleRate)
        let ch = max(1, channels)

        let dir = try Self.recordingsDirectory()
        let out = dir.appendingPathComponent("RemoteAU-\(Self.timestamp()).wav")

        let settings: [String: Any] = [
            AVFormatIDKey: kAudioFormatLinearPCM,
            AVSampleRateKey: rate,
            AVNumberOfChannelsKey: ch,
            AVLinearPCMBitDepthKey: 16,
            AVLinearPCMIsFloatKey: false,
            AVLinearPCMIsBigEndianKey: false,
            AVLinearPCMIsNonInterleaved: false,
        ]
        // Pin the processing format to interleaved Int16 so the buffers built
        // in `write` are guaranteed to match `file.processingFormat`
        // (AVAudioFile otherwise picks its own processing format, which may be
        // deinterleaved float and would reject Int16 buffers).
        let newFile = try AVAudioFile(forWriting: out, settings: settings,
                                      commonFormat: .pcmFormatInt16, interleaved: true)
        guard let newFormat = AVAudioFormat(commonFormat: .pcmFormatInt16,
                                            sampleRate: Double(rate),
                                            channels: AVAudioChannelCount(ch),
                                            interleaved: true) else {
            throw NSError(domain: "dev.remoteau.recorder", code: -1,
                          userInfo: [NSLocalizedDescriptionKey: "Cannot build recording format"])
        }

        lock.lock()
        file = newFile
        format = newFormat
        url = out
        pending.removeAll(keepingCapacity: true)
        scheduled = false
        recording = true
        lock.unlock()
    }

    /// Enqueues S16LE interleaved PCM. Safe from any thread; a no-op while not
    /// recording.
    func append(_ pcm: [UInt8]) {
        guard !pcm.isEmpty else { return }
        lock.lock()
        guard recording else {
            lock.unlock()
            return
        }
        pending.append(contentsOf: pcm)
        // Drop the oldest audio if the disk cannot keep up.
        if pending.count > maxPendingBytes {
            pending.removeFirst(pending.count - maxPendingBytes)
        }
        let shouldSchedule = !scheduled
        scheduled = true
        lock.unlock()
        if shouldSchedule {
            ioQueue.async { [weak self] in self?.drain() }
        }
    }

    /// Flushes pending audio and closes the file (AVAudioFile finalizes on
    /// deinit / release). Returns the saved URL. Idempotent: once a recording
    /// has been finalized, repeated calls keep returning that same URL. After
    /// this returns, `append(_:)` is a no-op until the next `start(...)`.
    func stop() -> URL? {
        lock.lock()
        let wasActive = recording
        recording = false
        let activeURL = url
        lock.unlock()

        if wasActive {
            // Flush remaining chunks; the drain loop exits once pending is empty.
            ioQueue.sync { [weak self] in self?.drain() }
        }

        lock.lock()
        if let activeURL {
            lastURL = activeURL
        }
        let saved = activeURL ?? lastURL
        file = nil          // releases the last reference → closes the WAV
        format = nil
        url = nil
        pending.removeAll(keepingCapacity: true)
        scheduled = false
        lock.unlock()
        return saved
    }

    // MARK: Private (ioQueue only)

    private func drain() {
        while true {
            lock.lock()
            if pending.isEmpty {
                scheduled = false
                lock.unlock()
                return
            }
            let chunk = pending
            pending.removeAll(keepingCapacity: true)
            guard let currentFile = file, let currentFormat = format else {
                // No destination (already closed): drop the backlog and stop
                // rather than spin on a non-drainable queue.
                scheduled = false
                lock.unlock()
                return
            }
            lock.unlock()

            write(chunk, to: currentFile, format: currentFormat)
        }
    }

    private func write(_ chunk: Data, to file: AVAudioFile, format: AVAudioFormat) {
        let bytesPerFrame = Int(format.streamDescription.pointee.mBytesPerFrame)
        guard bytesPerFrame > 0 else { return }
        let frameCount = chunk.count / bytesPerFrame
        guard frameCount > 0 else { return }
        guard let buffer = AVAudioPCMBuffer(pcmFormat: format,
                                            frameCapacity: AVAudioFrameCount(frameCount)) else {
            return
        }
        buffer.frameLength = AVAudioFrameCount(frameCount)
        guard let dst = buffer.int16ChannelData?[0] else { return }
        chunk.withUnsafeBytes { raw in
            if let base = raw.baseAddress {
                _ = memcpy(dst, base, frameCount * bytesPerFrame)
            }
        }
        do {
            try file.write(from: buffer)
        } catch {
            // Best-effort: drop the chunk on a write failure so recording
            // continues rather than silently stopping.
        }
    }

    // MARK: Helpers

    private static func recordingsDirectory() throws -> URL {
        let docs = FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0]
        let dir = docs.appendingPathComponent("Recordings", isDirectory: true)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        return dir
    }

    private static func timestamp() -> String {
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.dateFormat = "yyyyMMdd-HHmmss"
        return formatter.string(from: Date())
    }
}
