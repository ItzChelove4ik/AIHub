import SwiftUI
import UIKit

/// Renders an answer: Markdown text with inline styles, and fenced code
/// blocks in a monospaced box with a copy button.
struct MarkdownView: View {
    var text: String

    enum Block: Identifiable {
        case prose(Int, String)
        case code(Int, language: String, body: String)
        var id: Int {
            switch self {
            case .prose(let i, _), .code(let i, _, _): i
            }
        }
    }

    static func blocks(_ text: String) -> [Block] {
        var out: [Block] = []
        var prose: [Substring] = []
        var code: [Substring] = []
        var language = ""
        var inCode = false
        func flushProse() {
            let s = prose.joined(separator: "\n").trimmingCharacters(in: .newlines)
            if !s.isEmpty { out.append(.prose(out.count, s)) }
            prose = []
        }
        for line in text.split(separator: "\n", omittingEmptySubsequences: false) {
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("```") {
                if inCode {
                    out.append(.code(out.count, language: language, body: code.joined(separator: "\n")))
                    code = []
                    inCode = false
                } else {
                    flushProse()
                    language = String(trimmed.dropFirst(3)).trimmingCharacters(in: .whitespaces)
                    inCode = true
                }
                continue
            }
            if inCode { code.append(line) } else { prose.append(line) }
        }
        if inCode { out.append(.code(out.count, language: language, body: code.joined(separator: "\n"))) }
        flushProse()
        return out
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            ForEach(Self.blocks(text)) { block in
                switch block {
                case .prose(_, let s):
                    Text(Self.attributed(s))
                        .textSelection(.enabled)
                        .frame(maxWidth: .infinity, alignment: .leading)
                case .code(_, let language, let body):
                    CodeBlock(language: language, code: body)
                }
            }
        }
    }

    static func attributed(_ s: String) -> AttributedString {
        let opts = AttributedString.MarkdownParsingOptions(interpretedSyntax: .inlineOnlyPreservingWhitespace,
                                                           failurePolicy: .returnPartiallyParsedIfPossible)
        return (try? AttributedString(markdown: s, options: opts)) ?? AttributedString(s)
    }
}

struct CodeBlock: View {
    var language: String
    var code: String
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text(language.isEmpty ? "code" : language).font(.caption.monospaced()).foregroundStyle(.secondary)
                Spacer()
                Button {
                    UIPasteboard.general.string = code
                    copied = true
                    Task {
                        try? await Task.sleep(for: .seconds(1.5))
                        copied = false
                    }
                } label: {
                    Label(copied ? "Скопировано" : "Копировать", systemImage: copied ? "checkmark" : "doc.on.doc")
                        .font(.caption)
                }
                .buttonStyle(.borderless)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 6)
            ScrollView(.horizontal, showsIndicators: false) {
                Text(code)
                    .font(.system(.footnote, design: .monospaced))
                    .textSelection(.enabled)
                    .padding(12)
            }
        }
        .background(.quaternary.opacity(0.5), in: .rect(cornerRadius: 12))
    }
}
