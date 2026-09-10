import Foundation
import SwiftUI

/// Decoded v2 engine statistics (JSON emitted by `RemoteAULatestStats` /
/// the stats sink). All fields default so a blank value always compiles.
struct V2Stats: Equatable {
    var state: String = ""
    var connected: Bool = false
    var rttMs: Double = 0
    var lossPct: Double = 0
    var latePct: Double = 0
    var jitterMs: Double = 0
    var bufferMs: Double = 0
    var packetsSeen: Double = 0
    var lossPackets: Double = 0
    var latePackets: Double = 0
    var reorderedPackets: Double = 0
    var concealedFrames: Double = 0
    var maxBurst: Double = 0
    /// Stream format reported by the engine (used to size the WAV recorder).
    var sampleRate: Int = 0
    var channels: Int = 0
}

/// Swift bridge to the Go v2 engine (RemoteAU.xcframework via gomobile).
/// Everything here compiles to no-ops when the framework is not linked, so
/// the v1-only build stays intact.
#if canImport(RemoteAU)
import RemoteAU
import Security

/// Bridges the Go engine callbacks into Swift.
final class GoMediaSink: NSObject, RemoteAUMediaSink {
    let onPCM: ([UInt8]) -> Void

    init(onPCM: @escaping ([UInt8]) -> Void) {
        self.onPCM = onPCM
    }

    func onMedia(_ pcm: Data?) {
        guard let pcm else { return }
        onPCM([UInt8](pcm))
    }
}

final class GoStateSink: NSObject, RemoteAUStateSink {
    let onState: (String) -> Void

    init(onState: @escaping (String) -> Void) {
        self.onState = onState
    }

    func onState(_ jsonState: String?) {
        guard let jsonState else { return }
        onState(jsonState)
    }
}

final class GoStatsSink: NSObject, RemoteAUStatsSink {
    let onStats: (String) -> Void

    init(onStats: @escaping (String) -> Void) {
        self.onStats = onStats
    }

    // gomobile imports the Go `string` parameter as an optional, exactly like
    // GoStateSink.onState(_:) and GoMediaSink.onMedia(_:) above.
    func onStats(_ json: String?) {
        guard let json else { return }
        onStats(json)
    }
}

final class GoPINSource: NSObject, RemoteAURequestSource {
    let get: () -> String

    init(get: @escaping () -> String) {
        self.get = get
    }

    func getPIN() -> String {
        get()
    }
}

/// Keychain-backed trust storage: implements the Go `StoreSource` interface
/// (bound as the RemoteAUStoreSource protocol) so the engine keeps its
/// identity key and paired-peer records in the iOS Keychain instead of a
/// file in the app container.
///
/// Binding shapes assumed (consistent with GoMediaSink/GoPINSource above and
/// the Error?-returning RemoteAU* calls): Go `[]byte` bridges to `Data?`,
/// Go `error` bridges to an `Error?` return (gobind protocol methods return
/// the error rather than throwing), and Go `GetX` becomes `getX`.
final class KeychainTrust: NSObject, RemoteAUStoreSource {
    private static let service = "dev.remoteau.trust"
    private static let identityAccount = "identity"
    private static let peersAccount = "peers"

    /// PKCS#8 identity blob, or nil when none is stored. Read errors also
    /// surface as nil (the Go interface has no error channel for reads); the
    /// engine then provisions a fresh identity.
    func getIdentity() -> Data? {
        KeychainTrust.read(account: KeychainTrust.identityAccount)
    }

    /// JSON array of paired peers, or nil when none is stored.
    func getPeers() -> Data? {
        KeychainTrust.read(account: KeychainTrust.peersAccount)
    }

    /// Upserts the identity blob. Go always passes a marshalled key; nil is
    /// treated as a no-op.
    func putIdentity(_ pkcs8: Data?) -> Error? {
        guard let pkcs8 else { return nil }
        return KeychainTrust.upsert(account: KeychainTrust.identityAccount, data: pkcs8)
    }

    /// Upserts the peers JSON blob (Go always sends a valid JSON array,
    /// including "[]" once the last peer is forgotten).
    func putPeers(_ json: Data?) -> Error? {
        guard let json else { return nil }
        return KeychainTrust.upsert(account: KeychainTrust.peersAccount, data: json)
    }

    /// One-time migration of the legacy file-based trust store
    /// (<dir>/RemoteAU/trust.json) into the Keychain. Runs only while the
    /// Keychain has no identity, so it can never overwrite newer Keychain
    /// data. Only trust.json is touched — never anything else in the
    /// directory — and the file is removed only after the writes succeeded
    /// (the Go-side migration retries it on the next launch otherwise).
    func migrateLegacyFile(dir: URL) {
        guard getIdentity() == nil else { return }
        let legacyURL = dir.appendingPathComponent("RemoteAU", isDirectory: true)
            .appendingPathComponent("trust.json")
        guard let raw = try? Data(contentsOf: legacyURL),
              let state = try? JSONDecoder().decode(LegacyTrustFile.self, from: raw) else {
            return
        }
        var migrated = true
        if let pkcs8 = state.identity_pkcs8 {
            migrated = putIdentity(pkcs8) == nil
        }
        if migrated, let peers = state.peers,
           let blob = try? JSONEncoder().encode(peers) {
            migrated = putPeers(blob) == nil
        }
        if migrated {
            try? FileManager.default.removeItem(at: legacyURL)
        }
    }

    // MARK: - Keychain plumbing

    /// Mirror of the Go fileState / pairing.PeerRecord wire format (property
    /// names line up with the Go JSON tags so the engine unmarshals the
    /// re-encoded peers array cleanly).
    private struct LegacyTrustFile: Codable {
        struct LegacyPeer: Codable {
            var id: Data?
            var name: String?
            var pairing_secret: Data?
            var fingerprint: String?
            var paired_at: Int64?
        }
        var identity_pkcs8: Data?
        var peers: [LegacyPeer]?
    }

    private static func baseQuery(account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    private static func read(account: String) -> Data? {
        var query = baseQuery(account: account)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        guard status == errSecSuccess, let data = item as? Data else { return nil }
        return data
    }

    private static func upsert(account: String, data: Data) -> Error? {
        let changes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let updateStatus = SecItemUpdate(baseQuery(account: account) as CFDictionary,
                                         changes as CFDictionary)
        if updateStatus == errSecSuccess {
            return nil
        }
        if updateStatus != errSecItemNotFound {
            return statusError(updateStatus)
        }
        var add = baseQuery(account: account)
        add[kSecValueData as String] = data
        add[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let addStatus = SecItemAdd(add as CFDictionary, nil)
        if addStatus == errSecSuccess {
            return nil
        }
        if addStatus == errSecDuplicateItem {
            // Raced with another writer; the item now exists, update it.
            let retry = SecItemUpdate(baseQuery(account: account) as CFDictionary,
                                      changes as CFDictionary)
            return retry == errSecSuccess ? nil : statusError(retry)
        }
        return statusError(addStatus)
    }

    private static func statusError(_ status: OSStatus) -> NSError {
        NSError(domain: NSOSStatusErrorDomain, code: Int(status),
                userInfo: [NSLocalizedDescriptionKey:
                    "Keychain trust-store operation failed (OSStatus \(status))"])
    }
}

/// V2Controller exposes the Go v2 engine to SwiftUI.
@MainActor
final class V2Controller: ObservableObject {
    @Published var state: String = "idle"
    @Published var lastError: String = ""
    @Published var pairedPeers: String = "[]"
    @Published var stats = V2Stats()

    private var sink: GoMediaSink?
    // Keeps the bound sinks alive alongside their Go-side references.
    private var statsSink: GoStatsSink?
    // Keeps the bound StoreSource object alive alongside the Go-side ref.
    private var trustStore: KeychainTrust?

    func setup() {
        // Data dir: app support container.
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        let dir = base.appendingPathComponent("RemoteAU", isDirectory: true).path
        // Move the legacy file-based trust store into the Keychain first;
        // the Go-side migration inside RemoteAUSetupWithKeychainAndMigrate
        // only runs while the Keychain is still empty, so the two compose.
        let trust = KeychainTrust()
        trust.migrateLegacyFile(dir: base)
        let err = RemoteAUSetupWithKeychainAndMigrate(trust, dir)
        if err != nil {
            lastError = err.localizedDescription
            return
        }
        trustStore = trust
        RemoteAUSetStateSink(GoStateSink { [weak self] json in
            Task { @MainActor in
                self?.handleState(json)
            }
        })
        let sinkStats = GoStatsSink { [weak self] json in
            Task { @MainActor in
                self?.handleStats(json)
            }
        }
        statsSink = sinkStats
        RemoteAUSetStatsSink(sinkStats)
        pairedPeers = RemoteAUPeerList()
    }

    struct AdvancedSettings {
        var codecOpus: Bool = false
        var frameMs: Int = 5
        var bitrateKbps: Int = 128
        var fec: Bool = false
        var dtx: Bool = false
    }

    /// Starts streaming from the given host. `pinPrompt` runs on the main
    /// actor to ask the user for the pairing code.
    func connect(host: String, name: String, advanced: AdvancedSettings? = nil,
                 pinPrompt: @escaping () async -> String) {
        // The Go PIN callback blocks a Go goroutine; bridge it to async Swift
        // via a continuation held until the prompt resolves.
        let holder = PINContinuation(prompt: pinPrompt)
        let pinSource = GoPINSource { [holder] in
            holder.blockingPin()
        }
        let s = GoMediaSink { [weak self] pcm in
            self?.deliver(pcm)
        }
        sink = s
        RemoteAUSetMediaSink(s)

        var err: Error?
        if let adv = advanced {
            err = RemoteAUConnectWithCaps(
                host, name,
                Int32(adv.codecOpus ? 1 : 0),
                48000, 2, Int32(adv.frameMs), Int32(adv.bitrateKbps * 1000),
                adv.fec, adv.dtx, 5, 0,
                pinSource
            )
        } else {
            err = RemoteAUConnect(host, name, pinSource)
        }
        if let err {
            lastError = err.localizedDescription
        }
    }

    /// Starts streaming through the secure relay. `relayAddr` is the relay's
    /// `host:port`; `hostDeviceID` is the 32-hex PC device id the relay
    /// resolves to the PC's QUIC endpoint. Advanced defaults mirror `connect`.
    func connectRelay(relayAddr: String, hostDeviceID: String, name: String,
                      advanced: AdvancedSettings? = nil,
                      pinPrompt: @escaping () async -> String) {
        let holder = PINContinuation(prompt: pinPrompt)
        let pinSource = GoPINSource { [holder] in
            holder.blockingPin()
        }
        let s = GoMediaSink { [weak self] pcm in
            self?.deliver(pcm)
        }
        sink = s
        RemoteAUSetMediaSink(s)

        let adv = advanced ?? AdvancedSettings()
        let err = RemoteAUConnectViaRelay(
            relayAddr, hostDeviceID, name,
            Int32(adv.codecOpus ? 1 : 0),
            48000, 2, Int32(adv.frameMs), Int32(adv.bitrateKbps * 1000),
            adv.fec, adv.dtx, 5, 0,
            pinSource
        )
        if let err {
            lastError = err.localizedDescription
        }
    }

    func stop() {
        RemoteAUStop()
    }

    /// Windows capture source: kind 0 = system audio, 1 = render device by
    /// name, 2 = diagnostic test tone, 3 = per-app ("p:1,2" / "x:9").
    /// (Go `int` bridges to Swift `Int32`, matching the other RemoteAU caps.)
    func setSource(kind: Int, name: String) {
        if let err = RemoteAUSetSource(Int32(kind), name) {
            lastError = err.localizedDescription
        }
    }

    /// Quality mode: 0 auto, 1 lowest, 2 lossless, 3 robust, 4 advanced.
    func setQualityMode(_ mode: Int) {
        if let err = RemoteAUSetQualityMode(Int32(mode)) {
            lastError = err.localizedDescription
        }
    }

    func forgetPeer(idHex: String) {
        _ = RemoteAUForgetPeer(idHex)
        pairedPeers = RemoteAUPeerList()
    }

    /// Paired PCs as decoded records (JSON keys follow the Go struct tags).
    var peers: [PairedPC] {
        struct Wire: Decodable {
            var id: String?
            var name: String?
        }
        guard let data = pairedPeers.data(using: .utf8),
              let list = try? JSONDecoder().decode([Wire].self, from: data) else {
            return []
        }
        return list.map { PairedPC(id: $0.id ?? "", name: $0.name ?? "PC") }
    }

    private func deliver(_ pcm: [UInt8]) {
        // Route into the shared ring; the AudioEngineController consumes it.
        V2MediaRouter.shared.push(pcm)
    }

    private func handleState(_ json: String) {
        struct Payload: Decodable {
            var state: String?
            var error: String?
        }
        if let data = json.data(using: .utf8),
           let p = try? JSONDecoder().decode(Payload.self, from: data) {
            state = p.state ?? state
            lastError = p.error ?? ""
        }
    }

    private func handleStats(_ json: String) {
        struct Wire: Decodable {
            var state: String?
            var connected: Bool?
            var rtt_ms: Double?
            var loss_pct: Double?
            var late_pct: Double?
            var jitter_ms: Double?
            var buffer_ms: Double?
            var packets_seen: Double?
            var loss_packets: Double?
            var late_packets: Double?
            var reordered_packets: Double?
            var concealed_frames: Double?
            var max_burst: Double?
            var sample_rate: Int?
            var channels: Int?
        }
        guard let data = json.data(using: .utf8),
              let w = try? JSONDecoder().decode(Wire.self, from: data) else { return }
        var s = V2Stats()
        s.state = w.state ?? stats.state
        s.connected = w.connected ?? false
        s.rttMs = w.rtt_ms ?? 0
        s.lossPct = w.loss_pct ?? 0
        s.latePct = w.late_pct ?? 0
        s.jitterMs = w.jitter_ms ?? 0
        s.bufferMs = w.buffer_ms ?? 0
        s.packetsSeen = w.packets_seen ?? 0
        s.lossPackets = w.loss_packets ?? 0
        s.latePackets = w.late_packets ?? 0
        s.reorderedPackets = w.reordered_packets ?? 0
        s.concealedFrames = w.concealed_frames ?? 0
        s.maxBurst = w.max_burst ?? 0
        s.sampleRate = w.sample_rate ?? 0
        s.channels = w.channels ?? 0
        stats = s
    }
}

struct PairedPC: Identifiable {
    var id: String
    var name: String
}

/// Blocking bridge for the PIN prompt: the Go goroutine parks until the
/// Swift async prompt completes.
final class PINContinuation {
    private let prompt: () async -> String
    private let sema = DispatchSemaphore(value: 0)
    private var value = ""

    init(prompt: @escaping () async -> String) {
        self.prompt = prompt
    }

    /// Called from the Go goroutine.
    func blockingPin() -> String {
        DispatchQueue.main.async { [self] in
            Task {
                value = await prompt()
                sema.signal()
            }
        }
        sema.wait()
        return value
    }
}

/// Routes v2 PCM into the iOS audio pipeline.
final class V2MediaRouter {
    static let shared = V2MediaRouter()
    var handler: (([UInt8]) -> Void)?

    func push(_ pcm: [UInt8]) {
        handler?(pcm)
    }
}

#else

/// Stub mirror of the framework's RemoteAUStatsSink (whose gomobile-imported
/// string parameter is optional) so the placeholder controller below can
/// follow the same wiring as the real branch.
protocol StatsSink: AnyObject {
    func onStats(_ json: String?)
}

final class GoStatsSink: NSObject, StatsSink {
    let onStats: (String) -> Void

    init(onStats: @escaping (String) -> Void) {
        self.onStats = onStats
    }

    func onStats(_ json: String?) {
        guard let json else { return }
        onStats(json)
    }
}

/// Placeholder when the Go framework is not linked (v1-only build).
/// NOTE: release builds always link RemoteAU.xcframework (v2 + Opus), so this
/// branch exists only to keep source-level consistency; it mirrors the real
/// V2Controller surface used by the UI.
@MainActor
final class V2Controller: ObservableObject {
    @Published var state = "unavailable"
    @Published var lastError = ""
    @Published var pairedPeers = "[]"
    @Published var stats = V2Stats()

    private var statsSink: GoStatsSink?

    struct AdvancedSettings {
        var codecOpus: Bool = false
        var frameMs: Int = 5
        var bitrateKbps: Int = 128
        var fec: Bool = false
        var dtx: Bool = false
    }

    struct PairedPC: Identifiable {
        var id: String
        var name: String
    }

    var peers: [PairedPC] { [] }

    func setup() {
        statsSink = GoStatsSink { _ in }
    }
    func connect(host: String, name: String, advanced: AdvancedSettings? = nil,
                 pinPrompt: @escaping () async -> String) {
        lastError = "v2 engine framework is not linked into this build"
    }
    func connectRelay(relayAddr: String, hostDeviceID: String, name: String,
                      advanced: AdvancedSettings? = nil,
                      pinPrompt: @escaping () async -> String) {
        lastError = "v2 engine framework is not linked into this build"
    }
    func stop() {}
    func setSource(kind: Int, name: String) {}
    func setQualityMode(_ mode: Int) {}
    func forgetPeer(idHex: String) {}
}

/// Placeholder media router (real one exists in the framework build).
final class V2MediaRouter {
    static let shared = V2MediaRouter()
    var handler: (([UInt8]) -> Void)?
    func push(_ pcm: [UInt8]) { handler?(pcm) }
}

/// No-op mirror of the real KeychainTrust (framework build only): same API
/// shape so call sites compile unchanged in the v1-only build. The real
/// class conforms to RemoteAUStoreSource, which does not exist here.
final class KeychainTrust {
    func migrateLegacyFile(dir: URL) {}
    func getIdentity() -> Data? { nil }
    func getPeers() -> Data? { nil }
    func putIdentity(_ pkcs8: Data?) -> Error? { nil }
    func putPeers(_ json: Data?) -> Error? { nil }
}

#endif
