import Foundation
import AVFoundation
import AudioToolbox
import os

/// Owns AVAudioSession + AVAudioEngine with an AVAudioSourceNode pulling from
/// the PCMRing. Handles interruptions, route changes and media-services
/// resets so audio keeps playing through AirPods across ordinary disruptions
/// (Phase 2/9 behaviors).
final class AudioEngineController {

    private let ring: PCMRing

    private var engine: AVAudioEngine?
    private var sourceNode: AVAudioSourceNode?

    // Real-time state, written only inside the render callback.
    private var phase: Double = 0
    private var cachedTargetFrames: Double = 720

    // Shared config updated from non-RT threads under a tiny lock.
    private var targetLock = os_unfair_lock()
    private var targetFrames: Double = 720

    // Route/session bookkeeping.
    private var observers: [NSObjectProtocol] = []
    private let stateQueue = DispatchQueue(label: "remoteau.audio.state")
    private(set) var outputRouteName: String = ""
    private(set) var outputSampleRate: Double = 48000
    private(set) var engineRunning = false

    /// Milliseconds of buffered audio currently in the ring (for stats).
    var ringDepthMs: Double {
        Double(ring.queuedFrames) / max(1.0, outputSampleRate) * 1000.0
    }

    /// Current drift-correction ratio (for stats display).
    var driftRatio: Double {
        ring.driftRatio
    }

    init(ring: PCMRing) {
        self.ring = ring
    }

    deinit {
        removeObservers()
    }

    // MARK: Session & engine lifecycle

    func configureSession() throws {
        let session = AVAudioSession.sharedInstance()
        try session.setCategory(.playback, mode: .default, options: [])
        try session.setPreferredSampleRate(48000)
        try session.setPreferredIOBufferDuration(0.008)
        try session.setActive(true)
        installObservers()
    }

    func deactivateSession() {
        try? AVAudioSession.sharedInstance().setActive(false, options: .notifyOthersOnDeactivation)
    }

    /// Starts (or rebuilds) the engine. Safe to call repeatedly.
    func start() throws {
        try stateQueue.sync {
            if engineRunning { return }
            try AVAudioSession.sharedInstance().setActive(true)

            let engine = AVAudioEngine()
            let outFormat = engine.outputNode.outputFormat(forBus: 0)
            outputSampleRate = outFormat.sampleRate
            if outputSampleRate <= 0 { outputSampleRate = 48000 }

            let channels = max(1, Int(outFormat.channelCount))

            phase = 0
            cachedTargetFrames = currentTargetFramesLocked()
            ring.configureDrift(targetFrames: cachedTargetFrames, sampleRate: outputSampleRate)

            let nodeFormat = AVAudioFormat(
                standardFormatWithSampleRate: outFormat.sampleRate,
                channels: AVAudioChannelCount(channels)
            )!

            let node = AVAudioSourceNode(format: nodeFormat) { [weak self] silence, _, frames, outputData in
                guard let self else {
                    silence.pointee = true
                    return noErr
                }
                return self.render(isSilence: silence, frameCount: Int(frames), outputData: outputData)
            }

            engine.attach(node)
            engine.connect(node, to: engine.mainMixerNode, format: nodeFormat)
            engine.prepare()

            try engine.start()

            self.engine = engine
            self.sourceNode = node
            engineRunning = true
            outputRouteName = Self.currentRouteName()
        }
    }

    func stop() {
        stateQueue.sync {
            guard engineRunning else { return }
            if let engine {
                engine.stop()
            }
            if let node = sourceNode {
                engine?.detach(node)
            }
            sourceNode = nil
            engine = nil
            engineRunning = false
        }
    }

    private func render(isSilence: UnsafeMutablePointer<ObjCBool>,
                        frameCount: Int,
                        outputData: UnsafeMutablePointer<AudioBufferList>) -> OSStatus {
        let buffers = UnsafeMutableAudioBufferListPointer(outputData)
        let channelCount = buffers.count
        let outFrames = frameCount
        guard outFrames > 0, channelCount > 0 else {
            isSilence.pointee = true
            return noErr
        }

        var chans: [UnsafeMutablePointer<Float32>] = (0..<channelCount).map { i in
            buffers[i].mData!.assumingMemoryBound(to: Float32.self)
        }

        // Cached target (updated at ~1 Hz by the adaptive policy). Read with
        // try-lock so the callback never waits; on miss the cached value is
        // used.
        if os_unfair_lock_trylock(&targetLock) {
            cachedTargetFrames = targetFrames
            os_unfair_lock_unlock(&targetLock)
        }

        let produced = chans.withUnsafeMutableBufferPointer { p in
            ring.tryDrainAdvanced(
                into: p.baseAddress,
                channelCount: channelCount,
                frameCount: outFrames,
                targetFrames: cachedTargetFrames,
                phase: &phase
            )
        }

        if produced < 0 {
            // Ring lock busy: never wait — silence this callback.
            fillSilence(chans, frames: outFrames)
            isSilence.pointee = true
            return noErr
        }
        if produced < outFrames {
            fillSilence(chans, frames: outFrames, from: produced)
        }
        isSilence.pointee = ObjCBool(produced == 0)
        return noErr
    }

    private func fillSilence(_ channels: [UnsafeMutablePointer<Float32>],
                             frames: Int,
                             from: Int = 0) {
        guard from < frames else { return }
        for ch in 0..<channels.count {
            memset(channels[ch] + from, 0, MemoryLayout<Float32>.size * (frames - from))
        }
        if from == 0 {
            ring.noteUnderrun()
        }
    }

    // MARK: Target updates (adaptive policy thread)

    func setTargetMs(_ ms: Double) {
        os_unfair_lock_lock(&targetLock)
        targetFrames = ms * outputSampleRate / 1000.0
        os_unfair_lock_unlock(&targetLock)
    }

    /// Re-arms the drift controller around the current target without
    /// clearing the ring (controlled re-prime after a capture-clock
    /// discontinuity). Audio keeps flowing; only drift state is reset.
    func resetDrift() {
        let target = currentTargetFramesLocked()
        ring.configureDrift(targetFrames: target, sampleRate: outputSampleRate)
    }

    private func currentTargetFramesLocked() -> Double {
        os_unfair_lock_lock(&targetLock)
        defer { os_unfair_lock_unlock(&targetLock) }
        return targetFrames
    }

    // MARK: Notifications (interruptions / route changes / media resets)

    private func installObservers() {
        guard observers.isEmpty else { return }
        let center = NotificationCenter.default

        observers.append(center.addObserver(
            forName: AVAudioSession.interruptionNotification, object: nil, queue: nil
        ) { [weak self] note in
            self?.handleInterruption(note)
        })

        observers.append(center.addObserver(
            forName: AVAudioSession.routeChangeNotification, object: nil, queue: nil
        ) { [weak self] note in
            self?.handleRouteChange(note)
        })

        observers.append(center.addObserver(
            forName: AVAudioSession.mediaServicesWereResetNotification, object: nil, queue: nil
        ) { [weak self] _ in
            self?.handleMediaServicesReset()
        })
    }

    private func removeObservers() {
        for o in observers { NotificationCenter.default.removeObserver(o) }
        observers.removeAll()
    }

    private func handleInterruption(_ note: Notification) {
        guard let info = note.userInfo,
              let typeRaw = info[AVAudioSessionInterruptionTypeKey] as? UInt,
              let type = AVAudioSession.InterruptionType(rawValue: typeRaw) else { return }
        // Handlers run off the state queue; start/stop sync internally.
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            switch type {
            case .began:
                // iOS suspended output; release the graph cleanly.
                self.stop()
            case .ended:
                let optsRaw = info[AVAudioSessionInterruptionOptionKey] as? UInt ?? 0
                let opts = AVAudioSession.InterruptionOptions(rawValue: optsRaw)
                if opts.contains(.shouldResume) {
                    try? AVAudioSession.sharedInstance().setActive(true)
                    try? self.start()
                }
            @unknown default:
                break
            }
        }
    }

    private func handleRouteChange(_ note: Notification) {
        guard let info = note.userInfo,
              let reasonRaw = info[AVAudioSessionRouteChangeReasonKey] as? UInt,
              let reason = AVAudioSession.RouteChangeReason(rawValue: reasonRaw) else { return }
        outputRouteName = Self.currentRouteName()
        switch reason {
        case .newDeviceAvailable, .oldDeviceUnavailable:
            // AirPods connected/disconnected: keep streaming; the engine
            // follows the route automatically. Nudge the buffer accounting so
            // the new device starts with audio already flowing.
            ring.noteUnderrun()
        default:
            break
        }
    }

    private func handleMediaServicesReset() {
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            guard let self else { return }
            self.stop()
            try? self.configureSession()
            try? self.start()
        }
    }

    static func currentRouteName() -> String {
        let outputs = AVAudioSession.sharedInstance().currentRoute.outputs
        return outputs.first?.portName ?? "No output"
    }
}
