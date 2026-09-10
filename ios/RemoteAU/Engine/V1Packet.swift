import Foundation

/// Wire-format constants and parsers for remote-au v1 ("RAUU") UDP datagrams.
/// Byte order is big-endian, matching remote-au internal/protocol/udp.go.
enum V1Packet {

    static let magic: [UInt8] = [0x52, 0x41, 0x55, 0x55] // "RAUU"
    static let version: UInt8 = 1
    static let maxDatagramBytes = 1500
    static let maxAudioPayloadBytes = 960

    enum DatagramType: UInt8 {
        case hello = 1
        case audio = 2
    }

    /// Decoded remote-au v1 handshake (sent as HELLO datagrams).
    struct Handshake: Equatable {
        var sampleRate: UInt32
        var channels: UInt8
        var format: UInt8      // 1 = S16LE
        var frameSamples: UInt16
        var name: String

        var bytesPerFrame: Int { Int(channels) * 2 }

        static let formatS16LE: UInt8 = 1
    }

    /// Decoded audio datagram. `payload` aliases the packet buffer; copy before
    /// storing. Use `copyingPayload()` for an owned copy.
    struct Audio {
        var seq: UInt64
        var captureFrame: UInt64
        var payload: ArraySlice<UInt8>

        var payloadBytes: Int { payload.count }
    }

    enum DecodeResult {
        case handshake(Handshake)
        case audio(Audio)
    }

    /// Decodes one datagram. `packet` must be the exact received length.
    static func decode(_ packet: [UInt8]) -> DecodeResult? {
        guard packet.count >= 6 else { return nil }
        guard packet[0] == magic[0], packet[1] == magic[1],
              packet[2] == magic[2], packet[3] == magic[3] else { return nil }
        guard packet[4] == version else { return nil }
        guard let type = DatagramType(rawValue: packet[5]) else { return nil }
        let body = packet[6...]

        switch type {
        case .hello:
            return decodeHello(body)
        case .audio:
            return decodeAudio(body)
        }
    }

    private static func decodeHello(_ body: ArraySlice<UInt8>) -> DecodeResult? {
        // flags(1) sampleRate(4) channels(1) format(1) frameSamples(2) nameLen(2) name
        let fixed = 1 + 4 + 1 + 1 + 2 + 2
        guard body.count >= fixed else { return nil }
        let b = Array(body)
        let nameLen = Int(readU16(b, 9))
        guard b.count == fixed + nameLen, nameLen <= 255 else { return nil }
        let nameData = Data(b[11..<(11 + nameLen)])
        let hs = Handshake(
            sampleRate: readU32(b, 1),
            channels: b[5],
            format: b[6],
            frameSamples: readU16(b, 7),
            name: String(data: nameData, encoding: .utf8) ?? ""
        )
        guard hs.format == Handshake.formatS16LE else { return nil }
        guard (8000...192000).contains(hs.sampleRate) else { return nil }
        guard (1...8).contains(hs.channels) else { return nil }
        guard (1...4096).contains(hs.frameSamples) else { return nil }
        return .handshake(hs)
    }

    private static func decodeAudio(_ body: ArraySlice<UInt8>) -> DecodeResult? {
        // seq(8) captureFrame(8) payloadLen(2) payload
        guard body.count >= 18 else { return nil }
        let b = Array(body)
        let payloadLen = Int(readU16(b, 16))
        guard payloadLen > 0, payloadLen <= maxAudioPayloadBytes else { return nil }
        guard body.count == 18 + payloadLen else { return nil }
        let audio = Audio(
            seq: readU64(b, 0),
            captureFrame: readU64(b, 8),
            payload: body.suffix(payloadLen)
        )
        return .audio(audio)
    }

    // MARK: - Discovery ("RAUD" query/announce), big-endian

    static let discoveryMagic: [UInt8] = [0x52, 0x41, 0x55, 0x44] // "RAUD"
    static let discoveryVersion: UInt8 = 1
    static let defaultDiscoveryPorts: [UInt16] = [47001, 48001, 49001]

    enum DiscoveryType: UInt8 {
        case query = 1
        case announce = 2
        case announceV2 = 3
    }

    struct Announce {
        var tcpPort: Int
        var instanceID: [UInt8] // 16 bytes
        var advertised: [UInt8] // 4 bytes, announcer's IPv4 (network order)
        var name: String
        var protoVersion: UInt8 // 0 for plain v1 announces
    }

    /// Encodes a discovery QUERY datagram.
    static func encodeQuery(name: String) -> [UInt8] {
        let nameBytes = Array(name.utf8.prefix(255))
        var p: [UInt8] = []
        p.append(contentsOf: discoveryMagic)
        p.append(discoveryVersion)
        p.append(DiscoveryType.query.rawValue)
        p.append(contentsOf: u16(0))          // tcp port (unused for query)
        p.append(UInt8(nameBytes.count))
        p.append(contentsOf: nameBytes)
        return p
    }

    /// Encodes a discovery ANNOUNCE reply (remote-au v1 wire format:
    /// magic, version, type, port u16, nameLen u8, instance 16B,
    /// advertised IPv4 4B, name).
    static func encodeAnnounce(announce: Announce) -> [UInt8] {
        let nameBytes = Array(announce.name.utf8.prefix(255))
        var p: [UInt8] = []
        p.append(contentsOf: discoveryMagic)
        p.append(discoveryVersion)
        p.append(DiscoveryType.announce.rawValue)
        p.append(contentsOf: u16(UInt16(clamping: announce.tcpPort)))
        p.append(UInt8(nameBytes.count))
        p.append(contentsOf: announce.instanceID.prefix(16))
        p.append(contentsOf: announce.advertised.prefix(4))
        p.append(contentsOf: nameBytes)
        if announce.protoVersion != 0 {
            // v2 marker rides in a separate datagram type (see
            // encodeAnnounceV2); never inside the v1 packet.
        }
        return p
    }

    /// Encodes a v2 ANNOUNCE (type 3): identical body to v1 plus a trailing
    /// protocol-version byte. v1 parsers reject the unknown type gracefully;
    /// v2 finders prefer it.
    static func encodeAnnounceV2(announce: Announce) -> [UInt8] {
        var p = encodeAnnounce(announce: announce)
        p.append(announce.protoVersion)
        return p
    }

    /// Decodes a discovery QUERY or ANNOUNCE.
    static func decodeDiscovery(_ packet: [UInt8]) -> (type: DiscoveryType, queryName: String, announce: Announce)? {
        let headerLen = 9
        guard packet.count >= headerLen else { return nil }
        guard packet[0] == discoveryMagic[0], packet[1] == discoveryMagic[1],
              packet[2] == discoveryMagic[2], packet[3] == discoveryMagic[3] else { return nil }
        guard packet[4] == discoveryVersion else { return nil }
        guard let type = DiscoveryType(rawValue: packet[5]) else { return nil }
        let tcpPort = Int(readU16(packet, 6))
        let nameLen = Int(packet[8])

        switch type {
        case .query:
            guard packet.count == headerLen + nameLen else { return nil }
            guard tcpPort == 0 else { return nil }
            let name = String(data: Data(packet[headerLen..<(headerLen + nameLen)]), encoding: .utf8) ?? ""
            return (type, name, Announce(tcpPort: 0, instanceID: [], advertised: [], name: "", protoVersion: 0))
        case .announce, .announceV2:
            // Announce body: instance(16) + advertised(4) + name.
            let fixed = 16 + 4
            let wantLen = headerLen + fixed + nameLen
            guard packet.count == wantLen || packet.count == wantLen + 1 else { return nil }
            let instance = Array(packet[headerLen..<(headerLen + 16)])
            let advertised = Array(packet[(headerLen + 16)..<(headerLen + 20)])
            let nameStart = headerLen + fixed
            let name = String(data: Data(packet[nameStart..<(nameStart + nameLen)]), encoding: .utf8) ?? ""
            var protoVersion: UInt8 = 0
            if packet.count == wantLen + 1 {
                protoVersion = packet[wantLen]
            }
            return (type, name, Announce(tcpPort: tcpPort, instanceID: instance,
                                         advertised: advertised, name: name,
                                         protoVersion: protoVersion))
        }
    }

    // MARK: - Byte readers

    static func u16(_ v: UInt16) -> [UInt8] {
        [UInt8(v >> 8), UInt8(v & 0xFF)]
    }

    static func readU16(_ b: [UInt8], _ off: Int) -> UInt16 {
        UInt16(b[off]) << 8 | UInt16(b[off + 1])
    }

    static func readU32(_ b: [UInt8], _ off: Int) -> UInt32 {
        UInt32(b[off]) << 24 | UInt32(b[off + 1]) << 16 | UInt32(b[off + 2]) << 8 | UInt32(b[off + 3])
    }

    static func readU64(_ b: [UInt8], _ off: Int) -> UInt64 {
        var v: UInt64 = 0
        for i in 0..<8 { v = v << 8 | UInt64(b[off + i]) }
        return v
    }
}
