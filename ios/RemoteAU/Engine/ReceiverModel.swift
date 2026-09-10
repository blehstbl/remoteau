import Foundation
import UIKit
import Darwin

/// A tiny non-blocking lock wrapper around os_unfair_lock for short critical
/// sections on engine state. The real-time render callback never waits on it
/// (PCMRing has its own try-lock path); these are used by threads only.
struct UnfairLock {
    private var _lock = os_unfair_lock()

    mutating func withLock<R>(_ body: () -> R) -> R {
        os_unfair_lock_lock(&_lock)
        defer { os_unfair_lock_unlock(&_lock) }
        return body()
    }
}

/// Lock-protected box for engine configuration shared between threads.
final class Locked<T> {
    private var lock = UnfairLock()
    private var _value: T

    init(_ value: T) {
        _value = value
    }

    func withLock<R>(_ body: (inout T) -> R) -> R {
        lock.withLock { body(&_value) }
    }

    var value: T {
        withLock { $0 }
    }
}

/// Orchestrates the v1 receiver: UDP audio listener, discovery responder,
/// sender watchdog and the adaptive buffering policy.
///
/// Threading:
/// - `audioThread`: UDP receive + reorder/PLC + ring writes + watchdog.
/// - `discoveryThread`: answers remote-au discovery queries.
/// - `utilityTimer` (1 Hz): adaptive policy + stats snapshots.
/// - All `@Published` updates are posted to the main queue from those threads.
final class ReceiverModel: ObservableObject {

    enum ConnectionState: Equatable {
        case idle
        case waitingForSender       // sockets up, no sender seen yet
        case streaming(name: String)
        case interrupted(reason: String)
    }

    // MARK: Published UI state (main queue only)
    @Published var state: ConnectionState = .idle
    @Published var listening = false
    @Published var lastError: String?
    @Published var senderName: String = ""
    @Published var senderAddress: String = ""
    @Published var streamFormat: String = ""
    @Published var qualityPreset: QualityPreset = .auto
    @Published var manualTargetMs: Double = 20
    @Published var manualIPFilter: String = "" { didSet { syncIPFilter() } }
    @Published var stats = StatsSnapshot()
    @Published var discoveredPeers: [DiscoveredPeer] = []
    @Published var finderOn = false
    @Published var recording = false
    /// URL of the most recently finalized recording (set on manual and
    /// automatic stops so the UI can surface the saved path).
    @Published var lastRecordingURL: URL?

    // MARK: Engine pieces
    private let ring = PCMRing(capacityBytes: 48_000 * 4 * 2, bytesPerFrame: 4) // ~500 ms @48k stereo
    private let audio: AudioEngineController
    private let reorder: Locked<ReorderBuffer?>
    private let policy: Locked<AdaptiveJitterPolicy>

    // MARK: Session recording
    private let recorder = SessionRecorder()
    /// Fast, lock-guarded mirror of `recording` so the network/Go threads can
    /// decide whether to hand PCM to the recorder without touching main
    /// state. The recorder itself drops appends while inactive.
    private let recordingActive = Locked(false)
    /// Uptime of the last PCM delivered by the v2 (Go) engine. The 1 Hz
    /// housekeeping pass treats recent external PCM as a live stream, because
    /// the v2 path never drives the v1 `state` enum.
    private let lastExternalPCM = Locked(0.0)
    /// Format of the current stream, updated on the main queue by
    /// configureStream(). v2 (no v1 handshake) keeps the 48 kHz/stereo default.
    private var recordSampleRate = 48000
    private var recordChannels = 2
    /// Optional provider for the active v2 stream format (rate, channels).
    /// RootView wires this to the V2Controller's latest stats; when unset the
    /// v1 handshake format (or 48 kHz/stereo) is used.
    var v2FormatProvider: (() -> (rate: Int, channels: Int))?

    // Network threads
    private var audioThread: Thread?
    private var discoveryThread: Thread?
    private var finderThread: Thread?
    private var utilityTimer: DispatchSourceTimer?
    private var audioFD: Int32 = -1
    private var discoveryFD: Int32 = -1
    private var finderFD: Int32 = -1
    private let running = Locked(false)
    private let finderRunning = Locked(false)
    private let instanceID: [UInt8] = (0..<16).map { _ in UInt8.random(in: 0...255) }

    // Sender binding + watch state (guarded by senderLock).
    private var senderLock = UnfairLock()
    private var senderAddr: in_addr_t = 0
    private var senderPort: UInt16 = 0
    private var ipFilterAddr: in_addr_t = 0
    private var lastPacketUptime: Double = 0
    private var underrunBaseline: UInt64 = 0
    private var lastPacketsSeen: UInt64 = 0

    init() {
        audio = AudioEngineController(ring: ring)
        let rb = ReorderBuffer(bytesPerFrame: 4, frameDurationMs: 10.0)
        reorder = Locked<ReorderBuffer?>(rb)
        policy = Locked(AdaptiveJitterPolicy(preset: .auto, sampleRate: 48000))
    }

    // MARK: Lifecycle

    func start() {
        guard running.value == false else { return }
        running.withLock { $0 = true }

        do {
            try audio.configureSession()
        } catch {
            postError("Audio session: \(error.localizedDescription)")
        }

        do {
            audioFD = try UDPSocket.openUDP(port: 47000)
        } catch {
            postError("Cannot open UDP 47000: \(error)")
            running.withLock { $0 = false }
            return
        }

        let sink: (UnsafeRawPointer, Int) -> Void = { [weak self] ptr, count in
            self?.handleV1PCM(ptr, count: count)
        }
        reorder.withLock {
            $0?.sink = sink
        }

        setMainState(.waitingForSender)
        setMainListening(true)

        startDiscoveryThread()
        startAudioLoopThread()
        startUtilityTimer()
    }

    func stop() {
        guard running.value else { return }
        running.withLock { $0 = false }
        UDPSocket.closeSocket(audioFD)
        UDPSocket.closeSocket(discoveryFD)
        audioFD = -1
        discoveryFD = -1
        utilityTimer?.cancel()
        utilityTimer = nil
        audio.stop()
        audio.deactivateSession()
        senderLock.withLock {
            senderAddr = 0
            senderPort = 0
        }
        setMainState(.idle)
        setMainListening(false)
    }

    /// Stops everything including the discovery responder and finder.
    func stopAll() {
        stop()
        stopFinder()
    }
    // MARK: PC discovery (finder: broadcast queries, collect announces)

    /// Starts the finder: broadcasts remote-au discovery queries and lists
    /// answering PCs (v2 hosts via the type-3 announce; remote-au `recv`
    /// hosts via the plain v1 announce).
    func startFinder() {
        guard finderRunning.value == false else { return }
        finderRunning.withLock { $0 = true }
        DispatchQueue.main.async { [weak self] in self?.finderOn = true }

        let t = Thread { [weak self] in
            self?.runFinder()
        }
        t.name = "remoteau.finder"
        t.qualityOfService = .utility
        finderThread = t
        t.start()
    }

    func stopFinder() {
        guard finderRunning.value else { return }
        finderRunning.withLock { $0 = false }
        UDPSocket.closeSocket(finderFD)
        finderFD = -1
        DispatchQueue.main.async { [weak self] in self?.finderOn = false }
    }

    private func runFinder() {
        guard let fd = try? UDPSocket.openUDP(port: 0) else {
            return
        }
        finderFD = fd

        let targets = UDPSocket.broadcastTargets(ports: V1Packet.defaultDiscoveryPorts)
        let query = V1Packet.encodeQuery(name: Self.deviceName())

        var instances: [String: DiscoveredPeer] = [:]
        var lastQuery: Double = 0
        let buf = UnsafeMutablePointer<UInt8>.allocate(capacity: 2048)
        defer { buf.deallocate() }

        while finderRunning.value {
            let now = ProcessInfo.processInfo.systemUptime
            if now - lastQuery >= 1.0 {
                lastQuery = now
                for target in targets {
                    _ = UDPSocket.sendto(fd: fd, data: query, addr: target.addr, port: target.port)
                }
            }

            if UDPSocket.pollRead(fd: fd, timeoutMs: 200) {
                var fromAddr: in_addr_t = 0
                var fromPort: UInt16 = 0
                let n = UDPSocket.recvfrom(fd: fd, buffer: buf, bufferLen: 2048,
                                           sender: &fromAddr, senderPort: &fromPort)
                if n > 0 {
                    var packet = [UInt8](repeating: 0, count: n)
                    memcpy(&packet, buf, n)
                    if let msg = V1Packet.decodeDiscovery(packet),
                       msg.type == .announce || msg.type == .announceV2 {
                        let a = msg.announce
                        let key = a.instanceID.map { String(format: "%02x", $0) }.joined()
                        let addrString = a.advertised.count == 4 && a.advertised != [0, 0, 0, 0]
                            ? UDPSocket.ipv4String(bytes: a.advertised)
                            : UDPSocket.ipv4String(fromAddr)
                        instances[key] = DiscoveredPeer(
                            name: a.name.isEmpty ? "PC" : a.name,
                            address: addrString,
                            port: a.tcpPort,
                            protocolVersion: a.protoVersion == 0 ? 1 : Int(a.protoVersion),
                            paired: false
                        )
                        let snapshot = Array(instances.values)
                        DispatchQueue.main.async { [weak self] in
                            self?.discoveredPeers = snapshot.sorted { $0.name < $1.name }
                        }
                    }
                }
            }
        }
    }

    /// Drops the current sender binding but keeps listening (reconnect UX).
    func disconnect() {
        senderLock.withLock {
            senderAddr = 0
            senderPort = 0
        }
        ring.reset()
        reorder.withLock { $0?.reset() }
        DispatchQueue.main.async { [weak self] in
            self?.senderName = ""
            self?.senderAddress = ""
            self?.state = .waitingForSender
        }
    }

    /// Feeds externally produced PCM (the Go v2 engine, when linked) into the
    /// same ring/render pipeline. Non-blocking; called from Go callbacks.
    func feedExternalPCM(_ bytes: [UInt8]) {
        if !bytes.isEmpty {
            lastExternalPCM.withLock { $0 = ProcessInfo.processInfo.systemUptime }
            if recordingActive.value {
                recorder.append(bytes)
            }
        }
        bytes.withUnsafeBufferPointer { buf in
            if let base = buf.baseAddress {
                ring.write(base, count: bytes.count)
            }
        }
    }

    /// v1 path: the reorder buffer hands us ordered/concealed S16LE PCM from
    /// the network thread. Record the original bytes before the ring may drop
    /// the oldest frames under overflow, then write into the ring.
    private func handleV1PCM(_ ptr: UnsafeRawPointer, count: Int) {
        if count > 0, recordingActive.value {
            recorder.append([UInt8](UnsafeRawBufferPointer(start: ptr, count: count)))
        }
        ring.write(ptr, count: count)
    }

    // MARK: Session recording

    /// Starts or stops a WAV recording of the current stream. Returns the
    /// saved file URL when a recording was stopped, nil otherwise.
    @discardableResult
    func toggleRecording() -> URL? {
        if recording {
            return stopRecording()
        }
        // Prefer the live v2 format; fall back to the v1 handshake format
        // (which itself defaults to 48 kHz/stereo before a stream arrives).
        var rate = recordSampleRate > 0 ? recordSampleRate : 48000
        var channels = recordChannels > 0 ? recordChannels : 2
        if let v2fmt = v2FormatProvider?(), v2fmt.rate > 0 {
            rate = v2fmt.rate
            if v2fmt.channels > 0 { channels = v2fmt.channels }
        }
        do {
            try recorder.start(sampleRate: rate, channels: channels)
            recordingActive.withLock { $0 = true }
            recording = true
        } catch {
            postError("Recording: \(error.localizedDescription)")
        }
        return nil
    }

    /// Finalizes the active recording (if any) and stores the saved URL so the
    /// UI surfaces it even when the stop was automatic. Idempotent.
    @discardableResult
    private func stopRecording() -> URL? {
        guard recording else { return nil }
        recordingActive.withLock { $0 = false }
        let url = recorder.stop()
        recording = false
        if let url {
            lastRecordingURL = url
        }
        return url
    }

    func setPreset(_ preset: QualityPreset) {
        policy.withLock { p in
            // Bounds are user configuration, not part of the preset; carry
            // them across the rebuild.
            let lo = p.minMs
            let hi = p.maxMs
            p = AdaptiveJitterPolicy(preset: preset, sampleRate: 48000)
            p.setBounds(minMs: lo, maxMs: hi)
        }
    }

    func setManualTarget(ms: Double) {
        policy.withLock { $0.setManualTarget(ms: ms) }
    }

    /// Advanced jitter bounds; forwarded to the adaptive policy.
    func setJitterBounds(minMs: Double, maxMs: Double) {
        policy.withLock { $0.setBounds(minMs: minMs, maxMs: maxMs) }
    }

    private func syncIPFilter() {
        let s = manualIPFilter.trimmingCharacters(in: .whitespaces)
        var addr: in_addr_t = 0
        if !s.isEmpty {
            var inAddr = in_addr()
            if inet_pton(AF_INET, s, &inAddr) == 1 {
                addr = inAddr.s_addr
            }
        }
        senderLock.withLock { ipFilterAddr = addr }
    }

    // MARK: Discovery responder (v1: PCs running remote-au query us)

    private func startDiscoveryThread() {
        let t = Thread { [weak self] in
            self?.runDiscovery()
        }
        t.name = "remoteau.discovery"
        t.qualityOfService = .utility
        discoveryThread = t
        t.start()
    }

    private func runDiscovery() {
        var fd: Int32 = -1
        for p in V1Packet.defaultDiscoveryPorts {
            if let f = try? UDPSocket.openUDP(port: p) {
                fd = f
                break
            }
        }
        guard fd >= 0 else {
            postError("Discovery ports unavailable")
            return
        }
        discoveryFD = fd

        let name = Self.deviceName()
        var advertised: [UInt8] = [0, 0, 0, 0]
        let ownIP = UDPSocket.primaryIPv4()
        if ownIP != 0 {
            withUnsafeBytes(of: ownIP) { raw in
                for (i, b) in raw.prefix(4).enumerated() {
                    advertised[i] = b
                }
            }
        }
        let announce = V1Packet.encodeAnnounce(announce: .init(
            tcpPort: 47000, instanceID: instanceID, advertised: advertised, name: name,
            protoVersion: 0
        ))

        var tokens = 16
        var lastRefill = ProcessInfo.processInfo.systemUptime
        let buf = UnsafeMutablePointer<UInt8>.allocate(capacity: 2048)
        defer { buf.deallocate() }

        while running.value {
            if UDPSocket.pollRead(fd: fd, timeoutMs: 250) {
                var fromAddr: in_addr_t = 0
                var fromPort: UInt16 = 0
                let n = UDPSocket.recvfrom(fd: fd, buffer: buf, bufferLen: 2048,
                                           sender: &fromAddr, senderPort: &fromPort)
                if n > 0 {
                    var packet = [UInt8](repeating: 0, count: n)
                    memcpy(&packet, buf, n)
                    if let msg = V1Packet.decodeDiscovery(packet), msg.type == .query {
                        let now = ProcessInfo.processInfo.systemUptime
                        let elapsed = now - lastRefill
                        if elapsed >= 0.02 {
                            tokens = min(16, tokens + Int(elapsed / 0.02))
                            lastRefill = now
                        }
                        if tokens > 0 {
                            tokens -= 1
                            _ = UDPSocket.sendto(fd: fd, data: announce, addr: fromAddr, port: fromPort)
                        }
                    }
                }
            }
        }
    }

    // MARK: Audio receive loop

    private func startAudioLoopThread() {
        let t = Thread { [weak self] in
            self?.runAudio()
        }
        t.name = "remoteau.audio.rx"
        t.qualityOfService = .userInitiated
        audioThread = t
        t.start()
    }

    private func runAudio() {
        let buf = UnsafeMutablePointer<UInt8>.allocate(capacity: 4096)
        defer { buf.deallocate() }

        var lastHousekeeping = ProcessInfo.processInfo.systemUptime

        while running.value {
            let fd = audioFD
            guard fd >= 0 else { break }

            if UDPSocket.pollRead(fd: fd, timeoutMs: 10) {
                var fromAddr: in_addr_t = 0
                var fromPort: UInt16 = 0
                let n = UDPSocket.recvfrom(fd: fd, buffer: buf, bufferLen: 4096,
                                           sender: &fromAddr, senderPort: &fromPort)
                if n > 0 {
                    var packet = [UInt8](repeating: 0, count: n)
                    memcpy(&packet, buf, n)
                    handleDatagram(packet, fromAddr, fromPort)
                }
            }

            reorder.withLock { $0?.tick() }

            let now = ProcessInfo.processInfo.systemUptime

            // Watchdog: sender gone for 3 s → drop binding; HELLO resumes.
            senderLock.withLock {
                if senderAddr != 0, now - lastPacketUptime > 3.0 {
                    senderAddr = 0
                    senderPort = 0
                    ring.reset()
                    reorder.withLock { $0?.reset() }
                    DispatchQueue.main.async { [weak self] in
                        guard let self else { return }
                        self.senderName = ""
                        self.senderAddress = ""
                        self.state = .interrupted(reason: "Waiting for the PC to come back…")
                    }
                }
            }

            if now - lastHousekeeping >= 1.0 {
                lastHousekeeping = now
                housekeeping(now: now)
            }
        }
    }

    private func handleDatagram(_ packet: [UInt8], _ fromAddr: in_addr_t, _ fromPort: UInt16) {
        guard let decoded = V1Packet.decode(packet) else { return }

        // Sender binding: first active sender wins; optional manual filter.
        var boundAddr: in_addr_t = 0
        var filterAddr: in_addr_t = 0
        senderLock.withLock {
            boundAddr = senderAddr
            filterAddr = ipFilterAddr
        }
        if filterAddr != 0 && filterAddr != fromAddr { return }
        if boundAddr != 0 && boundAddr != fromAddr { return }

        switch decoded {
        case .handshake(let hs):
            senderLock.withLock {
                senderAddr = fromAddr
                senderPort = fromPort
            }
            lastPacketUptime = ProcessInfo.processInfo.systemUptime
            configureStream(hs)

        case .audio(let audioPkt):
            lastPacketUptime = ProcessInfo.processInfo.systemUptime
            if boundAddr == 0 {
                senderLock.withLock {
                    senderAddr = fromAddr
                    senderPort = fromPort
                }
                boundAddr = fromAddr
            }
            reorder.withLock { rb in
                rb?.accept(seq: audioPkt.seq, captureFrame: audioPkt.captureFrame,
                           payload: audioPkt.payload)
            }
        }
    }

    private func configureStream(_ hs: V1Packet.Handshake) {
        let bytesPerFrame = hs.bytesPerFrame
        let needsRebuild: Bool = reorder.withLock { rb -> Bool in
            guard let rb else { return true }
            return rb.bytesPerFrame != bytesPerFrame
        }
        if needsRebuild {
            let frameMs = Double(hs.frameSamples) / Double(hs.sampleRate) * 1000.0
            let rb = ReorderBuffer(bytesPerFrame: bytesPerFrame, frameDurationMs: frameMs)
            rb.sampleRate = Double(hs.sampleRate)
            rb.sink = { [weak self] ptr, count in
                self?.handleV1PCM(ptr, count: count)
            }
            ring.setBytesPerFrame(bytesPerFrame)
            reorder.withLock { $0 = rb }
        } else {
            reorder.withLock { $0?.sampleRate = Double(hs.sampleRate) }
        }

        let frameMs = Double(hs.frameSamples) / Double(hs.sampleRate) * 1000.0
        let fmt = "\(hs.sampleRate) Hz · \(hs.channels == 2 ? "stereo" : "\(hs.channels) ch") · \(Int(round(frameMs))) ms"

        var addrString = ""
        senderLock.withLock {
            if senderAddr != 0 { addrString = UDPSocket.ipv4String(senderAddr) }
        }
        let name = hs.name.isEmpty ? "PC" : hs.name

        DispatchQueue.main.async { [weak self] in
            guard let self else { return }
            if self.senderName != name || self.streamFormat != fmt {
                self.senderName = name
                self.streamFormat = fmt
                self.senderAddress = addrString
                self.ring.reset()
                self.reorder.withLock { $0?.reset() }
            }
            self.recordSampleRate = Int(hs.sampleRate)
            self.recordChannels = max(1, Int(hs.channels))
            self.state = .streaming(name: name)
        }

        // Make sure audio is playing (e.g. resumed while backgrounded).
        if !audio.engineRunning {
            try? audio.start()
        }
    }

    // MARK: Utility timer (adaptive policy + stats publication, 1 Hz)

    private func startUtilityTimer() {
        let t = DispatchSource.makeTimerSource(queue: DispatchQueue.global(qos: .utility))
        t.schedule(deadline: .now() + 1.0, repeating: 1.0)
        t.setEventHandler { [weak self] in
            self?.housekeeping(now: ProcessInfo.processInfo.systemUptime)
        }
        t.resume()
        utilityTimer = t
    }

    private func housekeeping(now: Double) {
        guard running.value else { return }

        // Controlled re-prime after a capture-clock discontinuity: reset the
        // adaptive policy and drift state but keep the buffers, so audio
        // keeps flowing. The audio thread's 1 Hz pass and the utility timer
        // both land here; consumeReprime() returns true exactly once.
        let reprime = reorder.withLock { rb -> Bool in
            guard let rb else { return false }
            return rb.consumeReprime()
        }
        if reprime {
            policy.withLock { $0.reset() }
            audio.resetDrift()
        }

        // Adaptive jitter target.
        let net = AdaptiveJitterPolicy.Network(
            lossPercent: reorder.value?.lossPercent ?? 0,
            jitterMs: reorder.value?.arrivalJitterMs ?? 0,
            underruns: ring.underruns &- underrunBaseline,
            bufferDepthMs: audio.ringDepthMs,
            maxBurst: reorder.value?.maxBurstLoss ?? 0
        )
        var targetMs = policy.withLock { p -> Double in
            p.update(net, now: now)
        }
        if policy.value.preset == .advanced {
            targetMs = policy.value.manualTargetMs
        }
        audio.setTargetMs(targetMs)

        // Stats snapshot.
        var s = StatsSnapshot()
        s.bufferDepthMs = audio.ringDepthMs
        s.targetBufferMs = targetMs
        s.jitterMs = reorder.value?.arrivalJitterMs ?? 0
        s.lossPercent = reorder.value?.lossPercent ?? 0
        s.underruns = ring.underruns
        s.droppedFrames = ring.droppedFrames
        s.concealedFrames = reorder.value?.concealedFrames ?? 0
        s.latePackets = reorder.value?.latePackets ?? 0
        s.reorderedPackets = reorder.value?.reorderedPackets ?? 0
        s.packetsSeen = reorder.value?.packetsSeen ?? 0
        s.outputRoute = audio.outputRouteName
        s.engineRunning = audio.engineRunning
        s.codec = "pcm"
        s.driftRatio = audio.driftRatio
        s.maxBurstLoss = reorder.value?.maxBurstLoss ?? 0
        s.overrunEvents = ring.overruns
        s.discontinuities = reorder.value?.discontinuities ?? 0
        s.fecAdvisory = policy.value.fecAdvisory
        s.captureFrameRateHz = reorder.value?.captureFrameRateHz ?? 0

        let packets = s.packetsSeen
        let deltaPackets = packets &- lastPacketsSeen
        lastPacketsSeen = packets
        s.packetRatePerSec = Int(deltaPackets)
        s.bitrateKbps = Double(deltaPackets) * Double(reorder.value?.bytesPerFrame ?? 4) * 8.0

        DispatchQueue.main.async { [weak self] in
            self?.stats = s
        }

        // Auto-stop recording once the stream ends so the WAV is finalized
        // even when the user never toggles it off. Performed on the main
        // queue because `recording`/`state` are main-queue state. The v2 path
        // does not drive the v1 state enum, so recent external PCM also
        // counts as a live stream.
        DispatchQueue.main.async { [weak self] in
            guard let self, self.recording else { return }
            if case .streaming = self.state { return }
            let fresh = ProcessInfo.processInfo.systemUptime
                - self.lastExternalPCM.value < 2.0
            if fresh { return }
            self.stopRecording()
        }
    }

    private func postError(_ message: String) {
        DispatchQueue.main.async { [weak self] in
            self?.lastError = message
        }
    }

    private func setMainState(_ s: ConnectionState) {
        DispatchQueue.main.async { [weak self] in
            self?.state = s
        }
    }

    private func setMainListening(_ value: Bool) {
        DispatchQueue.main.async { [weak self] in
            self?.listening = value
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
