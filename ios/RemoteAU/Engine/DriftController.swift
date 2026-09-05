import Foundation

/// Estimates clock drift between the sender's capture clock and the device's
/// output clock and produces a micro-resampling ratio so the ring buffer
/// neither drains nor overfills. Avoids periodic sample drop/insert (per
/// plan): correction is continuous, tiny (<±0.1%), and inaudible.
struct DriftController {

    /// Desired ring fill in source frames.
    var targetFrames: Double
    /// Aggressiveness of correction (fraction of error corrected per second).
    var gain: Double = 0.02
    /// Hard clamp on |ratio - 1|.
    var maxCorrection: Double = 0.001

    private(set) var fillEWMA: Double = 0
    private(set) var ratio: Double = 1.0
    private var initialized = false

    /// Sample rate (frames per second) of the source stream.
    var sampleRate: Double = 48000

    init(targetFrames: Double, sampleRate: Double) {
        self.targetFrames = targetFrames
        self.sampleRate = sampleRate
    }

    /// Updates the controller with the current ring fill (source frames) and
    /// returns the ratio to use for this callback (source frames per output
    /// frame).
    mutating func update(fillFrames: Double) -> Double {
        if !initialized {
            initialized = true
            fillEWMA = fillFrames
            ratio = 1.0
        }
        // EWMA smooths bursty arrivals so the ratio never wobbles.
        fillEWMA += 0.02 * (fillFrames - fillEWMA)

        let error = fillEWMA - targetFrames
        // Correct a fraction of the error per callback (~100 callbacks/sec at
        // 10ms), so the whole `gain` fraction is corrected per second.
        let perCallback = gain * 0.01
        let delta = (error / max(1.0, sampleRate)) * perCallback
        ratio = clamp(ratio + delta, 1.0 - maxCorrection, 1.0 + maxCorrection)
        return ratio
    }

    /// Retunes the target (preset change) smoothly.
    mutating func setTarget(frames: Double) {
        targetFrames = frames
    }

    mutating func reset() {
        initialized = false
        fillEWMA = 0
        ratio = 1.0
    }

    private func clamp(_ v: Double, _ lo: Double, _ hi: Double) -> Double {
        min(max(v, lo), hi)
    }
}
