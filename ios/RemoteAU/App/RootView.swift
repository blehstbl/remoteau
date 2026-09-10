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
    @AppStorage("manualV2Host") private var manualV2Host: String = ""
    @AppStorage("manualV2Port") private var manualV2Port: String = "47010"
    @State private var manualError: String? = nil

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: Theme.spacingL) {
                    connectionHero
                    if isLive {
                        qualitySection
                        liveStatsSummary
                    }
                    discoveredSection
                    manualConnectSection
                    legacyFilterSection
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
                PulseDot(color: heroColor, active: isLive)
                Text(heroTitle)
                    .font(.title2.weight(.semibold))
                    .accessibilityAddTraits(.isHeader)
                Spacer()
                if v2Streaming {
                    Button {
                        v2.stop()
                    } label: {
                        Text("Disconnect")
                            .font(.subheadline.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                } else if model.listening, case .streaming = model.state {
                    Button {
                        model.disconnect()
                    } label: {
                        Text("Disconnect")
                            .font(.subheadline.weight(.medium))
                    }
                    .buttonStyle(.bordered)
                }
            }

            if v2Active {
                HStack(spacing: 6) {
                    PulseDot(color: heroColor, active: true)
                    Text("Secure session: \(v2.state)")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
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
                if !v2Active {
                    Text("Searching for your PC on this Wi-Fi.\nOpen RemoteAU on the PC (tray app or `remote-au serve`) and tap it here, or start streaming to this iPhone directly.")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                        .padding(.top, 2)
                }

            case .interrupted(let reason):
                if !v2Active {
                    Text(reason)
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }

            case .idle:
                if !v2Active {
                    Text("Receiver stopped.")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
        .accessibilityElement(children: .contain)
    }

    private var v2Active: Bool {
        !v2.state.isEmpty && v2.state != "idle" && v2.state != "unavailable"
    }

    private var v2Streaming: Bool {
        v2.state == "streaming"
    }

    private var isLive: Bool {
        if v2Streaming { return true }
        if case .streaming = model.state { return true }
        return false
    }

    private var heroColor: Color {
        if v2Active {
            return v2Streaming ? .green : .orange
        }
        return Theme.statusColor(for: model.state)
    }

    private var heroTitle: String {
        if v2Streaming { return "Connected" }
        if v2Active {
            switch v2.state {
            case "connecting", "pairing": return "Connecting…"
            case "buffering": return "Buffering…"
            case "reconnecting": return "Reconnecting…"
            case "error", "failed": return "Connection problem"
            default: break
            }
        }
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
                if !model.listening {
                    VStack(alignment: .leading, spacing: Theme.spacingM) {
                        HStack(spacing: Theme.spacingS) {
                            Image(systemName: "stop.circle")
                                .font(.title3)
                                .foregroundStyle(.secondary)
                            VStack(alignment: .leading, spacing: 2) {
                                Text("Receiver stopped")
                                    .font(.subheadline.weight(.medium))
                                Text("Start the receiver to look for PCs on this Wi-Fi.")
                                    .font(.footnote)
                                    .foregroundStyle(.secondary)
                            }
                        }
                        Button {
                            model.start()
                            model.startFinder()
                        } label: {
                            Text("Start receiver")
                                .frame(maxWidth: .infinity)
                        }
                        .buttonStyle(.borderedProminent)
                    }
                } else {
                    HStack(spacing: Theme.spacingS) {
                        Image(systemName: "desktopcomputer")
                            .font(.title3)
                            .foregroundStyle(.secondary)
                        VStack(alignment: .leading, spacing: 2) {
                            Text("No PCs found yet")
                                .font(.subheadline.weight(.medium))
                            Text("Start the RemoteAU tray app (or `remote-au serve`) on the PC. If it still doesn't appear, use Connect manually below.")
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                        }
                    }
                }
            } else {
                ForEach(sortedPeers) { peer in
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

    /// Secure v2 peers first, then by name.
    private var sortedPeers: [DiscoveredPeer] {
        model.discoveredPeers.sorted { lhs, rhs in
            let lhsV2 = lhs.protocolVersion >= 2
            let rhsV2 = rhs.protocolVersion >= 2
            if lhsV2 != rhsV2 { return lhsV2 }
            return lhs.name.localizedCaseInsensitiveCompare(rhs.name) == .orderedAscending
        }
    }

    // MARK: Manual v2 connect

    private var manualConnectSection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Connect manually (secure v2)")
                .font(.headline)
            Text("Use this if the PC isn't discoverable. Pair with a code or scan its QR code.")
                .font(.footnote)
                .foregroundStyle(.secondary)
            HStack(spacing: Theme.spacingS) {
                TextField("Host, e.g. 192.168.1.10", text: $manualV2Host)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.decimalPad)
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                TextField("Port", text: $manualV2Port)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.numberPad)
                    .frame(width: 88)
            }
            if let manualError {
                Label(manualError, systemImage: "exclamationmark.triangle")
                    .font(.footnote)
                    .foregroundStyle(.orange)
            }
            Button {
                connectManual()
            } label: {
                Text("Connect")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    private func connectManual() {
        let host = manualV2Host.trimmingCharacters(in: .whitespaces)
        guard !host.isEmpty else {
            manualError = "Enter the PC's IP address or hostname."
            return
        }
        guard let port = Int(manualV2Port), (1...65535).contains(port) else {
            manualError = "Enter a valid port (1-65535)."
            return
        }
        manualError = nil
        pairingPeer = DiscoveredPeer(name: host, address: host, port: port,
                                     protocolVersion: 2, paired: false)
    }

    // MARK: Legacy v1 filter

    private var legacyFilterSection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Legacy (v1) filter")
                .font(.headline)
            TextField("PC IP address, e.g. 192.168.1.10", text: $model.manualIPFilter)
                .textFieldStyle(.roundedBorder)
                .keyboardType(.decimalPad)
                .autocorrectionDisabled()
                .onChange(of: model.manualIPFilter) { v in
                    storedIPFilter = v
                }
            Text("Legacy v1 PCs stream TO this iPhone; this field optionally restricts which PC is accepted. Secure v2 hosts connect from this screen instead.")
                .font(.footnote)
                .foregroundStyle(.secondary)
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
        VStack(spacing: Theme.spacingS) {
            if let err = model.lastError {
                Label(err, systemImage: "exclamationmark.triangle.fill")
                    .font(.footnote)
                    .foregroundStyle(.orange)
                    .padding(Theme.spacingM)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerM))
            }
            if !v2.lastError.isEmpty {
                Label(v2.lastError, systemImage: "exclamationmark.triangle.fill")
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
    @State private var showScanner = false
    @State private var linkError: String? = nil
    @State private var scannedLink: PairingLink? = nil

    /// `preScanned` lets callers seed the sheet from an already-parsed link
    /// (e.g. a Universal Link or a manual entry). The discovered-peer path
    /// initializes with it nil.
    init(peer: DiscoveredPeer,
         v2: V2Controller,
         v2CodecOpus: Binding<Bool>,
         v2FrameMs: Binding<Int>,
         v2BitrateKbps: Binding<Int>,
         v2FEC: Binding<Bool>,
         v2DTX: Binding<Bool>,
         preScanned: PairingLink? = nil) {
        self.peer = peer
        self._v2 = ObservedObject(wrappedValue: v2)
        self._v2CodecOpus = v2CodecOpus
        self._v2FrameMs = v2FrameMs
        self._v2BitrateKbps = v2BitrateKbps
        self._v2FEC = v2FEC
        self._v2DTX = v2DTX
        self._scannedLink = State(initialValue: preScanned)
        self._pin = State(initialValue: preScanned?.code ?? "")
    }

    var body: some View {
        NavigationStack {
            VStack(alignment: .leading, spacing: Theme.spacingL) {
                VStack(alignment: .leading, spacing: 4) {
                    Text(peer.name)
                        .font(.title3.weight(.semibold))
                    Text("\(targetHost) · protocol v\(peer.protocolVersion)")
                        .font(.subheadline)
                        .foregroundStyle(.secondary)
                }

                if peer.protocolVersion >= 2 {
                    v2ConnectControls
                } else {
                    legacyGuidance
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
            .sheet(isPresented: $showScanner) {
                scannerSheet
            }
        }
    }

    // MARK: v2 branch

    @ViewBuilder
    private var v2ConnectControls: some View {
        if connecting {
            VStack(spacing: Theme.spacingS) {
                ProgressView()
                Text("Pairing…")
                    .font(.subheadline)
                    .foregroundStyle(.secondary)
            }
            .frame(maxWidth: .infinity)
            .padding(.vertical, Theme.spacingL)
        } else {
            Text("Enter the pairing code shown on the PC, or scan its QR code to connect securely.")
                .font(.subheadline)
                .foregroundStyle(.secondary)
            TextField("Pairing code", text: $pin)
                .textFieldStyle(.roundedBorder)
                .keyboardType(.numberPad)
                .font(.title2.monospacedDigit())
                .multilineTextAlignment(.center)
                .accessibilityLabel("Pairing code")

            Button {
                connect()
            } label: {
                Text("Connect")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.borderedProminent)
            .disabled(pin.count < 4)

            Button {
                linkError = nil
                showScanner = true
            } label: {
                Label("Scan QR Code", systemImage: "qrcode.viewfinder")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
            .accessibilityLabel("Scan pairing QR code")
        }

        if let linkError {
            Label(linkError, systemImage: "exclamationmark.triangle")
                .font(.footnote)
                .foregroundStyle(.orange)
        }
        if !v2.lastError.isEmpty {
            Label(v2.lastError, systemImage: "exclamationmark.triangle")
                .font(.footnote)
                .foregroundStyle(.orange)
        }
    }

    private var legacyGuidance: some View {
        Group {
            Text("This PC speaks the legacy protocol. On the PC, start streaming and it will find this iPhone automatically:")
                .font(.subheadline)
                .foregroundStyle(.secondary)
            Text("remote-au send --source loopback --to \(peer.address):47000")
                .font(.footnote.monospaced())
                .textSelection(.enabled)
                .padding(8)
                .background(Color(.tertiarySystemFill), in: RoundedRectangle(cornerRadius: 8))
        }
    }

    private var scannerSheet: some View {
        NavigationStack {
            QRScannerView { raw in
                handleScan(raw)
            }
            .ignoresSafeArea()
            .navigationTitle("Scan QR code")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Cancel") {
                        showScanner = false
                    }
                }
            }
        }
    }

    // MARK: Actions

    private func handleScan(_ raw: String) {
        showScanner = false
        guard let link = PairingLink.parse(raw) else {
            linkError = "Not a RemoteAU QR code"
            return
        }
        linkError = nil
        scannedLink = link
        pin = link.code
        UIAccessibility.post(notification: .announcement,
                             argument: "Pairing code scanned. Connecting.")
        connect()
    }

    private func connect() {
        connecting = true
        let adv = V2Controller.AdvancedSettings(
            codecOpus: v2CodecOpus,
            frameMs: v2FrameMs,
            bitrateKbps: v2BitrateKbps,
            fec: v2FEC,
            dtx: v2DTX
        )
        // `pinPrompt` is `() async -> String`; the code (typed or scanned)
        // is already seeded into `pin`, so this resolves immediately.
        v2.connect(host: targetHost,
                   name: Self.deviceName(),
                   advanced: adv) {
            return pin
        }
    }

    private var targetHost: String {
        if let link = scannedLink {
            return "\(link.host):\(link.port)"
        }
        return "\(peer.address):\(peer.port)"
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
            Text(peer.protocolVersion >= 2 ? "Tap to pair" : "Legacy / unencrypted")
                .font(.caption.weight(.medium))
                .foregroundStyle(peer.protocolVersion >= 2 ? Color.green : Color.secondary)
                .padding(.horizontal, 8)
                .padding(.vertical, 4)
                .background(peer.protocolVersion >= 2 ? Color.green.opacity(0.15) : Color.gray.opacity(0.18),
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
