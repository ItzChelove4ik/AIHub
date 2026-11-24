package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
)

const testKey = "sk-test-secret-key-1234"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeProvider serves recorded responses and checks the auth header.
func fakeProvider(t *testing.T, authHeader, authValue string, routes map[string]string, captured *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get(authHeader); got != authValue {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"invalid key"}}`)
			return
		}
		if r.Method == http.MethodPost && captured != nil {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			*captured = body
		}
		// Longest prefix first: "/models/x:stream..." must not match "/models".
		prefixes := make([]string, 0, len(routes))
		for p := range routes {
			prefixes = append(prefixes, p)
		}
		sort.Slice(prefixes, func(i, j int) bool { return len(prefixes[i]) > len(prefixes[j]) })
		for _, prefix := range prefixes {
			file := routes[prefix]
			if strings.HasPrefix(r.URL.Path, prefix) {
				if strings.HasSuffix(file, ".sse") {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.Write(fixture(t, file))
				return
			}
		}
		w.WriteHeader(404)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func collect(t *testing.T, a Adapter, c Credential, req ChatRequest) (string, Usage, error) {
	t.Helper()
	var b strings.Builder
	u, err := a.StreamChat(context.Background(), c, req, func(d string) error { b.WriteString(d); return nil })
	return b.String(), u, err
}

func TestReadSSE(t *testing.T) {
	in := ": comment\nevent: a\ndata: one\ndata: two\n\ndata: three\r\n\r\n"
	var got []string
	err := ReadSSE(strings.NewReader(in), func(ev, data string) error {
		got = append(got, ev+"|"+data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a|one\ntwo" || got[1] != "|three" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenAIStreamAndModels(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "Authorization", "Bearer "+testKey,
		map[string]string{"/chat/completions": "openai_stream.sse", "/models": "openai_models.json"}, &body)
	spec := Specs["openai"]
	spec.BaseURL = srv.URL
	c := Credential{Spec: spec, Key: testKey}
	a := &OpenAI{c: NewClients(true)}

	ms, err := a.ListModels(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 { // the embedding model is filtered out
		t.Fatalf("models: %+v", ms)
	}
	if ms[1].ID != "vendor/model-x" || ms[1].Context == nil || *ms[1].Context != 131072 || ms[1].Vision == nil || !*ms[1].Vision || ms[1].MaxOutput != 8192 {
		t.Fatalf("openrouter-style model: %+v", ms[1])
	}

	temp := 0.5
	text, u, err := collect(t, a, c, ChatRequest{Model: "gpt-test-1", System: "be brief", Temperature: &temp,
		Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello!" || u.InputTokens != 12 || u.OutputTokens != 3 {
		t.Fatalf("text %q usage %+v", text, u)
	}
	msgs := body["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" || body["stream"] != true || body["stream_options"] == nil || body["temperature"] != 0.5 {
		t.Fatalf("request body: %v", body)
	}
}

func TestOpenAIBadKey(t *testing.T) {
	srv := fakeProvider(t, "Authorization", "Bearer "+testKey, map[string]string{"/models": "openai_models.json"}, nil)
	spec := Specs["openai"]
	spec.BaseURL = srv.URL
	_, err := (&OpenAI{c: NewClients(true)}).ListModels(context.Background(), Credential{Spec: spec, Key: "wrong"})
	var pe *Error
	if !errors.As(err, &pe) || !pe.Unauthorized() || pe.Message != "invalid key" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Fatal("error leaks the key")
	}
}

func TestAnthropicStreamAndModels(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "x-api-key", testKey,
		map[string]string{"/messages": "anthropic_stream.sse", "/models": "anthropic_models.json"}, &body)
	spec := Specs["anthropic"]
	spec.BaseURL = srv.URL
	c := Credential{Spec: spec, Key: testKey}
	a := &Anthropic{c: NewClients(true)}

	ms, err := a.ListModels(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].DisplayName != "Claude Test" || *ms[0].Context != 200000 || !*ms[0].Vision || ms[0].MaxOutput != 64000 {
		t.Fatalf("models: %+v", ms)
	}
	text, u, err := collect(t, a, c, ChatRequest{Model: "claude-test", System: "sys", Messages: []Message{
		{Role: "assistant", Content: "dropped"}, {Role: "user", Content: "a"}, {Role: "user", Content: "b"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hello!" || u.InputTokens != 25 || u.OutputTokens != 15 {
		t.Fatalf("text %q usage %+v", text, u)
	}
	msgs := body["messages"].([]any)
	blocks := msgs[0].(map[string]any)["content"].([]any)
	if len(msgs) != 1 || len(blocks) != 2 || blocks[1].(map[string]any)["text"] != "b" || body["system"] != "sys" || body["max_tokens"] == nil {
		t.Fatalf("request body: %v", body)
	}
}

func TestAnthropicStreamError(t *testing.T) {
	srv := fakeProvider(t, "x-api-key", testKey, map[string]string{"/messages": "anthropic_error.sse"}, nil)
	spec := Specs["anthropic"]
	spec.BaseURL = srv.URL
	text, _, err := collect(t, &Anthropic{c: NewClients(true)}, Credential{Spec: spec, Key: testKey},
		ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	var pe *Error
	if !errors.As(err, &pe) || pe.Message != "Overloaded" || text != "Par" {
		t.Fatalf("err %v text %q", err, text)
	}
}

func TestGeminiStreamAndModels(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "x-goog-api-key", testKey,
		map[string]string{"/models/gemini-test:streamGenerateContent": "gemini_stream.sse", "/models": "gemini_models.json"}, &body)
	spec := Specs["gemini"]
	spec.BaseURL = srv.URL
	c := Credential{Spec: spec, Key: testKey}
	g := &Gemini{c: NewClients(true)}

	ms, err := g.ListModels(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != "gemini-test" || *ms[0].Context != 1048576 {
		t.Fatalf("models: %+v", ms)
	}
	text, u, err := collect(t, g, c, ChatRequest{Model: "gemini-test", System: "sys", Messages: []Message{
		{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}, {Role: "user", Content: "q2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "Hi there" || u.InputTokens != 8 || u.OutputTokens != 6 {
		t.Fatalf("text %q usage %+v", text, u)
	}
	contents := body["contents"].([]any)
	if contents[1].(map[string]any)["role"] != "model" || body["systemInstruction"] == nil {
		t.Fatalf("request body: %v", body)
	}
}

// A 1x1 PNG.
const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

func imageRequest() ChatRequest {
	return ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "what is this?",
		Attachments: []Attachment{{Type: "image", MediaType: "image/png", Data: pngB64}}}}}
}

func TestOpenAIImage(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "Authorization", "Bearer "+testKey, map[string]string{"/chat/completions": "openai_stream.sse"}, &body)
	spec := Specs["openai"]
	spec.BaseURL = srv.URL
	if _, _, err := collect(t, &OpenAI{c: NewClients(true)}, Credential{Spec: spec, Key: testKey}, imageRequest()); err != nil {
		t.Fatal(err)
	}
	parts := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := parts[1].(map[string]any)
	if parts[0].(map[string]any)["text"] != "what is this?" || img["type"] != "image_url" ||
		img["image_url"].(map[string]any)["url"] != "data:image/png;base64,"+pngB64 {
		t.Fatalf("parts: %v", parts)
	}
}

func TestAnthropicImage(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "x-api-key", testKey, map[string]string{"/messages": "anthropic_stream.sse"}, &body)
	spec := Specs["anthropic"]
	spec.BaseURL = srv.URL
	if _, _, err := collect(t, &Anthropic{c: NewClients(true)}, Credential{Spec: spec, Key: testKey}, imageRequest()); err != nil {
		t.Fatal(err)
	}
	blocks := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	img := blocks[0].(map[string]any)
	src := img["source"].(map[string]any)
	if img["type"] != "image" || src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != pngB64 ||
		blocks[1].(map[string]any)["text"] != "what is this?" {
		t.Fatalf("blocks: %v", blocks)
	}
}

func TestGeminiImage(t *testing.T) {
	var body map[string]any
	srv := fakeProvider(t, "x-goog-api-key", testKey, map[string]string{"/models/m:streamGenerateContent": "gemini_stream.sse"}, &body)
	spec := Specs["gemini"]
	spec.BaseURL = srv.URL
	if _, _, err := collect(t, &Gemini{c: NewClients(true)}, Credential{Spec: spec, Key: testKey}, imageRequest()); err != nil {
		t.Fatal(err)
	}
	parts := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	inline := parts[0].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/png" || inline["data"] != pngB64 || parts[1].(map[string]any)["text"] != "what is this?" {
		t.Fatalf("parts: %v", parts)
	}
}

func TestValidateAttachment(t *testing.T) {
	if raw, err := ValidateAttachment(Attachment{Type: "image", MediaType: "image/png", Data: pngB64}); err != nil || len(raw) == 0 {
		t.Fatalf("valid png: %v", err)
	}
	bad := []Attachment{
		{Type: "file", MediaType: "image/png", Data: pngB64},
		{Type: "image", MediaType: "image/svg+xml", Data: pngB64},
		{Type: "image", MediaType: "image/jpeg", Data: pngB64}, // says jpeg, is png
		{Type: "image", MediaType: "image/png", Data: "%%%"},
		{Type: "image", MediaType: "image/png", Data: ""},
	}
	for i, a := range bad {
		if _, err := ValidateAttachment(a); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	big := strings.Repeat("A", (MaxImageBytes/3+10)*4)
	if _, err := ValidateAttachment(Attachment{Type: "image", MediaType: "image/png", Data: big}); err == nil {
		t.Error("oversized image accepted")
	}
}

func TestAttachmentsRejectedByAdapters(t *testing.T) {
	for _, a := range []Adapter{&OpenAI{}, &Anthropic{}, &Gemini{}} {
		_, err := a.StreamChat(context.Background(), Credential{Spec: Specs["openai"]}, ChatRequest{
			Messages: []Message{{Role: "user", Content: "x", Attachments: []Attachment{{Type: "pdf"}}}}}, func(string) error { return nil })
		if !errors.Is(err, ErrAttachmentsUnsupported) {
			t.Fatalf("%T: %v", a, err)
		}
	}
}

func TestCustomProviderBlocksPrivateAddresses(t *testing.T) {
	srv := fakeProvider(t, "Authorization", "Bearer "+testKey, map[string]string{"/models": "openai_models.json"}, nil)
	c := Credential{Spec: Specs["custom"], Key: testKey, BaseURL: srv.URL}
	_, err := (&OpenAI{c: NewClients(false)}).ListModels(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("loopback custom provider should be refused, got %v", err)
	}
}

func TestValidateBaseURL(t *testing.T) {
	if _, err := ValidateBaseURL("http://example.com/v1", false); err == nil {
		t.Fatal("http should need allowHTTP")
	}
	if u, err := ValidateBaseURL(" https://example.com/v1/ ", false); err != nil || u != "https://example.com/v1" {
		t.Fatalf("%q %v", u, err)
	}
	if _, err := ValidateBaseURL("https://user:pw@example.com", false); err == nil {
		t.Fatal("userinfo should be refused")
	}
}
