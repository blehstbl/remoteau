import Foundation

/// Aggregate receiver statistics snapshot for the UI (Phase 10).
/// All network-derived numbers are EWMA-smoothed in the engine layers.
struct StatsSnapshot: Equatable {
    var bufferDepthMs: Double = 0
    var targetBufferMs: Double = 0
    var jitterMs: Double = 0
    var lossPercent: Double = 0
    var underruns: UInt64 = 0
    var droppedFrames: UInt64 = 0
    var concealedFrames: UInt64 = 0
    var latePackets: UInt64 = 0
    var reorderedPackets: UInt64 = 0
    var packetsSeen: UInt64 = 0
    var packetRatePerSec: Int = 0
    var bitrateKbps: Double = 0
    var outputRoute: String = ""
    var engineRunning: Bool = false
    var codec: String = "pcm"
    var driftRatio: Double = 1.0
    var maxBurstLoss: Int = 0
    var captureFrameRateHz: Double = 0
    var rttMs: Double = 0

    /// Rough software-side latency estimate: buffering + one frame duration.
    /// The Bluetooth link adds more (not measurable from software alone).
    var softwareLatencyMs: Double {
        bufferDepthMs + 10.0
    }
}

/// A PC discovered on the LAN (v2 engine answers these; remote-au `recv`
/// answers in legacy mode).
struct DiscoveredPeer: Identifiable, Equatable {
    var id: String { "\(name)-\(address)" }
    var name: String
    var address: String
    var port: Int
    var protocolVersion: Int
    var paired: Bool
}
