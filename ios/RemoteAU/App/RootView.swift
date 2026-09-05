import SwiftUI
import UIKit

/// Main screen: connection hero, discovered PCs, quality presets, live stats
/// and an expandable statistics view.
struct RootView: View {
    @EnvironmentObject private var model: ReceiverModel
    @StateObject private var v2 = V2Controller()
    @Environment(\.scenePhase) private var scenePhase
    @State private var showAdvanced = false
    @State private var pairingPeer: DiscoveredPeer?

    // Persisted settings (last-used quality, filters, v2 codec prefs).
    @AppStorage("qualityPreset") private var storedPreset: String = QualityPreset.auto.rawValue
    @AppStorage("manualTargetMs") private var storedTargetMs: Double = 20
    @AppStorage("manualIPFilter") private var storedIPFilter: String = ""
    @AppStorage("v2CodecOpus") private var v2CodecOpus: Bool = false
    @AppStorage("v2FrameMs") private var v2FrameMs: Int = 5
    @AppStorage("v2BitrateKbps") private var v2BitrateKbps: Int = 128
    @AppStorage("v2FEC") private var v2FEC: Bool = false
    @AppStorage("v2DTX") private var v2DTX: Bool = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: Theme.spacingL) {
                    connectionHero
                    if case .streaming = model.state {
                        qualitySection
                        liveStatsSummary
                    }
                    discoveredSection
                    pairedSection
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
                            if model.listening {
                                model.stopAll()
                            } else {
                                model.start()
                                model.startFinder()
                            }
                        }
                        Button(model.finderOn ? "Stop searching for PCs" : "Search for PCs") {
                            if model.finderOn {
                                model.stopFinder()
                            } else {
                                model.startFinder()
                            }
                        }
                    } label: {
                        Image(systemName: "ellipsis.circle")
                    }
                    .accessibilityLabel("Receiver options")
                }
            }
            .sheet(item: $pairingPeer) { peer in
                PairingSheet(peer: peer, v2: v2,
                             v2CodecOpus: $v2CodecOpus,
                             v2FrameMs: $v2FrameMs,
                             v2BitrateKbps: $v2BitrateKbps,
                             v2FEC: $v2FEC,
                             v2DTX: $v2DTX)
            }
            .onAppear {
                v2.setup()
                if !model.listening { model.start() }
                model.startFinder()
                if model.qualityPreset.rawValue != storedPreset {
                    model.qualityPreset = QualityPreset(rawValue: storedPreset) ?? .auto
                }
                if model.manualTargetMs != storedTargetMs {
                    model.manualTargetMs = storedTargetMs
                    model.setManualTarget(ms: storedTargetMs)
                }
                if model.manualIPFilter != storedIPFilter {
                    model.manualIPFilter = storedIPFilter
                }
                V2MediaRouter.shared.handler = { pcm in
                    model.feedExternalPCM(pcm)
                }
            }
            .onChange(of: scenePhase) { phase in
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
                    Text("\(model.streamFormat)  ·  \(model.stats.codec)  ·  \(model.stats.outputRoute)")
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
                Text("Searching for your PC on this Wi-Fi.\nOpen RemoteAU on the PC (tray app or `remote-au serve`) and tap it here, or start streaming to this iPhone directly.")
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
                storedPreset = newValue.rawValue
            }

            if model.qualityPreset == .auto {
                Text(model.stats.presetDescription)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }

            Toggle("Advanced controls", isOn: $showAdvanced)
                .font(.subheadline)

            if showAdvanced {
                if model.qualityPreset == .advanced {
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
                        storedTargetMs = v
                    }
                }

                // v2 codec preferences (used when connecting through the v2
                // engine; harmless when the framework is unavailable).
                VStack(alignment: .leading, spacing: 6) {
                    Text("Codec (v2)")
                        .font(.footnote.weight(.semibold))
                    Toggle("Prefer Opus", isOn: $v2CodecOpus)
                    if v2CodecOpus {
                        Picker("Frame", selection: $v2FrameMs) {
                            Text("2 ms").tag(2)
                            Text("5 ms").tag(5)
                            Text("10 ms").tag(10)
                            Text("20 ms").tag(20)
                        }
                        .pickerStyle(.segmented)
                        HStack {
                            Slider(value: .init(get: { Double(v2BitrateKbps) },
                                                set: { v2BitrateKbps = Int(v) }),
                                   in: 16...256, step: 8) {
                                Text("Bitrate")
                            }
                            Text("\(v2BitrateKbps) kbps")
                                .monospacedDigit()
                                .frame(width: 84, alignment: .trailing)
                        }
                        Toggle("In-band FEC", isOn: $v2FEC)
                        Toggle("DTX (silence compression)", isOn: $v2DTX)
                    }
                    Text("Used when connecting to a PC over the secure v2 engine.")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    // MARK: Discovered PCs

    @ViewBuilder
    private var discoveredSection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Your PC")
                .font(.headline)

            if model.discoveredPeers.isEmpty {
                HStack(spacing: Theme.spacingS) {
                    Image(systemName: "desktopcomputer")
                        .font(.title3)
                        .foregroundStyle(.secondary)
                    VStack(alignment: .leading, spacing: 2) {
                        Text("No PCs found yet")
                            .font(.subheadline.weight(.medium))
                        Text("Start the RemoteAU tray app (or `remote-au serve`) on the PC. You can also restrict incoming audio to one PC by IP:")
                            .font(.footnote)
                            .foregroundStyle(.secondary)
                    }
                }
                TextField("PC IP address, e.g. 192.168.1.10", text: $model.manualIPFilter)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.decimalPad)
                    .autocorrectionDisabled()
                    .onChange(of: model.manualIPFilter) { v in
                        storedIPFilter = v
                    }
            } else {
                ForEach(model.discoveredPeers) { peer in
                    Button {
                        pairingPeer = peer
                    } label: {
                        DeviceCard(peer: peer)
                    }
                    .buttonStyle(.plain)
                    .accessibilityHint("Connect to this PC")
                }
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    // MARK: Paired PCs (v2 trust store)

    @ViewBuilder
    private var pairedSection: some View {
        let peers = v2.peers
        if !peers.isEmpty {
            VStack(alignment: .leading, spacing: Theme.spacingS) {
                Text("Paired PCs")
                    .font(.headline)
                ForEach(peers) { pc in
                    HStack {
                        Image(systemName: "checkmark.shield.fill")
                            .foregroundStyle(.green)
                        Text(pc.name)
                            .font(.subheadline)
                        Spacer()
                        Button(role: .destructive) {
                            v2.forgetPeer(idHex: pc.id)
                        } label: {
                            Text("Forget")
                                .font(.caption.weight(.medium))
                        }
                    }
                }
            }
            .padding(Theme.spacingL)
            .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
        }
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

// MARK: - Pairing sheet

struct PairingSheet: View {
    var peer: DiscoveredPeer
    @ObservedObject var v2: V2Controller
    @Binding var v2CodecOpus: Bool
    @Binding var v2FrameMs: Int
    @Binding var v2BitrateKbps: Int
    @Binding var v2FEC: Bool
    @Binding var v2DTX: Bool
    @Environment(\.dismiss) private var dismiss
    @State private var pin = ""
    @State private var connecting = false

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: Theme.spacingL) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(peer.name)
                        .font(.title3.weight(.semibold))
                    Text("\(peer.address):\(peer.port) · protocol v\(peer.protocolVersion)")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }

                if peer.protocolVersion >= 2 {
                    Text("Enter the pairing code shown on the PC to connect securely.")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                    TextField("6-digit code", text: $pin)
                        .textFieldStyle(.roundedBorder)
                        .keyboardType(.numberPad)
                        .font(.title2.monospacedDigit())
                        .multilineTextAlignment(.center)

                    Button {
                        connecting = true
                        let adv = V2Controller.AdvancedSettings(
                            codecOpus: v2CodecOpus,
                            frameMs: v2FrameMs,
                            bitrateKbps: v2BitrateKbps,
                            fec: v2FEC,
                            dtx: v2DTX
                        )
                        v2.connect(host: "\(peer.address):\(peer.port)",
                                   name: Self.deviceName(),
                                   advanced: adv) { code in
                            // Prompt (defensive fallback if the sheet input
                            // wasn't the path used).
                            return code.isEmpty ? pin : code
                        }
                    } label: {
                        if connecting {
                            ProgressView()
                        } else {
                            Text("Connect")
                                .frame(maxWidth: .infinity)
                        }
                    }
                    .buttonStyle(.borderedProminent)
                    .disabled(pin.count < 4 || connecting)

                    if !v2.lastError.isEmpty {
                        Label(v2.lastError, systemImage: "exclamationmark.triangle")
                            .font(.footnote)
                            .foregroundStyle(.orange)
                    }
                } else {
                    Text("This PC speaks the legacy protocol. On the PC, start streaming and it will find this iPhone automatically:")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                    Text("remote-au send --source loopback --to \(peer.address):47000")
                        .font(.footnote.monospaced())
                        .textSelection(.enabled)
                        .padding(8)
                        .background(Color(.tertiarySystemFill), in: RoundedRectangle(cornerRadius: 8))
                }

                Spacer()
            }
            .padding(Theme.spacingL)
            .navigationTitle("Connect")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Close") {
                        v2.stop()
                        dismiss()
                    }
                }
            }
        }
    }

    static func deviceName() -> String {
        #if targetEnvironment(simulator)
        return "iOS Simulator"
        #else
        return UIDevice.current.name
        #endif
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
                Text("\(peer.address):\(peer.port)")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
            Spacer()
            Text(peer.protocolVersion >= 2 ? "Tap to pair" : "Legacy")
                .font(.caption.weight(.medium))
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(peer.protocolVersion >= 2 ? Color.green.opacity(0.15) : Color.blue.opacity(0.12),
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
