import Foundation
import Darwin

/// UDP socket helpers over BSD sockets (Darwin). Used for remote-au v1
/// compatibility: the sender streams AUDIO datagrams to our listen port and
/// re-announces HELLO datagrams every second; discovery peers query our
/// responder port.
enum UDPSocket {

    struct SocketError: Error {
        var message: String
    }

    static func openUDP(port: UInt16, reuse: Bool = true) throws -> Int32 {
        let fd = socket(AF_INET, SOCK_DGRAM, 0)
        guard fd >= 0 else { throw SocketError(message: "socket: errno \(errno)") }

        var on: Int32 = 1
        if reuse {
            _ = setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &on, socklen_t(MemoryLayout<Int32>.size))
        }
        // Non-blocking; the receive loop uses poll().
        let flags = fcntl(fd, F_GETFL, 0)
        _ = fcntl(fd, F_SETFL, flags | O_NONBLOCK)

        // Bump the receive buffer; Wi-Fi bursts are real.
        var rcvbuf: Int32 = 256 * 1024
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVBUF, &rcvbuf, socklen_t(MemoryLayout<Int32>.size))

        var addr = sockaddr_in()
        addr.sin_family = sa_family_t(AF_INET)
        addr.sin_port = port.bigEndian
        addr.sin_addr = in_addr(s_addr: INADDR_ANY)

        let bindResult = withUnsafePointer(to: &addr) { ptr -> Int32 in
            ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                bind(fd, sa, socklen_t(MemoryLayout<sockaddr_in>.size))
            }
        }
        guard bindResult == 0 else {
            close(fd)
            throw SocketError(message: "bind \(port): errno \(errno)")
        }
        return fd
    }

    static func closeSocket(_ fd: Int32) {
        guard fd >= 0 else { return }
        close(fd)
    }

    /// Receives one datagram if available (non-blocking).
    /// Returns (bytes read, sender address in network order) or nil.
    static func recvfrom(fd: Int32, buffer: UnsafeMutableRawPointer, bufferLen: Int, sender: inout in_addr_t, senderPort: inout UInt16) -> Int {
        var from = sockaddr_in()
        var fromLen = socklen_t(MemoryLayout<sockaddr_in>.size)
        let n = withUnsafeMutablePointer(to: &from) { fromPtr -> Int in
            fromPtr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa -> Int in
                withUnsafeMutablePointer(to: &fromLen) { lenPtr in
                    Darwin.recvfrom(fd, buffer, bufferLen, 0, sa, lenPtr)
                }
            }
        }
        if n > 0 {
            sender = from.sin_addr.s_addr
            senderPort = UInt16(bigEndian: from.sin_port)
        }
        return n
    }

    /// Sends a datagram to an IPv4 address (network byte order) and port.
    static func sendto(fd: Int32, data: [UInt8], addr: in_addr_t, port: UInt16) -> Bool {
        var dst = sockaddr_in()
        dst.sin_family = sa_family_t(AF_INET)
        dst.sin_port = port.bigEndian
        dst.sin_addr = in_addr(s_addr: addr)

        let sent = data.withUnsafeBufferPointer { buf -> Int in
            withUnsafePointer(to: &dst) { ptr -> Int in
                ptr.withMemoryRebound(to: sockaddr.self, capacity: 1) { sa in
                    Darwin.sendto(fd, buf.baseAddress, data.count, 0, sa, socklen_t(MemoryLayout<sockaddr_in>.size))
                }
            }
        }
        return sent == data.count
    }

    /// Waits for readability with timeout (ms). Returns true when readable.
    static func pollRead(fd: Int32, timeoutMs: Int32) -> Bool {
        var pfd = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        let r = poll(&pfd, 1, timeoutMs)
        return r > 0 && (pfd.revents & Int16(POLLIN)) != 0
    }

    /// Local IPv4 address of the primary interface (for announce replies).
    static func primaryIPv4() -> in_addr_t {
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return 0 }
        defer { freeifaddrs(ifaddr) }

        var candidate: in_addr_t = 0
        var cursor: UnsafeMutablePointer<ifaddrs>? = first
        while let ptr = cursor {
            let ifa = ptr.pointee
            if let sa = ifa.ifa_addr, sa.pointee.sa_family == sa_family_t(AF_INET) {
                let sin = sa.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { $0.pointee }
                let addr = sin.sin_addr.s_addr
                let firstOctet = addr & 0xFF
                let secondOctet = (addr >> 8) & 0xFF
                if firstOctet != 127 { // skip loopback
                    let isLinkLocal = firstOctet == 169 && secondOctet == 254
                    if !isLinkLocal {
                        candidate = addr
                        break
                    }
                    if candidate == 0 { candidate = addr }
                }
            }
            cursor = ptr.pointee.ifa_next
        }
        return candidate
    }

    static func ipv4String(_ addr: in_addr_t) -> String {
        var a = addr
        var buf = [CChar](repeating: 0, count: Int(INET_ADDRSTRLEN))
        inet_ntop(AF_INET, &a, &buf, socklen_t(INET_ADDRSTRLEN))
        return String(cString: buf)
    }

    static func ipv4String(bytes b: [UInt8]) -> String {
        guard b.count == 4 else { return "0.0.0.0" }
        return "\(b[0]).\(b[1]).\(b[2]).\(b[3])"
    }

    /// Discovery broadcast targets: loopback, global broadcast and every
    /// interface's directed broadcast address (mirrors remote-au's targets).
    static func broadcastTargets(ports: [UInt16]) -> [(addr: in_addr_t, port: UInt16)] {
        var addrs: [in_addr_t] = []
        var seen = Set<in_addr_t>()
        func add(_ a: in_addr_t) {
            if seen.insert(a).inserted {
                addrs.append(a)
            }
        }
        add(0x0100007F) // 127.0.0.1 (network order)
        add(0xFFFFFFFF) // 255.255.255.255
        for a in interfaceBroadcastAddrs() {
            add(a)
        }

        var out: [(addr: in_addr_t, port: UInt16)] = []
        for p in ports {
            for a in addrs {
                out.append((a, p))
            }
        }
        return out
    }

    /// Directed broadcast addresses of up interfaces (network order).
    static func interfaceBroadcastAddrs() -> [in_addr_t] {
        var ifaddr: UnsafeMutablePointer<ifaddrs>?
        guard getifaddrs(&ifaddr) == 0, let first = ifaddr else { return [] }
        defer { freeifaddrs(ifaddr) }

        var out: [in_addr_t] = []
        var cursor: UnsafeMutablePointer<ifaddrs>? = first
        while let ptr = cursor {
            let ifa = ptr.pointee
            if let sa = ifa.ifa_addr, sa.pointee.sa_family == sa_family_t(AF_INET),
               let maskPtr = ifa.ifa_netmask {
                let sin = sa.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { $0.pointee }
                let mask = maskPtr.withMemoryRebound(to: sockaddr_in.self, capacity: 1) { $0.pointee }
                let ip = sin.sin_addr.s_addr
                let m = mask.sin_addr.s_addr
                // Directed broadcast = ip | ~mask, computed byte-wise (the
                // values are in network order).
                var broadcast: in_addr_t = 0
                for i in 0..<4 {
                    let ipB = UInt8(truncatingIfNeeded: ip >> UInt32(i * 8))
                    let mB = UInt8(truncatingIfNeeded: m >> UInt32(i * 8))
                    broadcast |= in_addr_t(UInt32(ipB | ~mB)) << UInt32(i * 8)
                }
                // Skip loopback broadcasts.
                if (broadcast & 0xFF) != 127 {
                    out.append(broadcast)
                }
            }
            cursor = ptr.pointee.ifa_next
        }
        return out
    }
}
