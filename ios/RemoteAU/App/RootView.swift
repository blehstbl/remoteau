import SwiftUI

/// Main screen: connection hero, quality presets, discovered/manual PCs and
/// an expandable statistics view.
struct RootView: View {
    @EnvironmentObject private var model: ReceiverModel
    @Environment(\.scenePhase) private var scenePhase
    @State private var showAdvanced = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: Theme.spacingL) {
                    connectionHero
                    if case .streaming = model.state {
                        qualitySection
                        liveStatsSummary
                    }
                    devicesSection
                    errorFooter
                }
                .padding(.horizontal, Theme.spacingM)
                .padding(.bottom, 32)
            }
            .background(Color(.systemGroupedBackground))
            .navigationTitle("RemoteAU")
            .toolbar {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Menu {
                        Button(model.listening ? "Stop receiver" : "Start receiver") {
                            model.listening ? model.stop() : model.start()
                        }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                    }
                    .accessibilityLabel("Receiver options")
                }
            }
            .onAppear {
                if !model.listening { model.start() }
            }
            .onChange(of: scenePhase) { phase in
                // Keep the receiver alive across foreground/background cycles;
                // background audio continues via the audio background mode.
                if phase == .active, !model.listening { model.start() }
            }
        }
    }

    // MARK: Hero

    private var connectionHero: some View {
        VStack(alignment: .leading, spacing: Theme.spacingM) {
            HStack(alignment: .center, spacing: Theme.spacingS) {
                PulseDot(color: Theme.statusColor(for: model.state),
                         active: isLive)
                Text(heroTitle)
                    .font(.title2.weight(.semibold))
                    .accessibilityAddTraits(.isHeader)
                Spacer()
                if model.listening, case .streaming = model.state {
                    Button {
                        model.disconnect()
                    } label: {
                        Text("Disconnect")
                            .font(.subheadline.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                }
            }

            switch model.state {
            case .streaming(let name):
                VStack(alignment: .leading, spacing: 4) {
                    Text(name)
                        .font(.headline)
                    Text("\(model.streamFormat)  ·  \(model.stats.outputRoute)")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
                .padding(.top, 2)

                HStack(spacing: Theme.spacingM) {
                    latencyPill
                    qualityPill
                }
                .padding(.top, 4)

            case .waitingForSender:
                Text("Listening for your PC on this Wi-Fi.\nOn the PC, run:\nremote-au send --source loopback --to \(localIPHint):47000")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
                    .padding(.top, 2)

            case .interrupted(let reason):
                Text(reason)
                    .font(.subheadline)
                    .foregroundStyle(.secondary)

            case .idle:
                Text("Receiver stopped.")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
        .accessibilityElement(children: .contain)
    }

    private var isLive: Bool {
        if case .streaming = model.state { return true }
        return false
    }

    private var heroTitle: String {
        switch model.state {
        case .idle: return "Receiver off"
        case .waitingForSender: return "Searching…"
        case .streaming: return "Connected"
        case .interrupted: return "Reconnecting…"
        }
    }

    private var localIPHint: String {
        model.senderAddress.isEmpty ? "<this-iphone-ip>" : model.senderAddress
    }

    private var latencyPill: some View {
        StatPill(icon: "timer",
                 title: "Latency",
                 value: String(format: "%.0f ms", model.stats.softwareLatencyMs))
    }

    private var qualityPill: some View {
        StatPill(icon: "wifi",
                 title: "Network",
                 value: Theme.qualityLabel(for: model.stats.lossPercent,
                                           jitter: model.stats.jitterMs))
    }

    // MARK: Quality presets

    private var qualitySection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Quality")
                .font(.headline)
            Picker("Quality preset", selection: $model.qualityPreset) {
                ForEach(QualityPreset.allCases.filter { $0 != .advanced }) { preset in
                    Text(preset.rawValue).tag(preset)
                }
            }
            .pickerStyle(.segmented)
            .onChange(of: model.qualityPreset) { newValue in
                model.setPreset(newValue)
            }

            if model.qualityPreset == .auto {
                Text(model.stats.presetDescription)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }

            if model.qualityPreset == .robust || model.qualityPreset == .auto {
                Toggle("Manual buffer target (Advanced)", isOn: $showAdvanced)
                    .font(.subheadline)
                if showAdvanced {
                    HStack {
                        Slider(value: $model.manualTargetMs, in: 8...200, step: 1) {
                            Text("Buffer target")
                        }
                        Text("\(Int(model.manualTargetMs)) ms")
                            .monospacedDigit()
                            .frame(width: 64, alignment: .trailing)
                    }
                    .onChange(of: model.manualTargetMs) { v in
                        model.setManualTarget(ms: v)
                    }
                }
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    // MARK: Live stats summary

    private var liveStatsSummary: some View {
        DisclosureGroup {
            StatsView(stats: model.stats)
        } label: {
            HStack {
                Image(systemName: "waveform.path.ecg")
                Text("Live statistics")
                    .font(.headline)
                Spacer()
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
        .accessibilityHint("Detailed network and buffer statistics")
    }

    // MARK: Devices

    private var devicesSection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Your PC")
                .font(.headline)

            if model.discoveredPeers.isEmpty {
                HStack(spacing: Theme.spacingS) {
                    Image(systemName: "desktopcomputer")
                        .font(.title3)
                        .foregroundStyle(.secondary)
                    VStack(alignment: .leading, spacing: 2) {
                        Text("Connect by IP")
                            .font(.subheadline.weight(.medium))
                        Text("Your PC streams to this iPhone automatically once started. Optionally restrict incoming audio to one PC:")
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                }
                TextField("PC IP address, e.g. 192.168.1.10", text: $model.manualIPFilter)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.decimalPad)
                    .autocorrectionDisabled()
            } else {
                ForEach(model.discoveredPeers) { peer in
                    DeviceCard(peer: peer)
                }
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    private var errorFooter: some View {
        Group {
            if let err = model.lastError {
                Label(err, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .padding(Theme.spacingM)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerM))
            }
        }
    }
}

// MARK: - Small building blocks

struct StatPill: View {
    var icon: String
    var title: String
    var value: String

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: icon)
                .foregroundStyle(.secondary)
            VStack(alignment: .leading, spacing: 0) {
                Text(title)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                Text(value)
                    .font(.subheadline.weight(.semibold))
                    .monospacedDigit()
            }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
        .background(Color(.tertiarySystemFill), in: Capsule())
        .accessibilityElement(children: .combine)
        .accessibilityLabel("\(title): \(value)")
    }
}

struct DeviceCard: View {
    var peer: DiscoveredPeer

    var body: some View {
        HStack(spacing: Theme.spacingM) {
            Image(systemName: "desktopcomputer")
                .font(.title2)
                .foregroundStyle(.tint)
                .frame(width: 36)
            VStack(alignment: .leading, spacing: 2) {
                Text(peer.name)
                    .font(.subheadline.weight(.semibold))
                Text(peer.address)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text(peer.paired ? "Paired" : "Nearby")
                .font(.caption.weight(.medium))
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(peer.paired ? Color.green.opacity(0.15) : Color.blue.opacity(0.12),
                            in: Capsule())
        }
        .padding(Theme.spacingM)
        .background(Color(.tertiarySystemFill), in: RoundedRectangle(cornerRadius: Theme.cornerM))
    }
}

extension StatsSnapshot {
    var presetDescription: String {
        "Auto adapts buffering to network health — small on clean Wi-Fi, larger when it degrades."
    }
}
