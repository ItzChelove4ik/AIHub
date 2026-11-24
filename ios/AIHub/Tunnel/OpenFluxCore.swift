import Foundation

/// Thin Swift layer over liboflux's C functions: strings go in as C strings
/// the core does not keep, strings that come back are freed with
/// OpenFluxFreeString.
enum OpenFluxCore {
    /// Passes a Swift string as `char *` for the duration of the call.
    private static func withC<T>(_ s: String, _ body: (UnsafeMutablePointer<CChar>) -> T) -> T {
        let p = strdup(s)!
        defer { free(p) }
        return body(p)
    }

    /// Takes ownership of a string the core returned.
    private static func take(_ p: UnsafeMutablePointer<CChar>?) -> String {
        guard let p else { return "" }
        defer { OpenFluxFreeString(p) }
        return String(cString: p)
    }

    static func shareDecode(_ link: String) -> String { withC(link) { take(OpenFluxShareDecode($0)) } }

    static func startSession(specs: String, secret: String, socksAddr: String) -> Int32 {
        withC(specs) { s in withC(secret) { k in withC(socksAddr) { a in Int32(OpenFluxStartSession(s, k, a)) } } }
    }

    static func stop() { OpenFluxStop() }
    static var isRunning: Bool { OpenFluxIsRunning() != 0 }
    static var isConnected: Bool { OpenFluxIsConnected() != 0 }
    static func statsJSON() -> String { take(OpenFluxStatsJSON()) }
    static func activeTransport() -> String { take(OpenFluxActiveTransport()) }
    static func readLog() -> String { take(OpenFluxReadLog()) }
    static func setDebugLevel(_ n: Int) { OpenFluxSetDebugLevel(Int32(n)) }
    static func setDoTResolver(_ spec: String) { withC(spec) { OpenFluxSetDoTResolver($0) } }

    static func captchaPending() -> String { take(OpenFluxCaptchaPending()) }
    static func remoteCaptchaPending() -> String { take(OpenFluxRemoteCaptchaPending()) }
    static func remoteCaptchaProxy() -> String { take(OpenFluxRemoteCaptchaProxy()) }
    static func captchaReason() -> String { take(OpenFluxCaptchaReason()) }
    static func applyCaptchaCookies(_ header: String) -> Int { withC(header) { Int(OpenFluxApplyCaptchaCookies($0)) } }
    static func cancelCaptcha() { OpenFluxCancelCaptcha() }
    static func setCookieStore(_ path: String) -> Bool { withC(path) { OpenFluxSetCookieStore($0) != 0 } }
}
