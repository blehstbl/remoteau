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
            row("Dropped frames", "\(stats.droppedFrames)")
            row("Concealed (PLC)", "\(stats.concealedFrames) frames")
            row("Late packets", "\(stats.latePackets)")
            row("Reordered packets", "\(stats.reorderedPackets)")
            Divider()
            row("Software latency (est.)", ms(stats.softwareLatencyMs))
            row("Output route", stats.outputRoute.isEmpty ? "—" : stats.outputRoute)
            Text("AirPods/Bluetooth adds additional delay that iOS does not expose to apps; the latency above is the software portion only.")
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
