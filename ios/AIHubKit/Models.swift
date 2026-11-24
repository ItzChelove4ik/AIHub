import Foundation

// Models of docs/api.yaml. JSON keys are snake_case on the wire; the coders
// in JSONCoding convert them.

public struct User: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var email: String
    public var isAdmin: Bool
    public var createdAt: Date
}

public struct Tokens: Codable, Sendable, Hashable {
    public var accessToken: String
    public var refreshToken: String
    public var expiresAt: Date
    public var tokenType: String
}

public struct AuthResponse: Codable, Sendable {
    public var user: User
    public var tokens: Tokens
}

public struct MeResponse: Codable, Sendable {
    public var user: User
    public var deviceId: Int64
}

public struct Device: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var deviceId: String
    public var name: String
    public var createdAt: Date
    public var lastSeenAt: Date
    public var revokedAt: Date?
    public var current: Bool
}

public struct ProviderSpec: Codable, Sendable, Hashable, Identifiable {
    public var id: String
    public var name: String
    public var kind: String
    public var baseUrl: String?
    public var custom: Bool?
    public var oauth: Bool?
}

public struct Connection: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    /// Slug models and chats refer to ("openai", "custom-mylab").
    public var provider: String
    public var type: String
    public var name: String
    public var label: String
    public var baseUrl: String?
    public var kind: String
    public var last4: String
    public var createdAt: Date
    public var updatedAt: Date
}

public struct AIModel: Codable, Sendable, Hashable, Identifiable {
    public var provider: String
    public var id: String
    public var displayName: String
    public var context: Int?
    public var vision: Bool?
    /// Unique across providers.
    public var key: String { provider + "/" + id }

    public init(provider: String, id: String, displayName: String, context: Int? = nil, vision: Bool? = nil) {
        self.provider = provider
        self.id = id
        self.displayName = displayName
        self.context = context
        self.vision = vision
    }
}

public struct ProviderError: Codable, Sendable, Hashable {
    public var provider: String
    public var error: String
}

public struct ModelsResponse: Codable, Sendable {
    public var models: [AIModel]
    public var errors: [ProviderError]
}

public struct AttachmentInput: Codable, Sendable, Hashable {
    public var type: String = "image"
    public var mediaType: String
    /// Standard base64.
    public var data: String

    public init(mediaType: String, data: String) {
        self.mediaType = mediaType
        self.data = data
    }
}

public struct ChatMessageInput: Codable, Sendable, Hashable {
    public var role: String
    public var content: String
    public var attachments: [AttachmentInput]?

    public init(role: String, content: String, attachments: [AttachmentInput]? = nil) {
        self.role = role
        self.content = content
        self.attachments = attachments
    }
}

public struct ChatRequest: Codable, Sendable {
    public var conversationId: Int64?
    public var provider: String
    public var model: String
    public var messages: [ChatMessageInput]
    public var system: String?
    public var temperature: Double?

    public init(conversationId: Int64?, provider: String, model: String, messages: [ChatMessageInput],
                system: String? = nil, temperature: Double? = nil) {
        self.conversationId = conversationId
        self.provider = provider
        self.model = model
        self.messages = messages
        self.system = system
        self.temperature = temperature
    }
}

public struct Conversation: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var title: String
    public var provider: String
    public var model: String
    public var system: String
    public var createdAt: Date
    public var updatedAt: Date
}

public struct AttachmentInfo: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var type: String
    public var mediaType: String
    public var size: Int
    public var url: String
}

public struct Message: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var role: String
    public var content: String
    public var attachments: [AttachmentInfo]?
    public var provider: String?
    public var model: String?
    public var inputTokens: Int64?
    public var outputTokens: Int64?
    public var status: String
    public var createdAt: Date
}

public struct ConversationsResponse: Codable, Sendable {
    public var conversations: [Conversation]
}

public struct ConversationDetail: Codable, Sendable {
    public var conversation: Conversation
    public var messages: [Message]
}

public struct Usage: Codable, Sendable, Hashable {
    public var inputTokens: Int64
    public var outputTokens: Int64
}

public struct APIKeyInfo: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var name: String
    public var prefix: String
    public var scopes: [String]
    public var rpm: Int
    public var lastUsedAt: Date?
    public var createdAt: Date
    public var revokedAt: Date?
}

public struct CreatedAPIKey: Codable, Sendable {
    public var key: String
    public var info: APIKeyInfo
}

public struct Channel: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var channel: String
    public var port: Int
    public var transports: [String]
    public var userId: Int64?
    public var deviceId: Int64?
    public var invite: Bool
    public var state: String
    public var createdAt: Date
}

public struct IssuedChannel: Codable, Sendable, Identifiable {
    public var id: Int64
    public var channel: String
    public var port: Int
    public var transports: [String]
    public var state: String
    /// openflux:// link with the channel key: store it in the Keychain only.
    public var link: String
    /// PNG of the link's QR code.
    public var qrPng: Data
    public var inviteCode: String?
}

public struct ChannelsResponse: Codable, Sendable {
    public var channels: [Channel]
    public var enabled: Bool
}

public struct Carrier: Codable, Sendable, Hashable, Identifiable {
    public var id: Int64
    public var type: String
    public var hint: String
    public var status: String
    public var channelId: Int64?
    public var createdAt: Date
}

public struct ReadyStatus: Codable, Sendable {
    public var status: String
    public var version: String?
    public var tunnel: Bool?
}

public struct UsageRow: Codable, Sendable, Hashable {
    public var provider: String
    public var day: String
    public var requests: Int64
    public var inputTokens: Int64
    public var outputTokens: Int64
}

public struct Limits: Codable, Sendable, Hashable {
    public var dailyRequests: Int64
    public var dailyTokens: Int64
}

public struct UsageResponse: Codable, Sendable {
    public var usage: [UsageRow]
    public var limits: Limits
}

public struct APIErrorBody: Codable, Sendable {
    public struct Inner: Codable, Sendable {
        public var code: String
        public var message: String
    }
    public var error: Inner
}

public enum JSONCoding {
    public static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        d.dateDecodingStrategy = .custom { decoder in
            let c = try decoder.singleValueContainer()
            let s = try c.decode(String.self)
            if let date = parseDate(s) { return date }
            throw DecodingError.dataCorruptedError(in: c, debugDescription: "bad date \(s)")
        }
        return d
    }()

    public static let encoder: JSONEncoder = {
        let e = JSONEncoder()
        e.keyEncodingStrategy = .convertToSnakeCase
        e.dateEncodingStrategy = .iso8601
        return e
    }()

    /// Go writes RFC 3339 with a variable number of fractional digits.
    static func parseDate(_ s: String) -> Date? {
        let withFraction = Date.ISO8601FormatStyle(includingFractionalSeconds: true)
        if let d = try? withFraction.parse(s) { return d }
        if let d = try? Date.ISO8601FormatStyle().parse(s) { return d }
        // Trim nanoseconds to milliseconds and try again.
        if let dot = s.firstIndex(of: "."), let end = s[dot...].firstIndex(where: { !$0.isNumber && $0 != "." }) {
            let digits = s[s.index(after: dot)..<end].prefix(3)
            let trimmed = String(s[..<dot]) + "." + digits + String(s[end...])
            if let d = try? withFraction.parse(trimmed) { return d }
        }
        return nil
    }
}
