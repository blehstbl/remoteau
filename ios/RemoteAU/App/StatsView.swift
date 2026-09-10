import SwiftUI

/// Expandable detailed statistics (Phase 10 groundwork). Clearly labels
/// software-measured values vs. the Bluetooth delay that can't be measured
/// from software alone.
struct StatsView: View {
    var stats: StatsSnapshot

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            row("Buffer depth", ms(stats.bufferDepthMs))
            row("Buffer target", ms(stats.targetBufferMs))
            row("Network jitter", ms(stats.jitterMs))
            row("Packet loss", String(format: "%.2f %%", stats.lossPercent))
            row("Packet rate", "\(stats.packetRatePerSec) pkt/s")
            row("Bitrate", String(format: "%.0f kbps", stats.bitrateKbps))
            row("Underruns", "\(stats.underruns)")
            row("Dropped (overrun) frames", "\(stats.droppedFrames)")
            row("Overrun events", "\(stats.overrunEvents)")
            row("Concealed (PLC)", "\(stats.concealedFrames)")
            row("Late packets", "\(stats.latePackets)")
            row("Reordered packets", "\(stats.reorderedPackets)")
            row("Longest loss burst", "\(stats.maxBurstLoss) packets")
            row("FEC advisory", stats.fecAdvisory ? "Yes" : "No")
            Divider()
            row("Codec", stats.codec)
            row("Drift correction", String(format: "%+.3f %%", (stats.driftRatio - 1.0) * 100.0))
            row("Capture clock", String(format: "%.1f Hz", stats.captureFrameRateHz))
            row("Capture-clock discontinuities", "\(stats.discontinuities)")
            Divider()
            row("Software latency (est.)", ms(stats.softwareLatencyMs))
            row("Output route", stats.outputRoute.isEmpty ? "—" : stats.outputRoute)
            Text("AirPods/Bluetooth adds additional delay that iOS does not expose to apps; the latency above is the software portion only. RTT is not measurable on the legacy v1 protocol.")
                .font(.caption2)
                .foregroundStyle(.secondary)
                .padding(.top, 4)
        }
        .font(.footnote)
    }

    private func row(_ title: String, _ value: String) -> some View {
        HStack {
            Text(title)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
                .monospacedDigit()
        }
        .accessibilityElement(children: .combine)
    }

    private func ms(_ v: Double) -> String {
        String(format: "%.1f ms", v)
    }
}

/// Compact live statistics for the secure v2 engine. Shown alongside (and
/// independently of) the legacy v1 snapshot when the v2 session reports
/// `connected`.
struct V2StatsView: View {
    var stats: V2Stats

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Secure session (v2)")
                .font(.footnote.weight(.semibold))
            row("State", stats.state.isEmpty ? "—" : stats.state)
            row("Round-trip time", ms(stats.rttMs))
            row("Packet loss", String(format: "%.2f %%", stats.lossPct))
            row("Late", String(format: "%.2f %%", stats.latePct))
            row("Network jitter", ms(stats.jitterMs))
            row("Buffer", ms(stats.bufferMs))
            row("Packets seen", whole(stats.packetsSeen))
            row("Lost packets", whole(stats.lossPackets))
            row("Late packets", whole(stats.latePackets))
            row("Reordered packets", whole(stats.reorderedPackets))
            row("Concealed frames", whole(stats.concealedFrames))
            row("Longest loss burst", "\(whole(stats.maxBurst)) packets")
        }
        .font(.footnote)
    }

    private func row(_ title: String, _ value: String) -> some View {
        HStack {
            Text(title)
                .foregroundStyle(.secondary)
            Spacer()
            Text(value)
                .monospacedDigit()
        }
        .accessibilityElement(children: .combine)
    }

    private func ms(_ v: Double) -> String {
        String(format: "%.1f ms", v)
    }

    private func whole(_ v: Double) -> String {
        String(format: "%.0f", v)
    }
}
