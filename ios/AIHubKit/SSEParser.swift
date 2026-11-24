import Foundation

/// One server-sent event.
public struct SSEEvent: Sendable, Equatable {
    public var event: String
    public var data: String

    public init(event: String, data: String) {
        self.event = event
        self.data = data
    }
}

/// Incremental text/event-stream parser. Feed it bytes as they arrive;
/// it returns complete events. Comment lines (": ping") are skipped,
/// "\r\n" and "\n" both end a line, an empty line ends an event.
public struct SSEParser: Sendable {
    private var line = [UInt8]()
    private var event = ""
    private var data = [String]()

    public init() {}

    public mutating func push(_ byte: UInt8) -> SSEEvent? {
        if byte == UInt8(ascii: "\n") {
            var bytes = line
            line.removeAll(keepingCapacity: true)
            if bytes.last == UInt8(ascii: "\r") { bytes.removeLast() }
            return take(line: String(decoding: bytes, as: UTF8.self))
        }
        line.append(byte)
        return nil
    }

    public mutating func push(_ chunk: some Sequence<UInt8>) -> [SSEEvent] {
        var out = [SSEEvent]()
        for b in chunk {
            if let e = push(b) { out.append(e) }
        }
        return out
    }

    /// Ends the stream: an event without its closing empty line still counts.
    public mutating func finish() -> SSEEvent? {
        if !line.isEmpty {
            let rest = String(decoding: line, as: UTF8.self)
            line.removeAll()
            if let e = take(line: rest) { return e }
        }
        return dispatch()
    }

    private mutating func take(line: String) -> SSEEvent? {
        if line.isEmpty { return dispatch() }
        if line.hasPrefix(":") { return nil }
        let field: Substring
        var value: Substring
        if let colon = line.firstIndex(of: ":") {
            field = line[..<colon]
            value = line[line.index(after: colon)...]
            if value.hasPrefix(" ") { value = value.dropFirst() }
        } else {
            field = Substring(line)
            value = ""
        }
        switch field {
        case "event": event = String(value)
        case "data": data.append(String(value))
        default: break
        }
        return nil
    }

    private mutating func dispatch() -> SSEEvent? {
        defer {
            event = ""
            data.removeAll()
        }
        guard !data.isEmpty else { return nil }
        return SSEEvent(event: event.isEmpty ? "message" : event, data: data.joined(separator: "\n"))
    }
}

/// The chat stream's events (POST /v1/chat).
public enum ChatEvent: Sendable, Equatable {
    case start(conversationId: Int64, userMessageId: Int64)
    case delta(String)
    case usage(Usage)
    case done(conversationId: Int64, messageId: Int64, stopped: Bool)
    case error(code: String, message: String)

    public init?(_ e: SSEEvent) {
        let data = Data(e.data.utf8)
        let dec = JSONCoding.decoder
        switch e.event {
        case "start":
            struct S: Decodable { var conversationId: Int64; var userMessageId: Int64 }
            guard let s = try? dec.decode(S.self, from: data) else { return nil }
            self = .start(conversationId: s.conversationId, userMessageId: s.userMessageId)
        case "delta":
            struct D: Decodable { var text: String }
            guard let d = try? dec.decode(D.self, from: data) else { return nil }
            self = .delta(d.text)
        case "usage":
            guard let u = try? dec.decode(Usage.self, from: data) else { return nil }
            self = .usage(u)
        case "done":
            struct D: Decodable { var conversationId: Int64; var messageId: Int64; var stopped: Bool }
            guard let d = try? dec.decode(D.self, from: data) else { return nil }
            self = .done(conversationId: d.conversationId, messageId: d.messageId, stopped: d.stopped)
        case "error":
            guard let x = try? dec.decode(APIErrorBody.Inner.self, from: data) else { return nil }
            self = .error(code: x.code, message: x.message)
        default:
            return nil
        }
    }
}
