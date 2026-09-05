import Foundation

/// Quality presets (Phase 3 / 7). Auto monitors network health and moves the
/// target between a floor (10–20 ms on clean LAN) and a safety ceiling; fixed
/// presets pin the behavior.
enum QualityPreset: String, CaseIterable, Identifiable {
    case auto = "Auto"
    case lowestLatency = "Lowest Latency"
    case lossless = "Lossless"
    case robust = "Robust"
    case advanced = "Advanced"

    var id: String { rawValue }

    struct Profile {
        /// Jitter target in milliseconds (ring fill target).
        var targetMs: Double
        /// Maximum the auto policy may raise the target to (ms).
        var ceilingMs: Double
        /// Prefer PCM (true) or allow Opus in Auto (false).
        var preferPCM: Bool
        /// Rename for UI.
        var description: String
    }

    var profile: Profile {
        switch self {
        case .auto:
            return Profile(targetMs: 15, ceilingMs: 120, preferPCM: false,
                           description: "Adapts buffering and codec to network health")
        case .lowestLatency:
            return Profile(targetMs: 10, ceilingMs: 30, preferPCM: true,
                           description: "PCM, very small adaptive buffer")
        case .lossless:
            return Profile(targetMs: 25, ceilingMs: 80, preferPCM: true,
                           description: "PCM with a slightly safer buffer")
        case .robust:
            return Profile(targetMs: 60, ceilingMs: 250, preferPCM: false,
                           description: "Opus + FEC, tolerates poor Wi-Fi")
        case .advanced:
            return Profile(targetMs: 20, ceilingMs: 120, preferPCM: false,
                           description: "Manual control of every parameter")
        }
    }
}

/// The adaptive jitter target policy (Phase 3):
/// - starts very low on a clean LAN,
/// - rises fast on instability (loss/jitter/underruns),
/// - decays slowly when conditions improve,
/// - never oscillates (rate limiting + hysteresis).
struct AdaptiveJitterPolicy {

    var preset: QualityPreset
    var sampleRate: Double

    // Internal state (ms).
    private(set) var currentTargetMs: Double
    /// Advisory only (for the v2 engine path): suggests enabling FEC while
    /// loss arrives in bursts or at a high rate. Updated with hysteresis in
    /// update().
    private(set) var fecAdvisory = false
    private var lastChange: TimeInterval = 0
    private var stableSince: TimeInterval = 0

    init(preset: QualityPreset, sampleRate: Double) {
        self.preset = preset
        self.sampleRate = sampleRate
        self.currentTargetMs = preset.profile.targetMs
    }

    static let minimumMs = 8.0
    static let fallRateMsPerSec = 6.0      // decrease slowly
    static let riseRateMsPerSec = 60.0     // increase quickly
    private let holdAfterChange: TimeInterval = 1.0
    private let improvementHold: TimeInterval = 4.0

    struct Network {
        var lossPercent: Double
        var jitterMs: Double
        var underruns: UInt64
        var bufferDepthMs: Double
        /// Longest recent loss burst in packets (from ReorderBuffer).
        var maxBurst: Int = 0
    }

    /// Advances the policy with the latest network sample. Returns the target
    /// in ms. Call at ~1 Hz.
    mutating func update(_ net: Network, now: TimeInterval) -> Double {
        // FEC advisory (v2 path only; purely advisory here). Hysteresis:
        // latch on at ≥3-packet bursts or ≥3% loss, clear only below 1% loss
        // with bursts ≤1, so the flag doesn't flap around the thresholds.
        if net.maxBurst >= 3 || net.lossPercent >= 3.0 {
            fecAdvisory = true
        } else if net.lossPercent < 1.0 && net.maxBurst <= 1 {
            fecAdvisory = false
        }

        let profile = preset.profile
        switch preset {
        case .lowestLatency, .lossless, .robust:
            // Fixed presets: clamp within [min, ceiling], start at profile.
            currentTargetMs = profile.targetMs
            return currentTargetMs
        case .advanced:
            return currentTargetMs // manual target set by UI
        case .auto:
            break
        }

        let ceiling = profile.ceilingMs
        let floorMs = Self.minimumMs

        // Required target from observed conditions (with headroom).
        var required = floorMs
        required += net.jitterMs * 2.5
        required += net.lossPercent * 3.0
        // Bursty loss needs more depth than the same average rate spread
        // evenly: a 3% loss arriving in long bursts can empty the buffer in
        // one hit, so each packet of the longest recent burst adds ~4 ms.
        required += Double(min(max(net.maxBurst, 0), 8)) * 4.0
        required += net.bufferDepthMs * 0.15 // mild feedback from buffer state

        if lastChange == 0 {
            lastChange = now
            stableSince = now
        }

        let healthy = net.lossPercent < 0.5 && net.jitterMs < 5 && net.underruns == 0

        if currentTargetMs < required {
            // Rise quickly toward required (capped by ceiling).
            let rise = min(required - currentTargetMs, Self.riseRateMsPerSec * 1.0)
            currentTargetMs = min(ceiling, currentTargetMs + rise)
            lastChange = now
            stableSince = now
        } else if healthy, now - stableSince > improvementHold,
                  now - lastChange > holdAfterChange {
            // Fall slowly when stable.
            currentTargetMs = max(floorMs, currentTargetMs - Self.fallRateMsPerSec * 1.0)
            lastChange = now
        }
        if !healthy {
            stableSince = now
        }
        return currentTargetMs
    }

    /// Manual override (Advanced).
    private(set) var manualTargetMs: Double = 20
    mutating func setManualTarget(ms: Double) {
        manualTargetMs = max(Self.minimumMs, min(400, ms))
        currentTargetMs = manualTargetMs
    }

    var activeTargetMs: Double { currentTargetMs }

    mutating func reset() {
        currentTargetMs = preset.profile.targetMs
        lastChange = 0
        stableSince = 0
    }
}
