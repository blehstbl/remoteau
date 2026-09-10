import SwiftUI
import UIKit

/// What the pairing sheet connects to: a discovered/manual host, or the
/// secure relay path (relay `host:port` + 32-hex host device id).
private enum PairingTarget: Identifiable {
    case peer(DiscoveredPeer)
    case relay(addr: String, hostID: String)

    var id: String {
        switch self {
        case .peer(let peer): return "peer.\(peer.id)"
        case .relay(let addr, let hostID): return "relay.\(addr).\(hostID)"
        }
    }
}

/// Main screen: connection hero, discovered PCs, quality presets, live stats
/// and an expandable statistics view.
struct RootView: View {
    @EnvironmentObject private var model: ReceiverModel
    @StateObject private var v2 = V2Controller()
    @Environment(\.scenePhase) private var scenePhase
    @State private var showAdvanced = false
    @State private var pairingTarget: PairingTarget?

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
    // Secure relay (WAN) target.
    @AppStorage("v2RelayAddr") private var v2RelayAddr: String = ""
    @AppStorage("v2HostDeviceID") private var v2HostDeviceID: String = ""
    // Windows capture source (v2), jitter bounds, named profile.
    @AppStorage("v2SourceKind") private var v2SourceKind: Int = 0
    @AppStorage("v2SourceName") private var v2SourceName: String = ""
    @AppStorage("jitterMinMs") private var jitterMinMs: Double = 8
    @AppStorage("jitterMaxMs") private var jitterMaxMs: Double = 120
    @AppStorage("profile") private var storedProfile: String = ""
    @AppStorage("remoteMode") private var remoteMode: Bool = false
    @State private var manualError: String? = nil
    @State private var lastRecordingPath: String = ""

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: Theme.spacingL) {
                    connectionHero
                    profileSection
                    if isLive {
                        qualitySection
                    }
                    if isLive || v2.stats.connected {
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
            .sheet(item: $pairingTarget) { target in
                switch target {
                case .peer(let peer):
                    PairingSheet(peer: peer, v2: v2,
                                 v2CodecOpus: $v2CodecOpus,
                                 v2FrameMs: $v2FrameMs,
                                 v2BitrateKbps: $v2BitrateKbps,
                                 v2FEC: $v2FEC,
                                 v2DTX: $v2DTX)
                case .relay(let addr, let hostID):
                    PairingSheet(peer: DiscoveredPeer(name: "Relay", address: addr,
                                                      port: 0, protocolVersion: 2,
                                                      paired: false),
                                 v2: v2,
                                 v2CodecOpus: $v2CodecOpus,
                                 v2FrameMs: $v2FrameMs,
                                 v2BitrateKbps: $v2BitrateKbps,
                                 v2FEC: $v2FEC,
                                 v2DTX: $v2DTX,
                                 relay: (addr: addr, hostID: hostID))
                }
            }
            .onAppear {
                v2.setup()
                if !model.listening { model.start() }
                model.startFinder()
                // Re-apply the persisted profile, or the individual settings
                // when no profile has been chosen yet.
                if let profile = Profile(rawValue: storedProfile) {
                    applyProfile(profile)
                } else {
                    if model.qualityPreset.rawValue != storedPreset {
                        let preset = QualityPreset(rawValue: storedPreset) ?? .auto
                        model.qualityPreset = preset
                        model.setPreset(preset)
                    }
                    if model.manualTargetMs != storedTargetMs {
                        model.manualTargetMs = storedTargetMs
                        model.setManualTarget(ms: storedTargetMs)
                    }
                }
                if model.manualIPFilter != storedIPFilter {
                    model.manualIPFilter = storedIPFilter
                }
                model.setJitterBounds(minMs: jitterMinMs, maxMs: jitterMaxMs)
                V2MediaRouter.shared.handler = { pcm in
                    model.feedExternalPCM(pcm)
                }
                // Let the recorder pick up the v2 stream's real format.
                let v2ref = v2
                model.v2FormatProvider = {
                    (v2ref.stats.sampleRate, v2ref.stats.channels)
                }
            }
            .onChange(of: model.recording) { isRecording in
                // Surface the saved path when recording auto-stops because the
                // stream ended (the toggle binding only covers manual stops).
                if !isRecording, let url = model.lastRecordingURL {
                    lastRecordingPath = url.path
                }
            }
            .onChange(of: v2.state) { newState in
                // Sync the host quality mode once the secure session is live.
                if newState == "streaming" {
                    v2.setQualityMode(model.qualityPreset.v2Mode)
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
        if v2.stats.connected {
            return StatPill(icon: "timer",
                            title: "RTT",
                            value: String(format: "%.0f ms", v2.stats.rttMs))
        }
        return StatPill(icon: "timer",
                        title: "Latency",
                        value: String(format: "%.0f ms", model.stats.softwareLatencyMs))
    }

    private var qualityPill: some View {
        let loss = v2.stats.connected ? v2.stats.lossPct : model.stats.lossPercent
        let jitter = v2.stats.connected ? v2.stats.jitterMs : model.stats.jitterMs
        return StatPill(icon: "wifi",
                        title: "Network",
                        value: Theme.qualityLabel(for: loss, jitter: jitter))
    }

    // MARK: Named profiles

    private var profileSection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Profile")
                .font(.headline)
            Picker("Profile", selection: profileBinding) {
                ForEach(Profile.allCases) { profile in
                    Text(profile.rawValue).tag(profile)
                }
            }
            .pickerStyle(.menu)
            .frame(maxWidth: .infinity, alignment: .leading)

            Text(selectedProfile.detail)
                .font(.footnote)
                .foregroundStyle(.secondary)

            if remoteMode {
                Text("Remote / Efficient tunes codec and buffering for relay-friendly streaming. To use a relay, enter its address and the PC's host device ID under Connect manually.")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    private var selectedProfile: Profile {
        Profile(rawValue: storedProfile) ?? .homeLossless
    }

    private var profileBinding: Binding<Profile> {
        Binding(
            get: { selectedProfile },
            set: { applyProfile($0) }
        )
    }

    /// Applies every knob a named profile controls: quality preset, v2 codec
    /// preferences, jitter target and the remote-mode hint.
    private func applyProfile(_ profile: Profile) {
        storedProfile = profile.rawValue

        model.qualityPreset = profile.qualityPreset
        model.setPreset(profile.qualityPreset)
        storedPreset = profile.qualityPreset.rawValue

        v2CodecOpus = profile.preferOpus
        v2FEC = profile.useFEC
        v2DTX = profile.useDTX
        v2BitrateKbps = profile.bitrateKbps

        model.manualTargetMs = profile.jitterTargetMs
        storedTargetMs = profile.jitterTargetMs
        model.setManualTarget(ms: profile.jitterTargetMs)

        remoteMode = profile.remoteMode

        if v2Streaming {
            v2.setQualityMode(profile.qualityPreset.v2Mode)
        }
    }

    // MARK: Quality presets

    private var qualitySection: some View {
        VStack(alignment: .leading, spacing: Theme.spacingS) {
            Text("Quality")
                .font(.headline)
            Picker("Quality preset", selection: $model.qualityPreset) {
                ForEach(QualityPreset.allCases) { preset in
                    Text(preset.rawValue).tag(preset)
                }
            }
            .pickerStyle(.segmented)
            .onChange(of: model.qualityPreset) { newValue in
                model.setPreset(newValue)
                storedPreset = newValue.rawValue
                if v2Streaming {
                    v2.setQualityMode(newValue.v2Mode)
                }
            }

            if model.qualityPreset == .auto {
                Text(model.stats.presetDescription)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
            }

            Toggle("Advanced controls", isOn: $showAdvanced)
                .font(.subheadline)

            if showAdvanced || model.qualityPreset == .advanced {
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

                captureSourceControls
                jitterBoundsControls
                recordingControls
            }
        }
        .padding(Theme.spacingL)
        .background(.background.secondary, in: RoundedRectangle(cornerRadius: Theme.cornerL))
    }

    // MARK: Capture source (Windows, v2)

    private var captureSourceControls: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Capture source (Windows, v2)")
                .font(.footnote.weight(.semibold))
            Picker("Capture source", selection: $v2SourceKind) {
                Text("System audio (default)").tag(0)
                Text("Test tone (diagnostics)").tag(2)
                Text("Custom device…").tag(1)
                Text("Per-app…").tag(3)
            }
            .pickerStyle(.menu)
            .frame(maxWidth: .infinity, alignment: .leading)
            .disabled(!v2Active)

            if v2SourceKind == 1 {
                TextField("Render device name", text: $v2SourceName)
                    .textFieldStyle(.roundedBorder)
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                    .disabled(!v2Active)
            } else if v2SourceKind == 3 {
                TextField("PIDs, e.g. p:1234 or x:5678", text: $v2SourceName)
                    .textFieldStyle(.roundedBorder)
                    .keyboardType(.numbersAndPunctuation)
                    .autocorrectionDisabled()
                    .textInputAutocapitalization(.never)
                    .disabled(!v2Active)
            }

            Button {
                applySource()
            } label: {
                Text("Apply capture source")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
            .disabled(!v2Active)

            Text(v2Active
                 ? "The PC validates the source and rejects invalid devices or PIDs."
                 : "Connect to a PC first.")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }

    private func applySource() {
        let name: String
        switch v2SourceKind {
        case 1, 3:
            name = v2SourceName.trimmingCharacters(in: .whitespaces)
        default:
            name = ""
        }
        v2.setSource(kind: v2SourceKind, name: name)
    }

    // MARK: Advanced jitter bounds

    private var jitterBoundsControls: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Jitter bounds")
                .font(.footnote.weight(.semibold))
            HStack {
                Slider(value: $jitterMinMs, in: 4...120, step: 1) {
                    Text("Minimum")
                }
                Text("\(Int(jitterMinMs)) ms")
                    .monospacedDigit()
                    .frame(width: 64, alignment: .trailing)
            }
            .onChange(of: jitterMinMs) { _ in
                model.setJitterBounds(minMs: jitterMinMs, maxMs: jitterMaxMs)
            }
            HStack {
                Slider(value: $jitterMaxMs, in: 20...400, step: 1) {
                    Text("Maximum")
                }
                Text("\(Int(jitterMaxMs)) ms")
                    .monospacedDigit()
                    .frame(width: 64, alignment: .trailing)
            }
            .onChange(of: jitterMaxMs) { _ in
                model.setJitterBounds(minMs: jitterMinMs, maxMs: jitterMaxMs)
            }
            Text("Auto never buffers below Min or above Max; fixed presets and the manual target stay in range too.")
                .font(.caption2)
                .foregroundStyle(.secondary)
        }
    }

    // MARK: Session recording

    private var recordingControls: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("Session recording")
                .font(.footnote.weight(.semibold))
            Toggle("Record session", isOn: recordingBinding)
            if lastRecordingPath.isEmpty {
                Text("Saves the received audio as a 16-bit WAV in Documents/Recordings.")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
            } else {
                Text("Last saved: \(lastRecordingPath)")
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
                    .truncationMode(.middle)
            }
        }
    }

    private var recordingBinding: Binding<Bool> {
        Binding(
            get: { model.recording },
            set: { on in
                guard on != model.recording else { return }
                if let url = model.toggleRecording() {
                    lastRecordingPath = url.path
                }
            }
        )
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
                        pairingTarget = .peer(peer)
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

            Divider()
                .padding(.vertical, 2)

            Text("Relay (WAN)")
                .font(.subheadline.weight(.semibold))
            TextField("Relay address (host:port)", text: $v2RelayAddr)
                .textFieldStyle(.roundedBorder)
                .keyboardType(.numbersAndPunctuation)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
            TextField("Host device ID (32 hex chars)", text: $v2HostDeviceID)
                .textFieldStyle(.roundedBorder)
                .keyboardType(.asciiCapable)
                .autocorrectionDisabled()
                .textInputAutocapitalization(.never)
            Button {
                openRelayPairing()
            } label: {
                Text("Connect via relay")
                    .frame(maxWidth: .infinity)
            }
            .buttonStyle(.bordered)
            .disabled(!relayConnectReady)
            Text("For PCs reachable only through `remote-au relay`. Scan the PC's QR code to fill the host device ID.")
                .font(.caption2)
                .foregroundStyle(.secondary)
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
        pairingTarget = .peer(DiscoveredPeer(name: host, address: host, port: port,
                                             protocolVersion: 2, paired: false))
    }

    /// True when the relay address is non-empty and the device id is 32 hex.
    private var relayConnectReady: Bool {
        !v2RelayAddr.trimmingCharacters(in: .whitespaces).isEmpty && hostDeviceIDIsValid
    }

    private var hostDeviceIDIsValid: Bool {
        let s = v2HostDeviceID.trimmingCharacters(in: .whitespaces).lowercased()
        return s.count == 32 && s.allSatisfy { $0.isHexDigit }
    }

    private func openRelayPairing() {
        let addr = v2RelayAddr.trimmingCharacters(in: .whitespaces)
        let hostID = v2HostDeviceID.trimmingCharacters(in: .whitespaces).lowercased()
        guard !addr.isEmpty, hostDeviceIDIsValid else {
            manualError = "Enter a relay address and a 32-character hex host device ID."
            return
        }
        manualError = nil
        pairingTarget = .relay(addr: addr, hostID: hostID)
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
            if v2.stats.connected {
                V2StatsView(stats: v2.stats)
                Divider()
            }
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
    /// Non-nil when connecting through the secure relay; `hostID` may be
    /// overridden by a scanned QR link that carries an `id`.
    var relay: (addr: String, hostID: String)?
    @ObservedObject var v2: V2Controller
    @Binding var v2CodecOpus: Bool
    @Binding var v2FrameMs: Int
    @Binding var v2BitrateKbps: Int
    @Binding var v2FEC: Bool
    @Binding var v2DTX: Bool
    @Environment(\.dismiss) private var dismiss
    // Shared with RootView so scanning the PC's QR prefills the host id.
    @AppStorage("v2HostDeviceID") private var storedHostDeviceID: String = ""
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
         preScanned: PairingLink? = nil,
         relay: (addr: String, hostID: String)? = nil) {
        self.peer = peer
        self.relay = relay
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
                    Text(subtitle)
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
        // A QR that carries a device id is usable as the relay host id too,
        // so mirror it into the shared field the manual card reads.
        if let id = link.id {
            storedHostDeviceID = id
        }
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
        if let relay {
            // Prefer a scanned id (it is the authoritative peer id) over the
            // manually typed one, then connect through the relay.
            let hostID = scannedLink?.id ?? relay.hostID
            v2.connectRelay(relayAddr: relay.addr,
                            hostDeviceID: hostID,
                            name: Self.deviceName(),
                            advanced: adv) {
                return pin
            }
        } else {
            v2.connect(host: targetHost,
                       name: Self.deviceName(),
                       advanced: adv) {
                return pin
            }
        }
    }

    private var subtitle: String {
        if let relay {
            return "\(relay.addr) · secure relay"
        }
        return "\(targetHost) · protocol v\(peer.protocolVersion)"
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

// MARK: - Named profiles

/// One-tap configurations bundling a quality preset, v2 codec preferences and
/// a jitter target. Selected profiles are persisted and re-applied at launch.
enum Profile: String, CaseIterable, Identifiable {
    case homeLossless = "Home / Lossless"
    case gamingLowest = "Gaming / Lowest Latency"
    case weakWiFi = "Weak Wi-Fi / Robust"
    case remote = "Remote / Efficient"

    var id: String { rawValue }

    var qualityPreset: QualityPreset {
        switch self {
        case .homeLossless: return .lossless
        case .gamingLowest: return .lowestLatency
        case .weakWiFi, .remote: return .robust
        }
    }

    /// Manual jitter target in ms (applied through the advanced/manual path).
    var jitterTargetMs: Double {
        switch self {
        case .homeLossless: return 25
        case .gamingLowest: return 10
        case .weakWiFi: return 60
        case .remote: return 90
        }
    }

    var preferOpus: Bool {
        switch self {
        case .weakWiFi, .remote: return true
        case .homeLossless, .gamingLowest: return false
        }
    }

    var useFEC: Bool { preferOpus }
    var useDTX: Bool { preferOpus }

    var bitrateKbps: Int {
        switch self {
        case .remote: return 64
        case .weakWiFi: return 96
        case .homeLossless, .gamingLowest: return 128
        }
    }

    var remoteMode: Bool { self == .remote }

    var detail: String {
        switch self {
        case .homeLossless:
            return "PCM with a slightly safer buffer for home Wi-Fi."
        case .gamingLowest:
            return "PCM with the smallest adaptive buffer for the lowest latency."
        case .weakWiFi:
            return "Opus + FEC + DTX at 96 kbps; tolerates poor Wi-Fi."
        case .remote:
            return "Opus + FEC + DTX at 64 kbps; relay-friendly and data-efficient."
        }
    }
}

extension QualityPreset {
    /// Maps to the Go `RemoteAUSetQualityMode` argument.
    var v2Mode: Int {
        switch self {
        case .auto: return 0
        case .lowestLatency: return 1
        case .lossless: return 2
        case .robust: return 3
        case .advanced: return 4
        }
    }
}
