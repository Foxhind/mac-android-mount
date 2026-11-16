import Foundation

/// A minimal HTTP client for the daemon's control socket (a Unix socket,
/// which URLSession cannot reach). Each call is one blocking HTTP/1.0
/// exchange, so run it off the main actor.
struct ControlClient: Sendable {
    let socketPath: String

    struct Failure: Error, CustomStringConvertible {
        let description: String
    }

    /// Lists the devices the daemon knows about.
    func devices() throws -> [Device] {
        let (status, body) = try request("GET", "/v1/devices")
        guard status == 200 else { throw Failure(description: "devices: HTTP \(status)") }
        return try JSONDecoder().decode([Device].self, from: body)
    }

    /// Sends a command ("eject" or "mount") for one device.
    func send(_ command: String, to id: String) throws {
        let escaped = id.addingPercentEncoding(withAllowedCharacters: .alphanumerics) ?? id
        let (status, body) = try request("POST", "/v1/devices/\(escaped)/\(command)")
        guard status == 204 else {
            // The daemon explains failures as {"error": "..."}.
            struct Reply: Decodable { let error: String }
            let reason = (try? JSONDecoder().decode(Reply.self, from: body))?.error
            throw Failure(description: reason ?? "HTTP \(status)")
        }
    }

    private func request(_ method: String, _ path: String) throws -> (Int, Data) {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { throw posixFailure("socket") }
        defer { close(fd) }

        var one: Int32 = 1
        setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
        var timeout = timeval(tv_sec: 3, tv_usec: 0)
        setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))
        setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, socklen_t(MemoryLayout<timeval>.size))

        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let pathBytes = Array(socketPath.utf8)
        guard pathBytes.count < MemoryLayout.size(ofValue: addr.sun_path) else {
            throw Failure(description: "socket path too long: \(socketPath)")
        }
        withUnsafeMutableBytes(of: &addr.sun_path) { dst in
            dst.copyBytes(from: pathBytes)
        }
        let connected = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size))
            }
        }
        guard connected == 0 else { throw posixFailure("connect") }

        let head = "\(method) \(path) HTTP/1.0\r\nHost: droidfs\r\nContent-Length: 0\r\n\r\n"
        let sent = head.utf8CString.withUnsafeBufferPointer { buf in
            write(fd, buf.baseAddress, buf.count - 1) // without the trailing NUL
        }
        guard sent == head.utf8.count else { throw posixFailure("write") }

        var response = Data()
        var chunk = [UInt8](repeating: 0, count: 16 * 1024)
        while true {
            let n = read(fd, &chunk, chunk.count)
            if n < 0 { throw posixFailure("read") }
            if n == 0 { break } // HTTP/1.0: the server closes after the response
            response.append(contentsOf: chunk[0..<n])
        }
        return try parse(response)
    }

    private func parse(_ response: Data) throws -> (Int, Data) {
        guard let split = response.firstRange(of: Data("\r\n\r\n".utf8)) else {
            throw Failure(description: "malformed response")
        }
        let head = String(decoding: response[..<split.lowerBound], as: UTF8.self)
        let statusLine = head.split(separator: "\r\n", maxSplits: 1).first ?? ""
        let parts = statusLine.split(separator: " ")
        guard parts.count >= 2, let status = Int(parts[1]) else {
            throw Failure(description: "malformed status line: \(statusLine)")
        }
        return (status, Data(response[split.upperBound...]))
    }

    private func posixFailure(_ call: String) -> Failure {
        Failure(description: "\(call): \(String(cString: strerror(errno)))")
    }
}
