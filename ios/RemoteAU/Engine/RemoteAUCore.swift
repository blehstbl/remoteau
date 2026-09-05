import Foundation
import SwiftUI

/// Swift bridge to the Go v2 engine (RemoteAU.xcframework via gomobile).
/// Everything here compiles to no-ops when the framework is not linked, so
/// the v1-only build stays intact.
#if canImport(RemoteAU)
import RemoteAU

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

final class GoPINSource: NSObject, RemoteAURequestSource {
    let get: () -> String

    init(get: @escaping () -> String) {
        self.get = get
    }

    func getPIN() -> String {
        get()
    }
}

/// V2Controller exposes the Go v2 engine to SwiftUI.
@MainActor
final class V2Controller: ObservableObject {
    @Published var state: String = "idle"
    @Published var lastError: String = ""
    @Published var pairedPeers: String = "[]"

    private var sink: GoMediaSink?

    func setup() {
        // Data dir: app support container.
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first!
        let dir = base.appendingPathComponent("RemoteAU", isDirectory: true).path
        let err = RemoteAUSetup(dir)
        if err != nil {
            lastError = err.localizedDescription
            return
        }
        RemoteAUSetStateSink(GoStateSink { [weak self] json in
            Task { @MainActor in
                self?.handleState(json)
            }
        })
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
        if err != nil {
            lastError = err.localizedDescription
        }
    }

    func stop() {
        RemoteAUStop()
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

/// Placeholder when the Go framework is not linked (v1-only build).
@MainActor
final class V2Controller: ObservableObject {
    @Published var state = "unavailable"
    @Published var lastError = ""
    @Published var pairedPeers = "[]"

    func setup() {}
    func connect(host: String, name: String, pinPrompt: @escaping () async -> String) {
        lastError = "v2 engine framework is not linked into this build"
    }
    func stop() {}
    func forgetPeer(idHex: String) {}
}

#endif
