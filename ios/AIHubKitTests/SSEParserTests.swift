import XCTest
@testable import AIHubKit

final class SSEParserTests: XCTestCase {
    func testEventsCommentsAndCRLF() {
        var p = SSEParser()
        let input = ": ping\n\nevent: start\ndata: {\"conversation_id\":7,\"user_message_id\":9}\n\nevent: delta\r\ndata: {\"text\":\"Hel\"}\r\n\r\ndata: plain\n\n"
        let events = p.push(Array(input.utf8))
        XCTAssertEqual(events.map(\.event), ["start", "delta", "message"])
        XCTAssertEqual(events[2].data, "plain")
        XCTAssertEqual(ChatEvent(events[0]), .start(conversationId: 7, userMessageId: 9))
        XCTAssertEqual(ChatEvent(events[1]), .delta("Hel"))
    }

    func testSplitAcrossChunksAndMultilineData() {
        var p = SSEParser()
        var got = [SSEEvent]()
        for chunk in ["eve", "nt: delta\nda", "ta: {\"text\":", "\"Привет\"}\n", "\n", "data: a\ndata: b\n\n"] {
            got += p.push(Array(chunk.utf8))
        }
        XCTAssertEqual(got.count, 2)
        XCTAssertEqual(ChatEvent(got[0]), .delta("Привет"))
        XCTAssertEqual(got[1].data, "a\nb")
    }

    func testFinishFlushesLastEvent() {
        var p = SSEParser()
        XCTAssertTrue(p.push(Array("event: done\ndata: {\"conversation_id\":1,\"message_id\":2,\"stopped\":false}".utf8)).isEmpty)
        let last = p.finish()
        XCTAssertEqual(last.flatMap(ChatEvent.init), .done(conversationId: 1, messageId: 2, stopped: false))
    }

    func testUsageAndError() {
        var p = SSEParser()
        let e = p.push(Array("event: usage\ndata: {\"input_tokens\":3,\"output_tokens\":4}\n\nevent: error\ndata: {\"code\":\"provider_error\",\"message\":\"boom\"}\n\n".utf8))
        XCTAssertEqual(ChatEvent(e[0]), .usage(Usage(inputTokens: 3, outputTokens: 4)))
        XCTAssertEqual(ChatEvent(e[1]), .error(code: "provider_error", message: "boom"))
    }
}
