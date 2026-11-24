package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/apikeys"
	"github.com/ItzChelove4ik/AIHub/backend/internal/auth"
	"github.com/ItzChelove4ik/AIHub/backend/internal/chat"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store/sqlite"
	"github.com/ItzChelove4ik/AIHub/backend/internal/tunnel"
)

const providerKey = "sk-fake-provider-key-9876"

const pngB64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="

var lastProviderBody atomic.Value

// fakeOpenAI is an OpenAI-compatible provider that answers "Hello world".
func fakeOpenAI(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+providerKey {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":{"message":"bad key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"fake-model"}]}`)
		case "/v1/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			lastProviderBody.Store(string(raw))
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []string{"Hello", " world"} {
				io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+part+`"}}]}`+"\n\n")
			}
			io.WriteString(w, `data: {"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`+"\n\ndata: [DONE]\n\n")
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	t   *testing.T
	srv *httptest.Server
	db  *sqlite.DB
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	master := bytes.Repeat([]byte{5}, 32)
	box, _ := secure.NewBox(master)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	clients := providers.NewClients(true)
	conn := connections.New(db, box, providers.NewRegistry(clients), true)
	a := auth.New(db, master, "", "open")
	tun := tunnel.New(db, box, nil, tunnel.Options{}, log)
	a.SetInviteBinder(tun)
	s := &Server{Store: db, Auth: a, Conn: conn, OAuth: connections.NewOpenRouterOAuth(conn, "http://localhost", clients.Trusted),
		Chat: chat.New(db, conn, log, 0, 0), APIKeys: apikeys.New(db), Tunnel: tun, Log: log, Version: "test"}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, db: db}
}

func (h *harness) do(method, path, token string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func (h *harness) json(method, path, token string, body any, want int, out any) {
	h.t.Helper()
	resp, raw := h.do(method, path, token, body)
	if resp.StatusCode != want {
		h.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			h.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

type sseEvent struct{ name, data string }

func readEvents(t *testing.T, body io.Reader) []sseEvent {
	t.Helper()
	var out []sseEvent
	err := providers.ReadSSE(bufio.NewReader(body), func(ev, data string) error {
		out = append(out, sseEvent{ev, data})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t)
	prov := fakeOpenAI(t)

	h.json("GET", "/healthz", "", nil, 200, nil)
	h.json("GET", "/readyz", "", nil, 200, nil)

	// Register → the first account is admin.
	var reg struct {
		User struct {
			IsAdmin bool `json:"is_admin"`
		} `json:"user"`
		Tokens auth.Tokens `json:"tokens"`
	}
	h.json("POST", "/v1/auth/register", "", map[string]string{"email": "me@example.com", "password": "a-long-password-1",
		"device_id": "iphone-1", "device_name": "iPhone"}, 201, &reg)
	if !reg.User.IsAdmin {
		t.Fatal("first user should be admin")
	}
	tok := reg.Tokens.AccessToken
	h.json("GET", "/v1/me", "", nil, 401, nil)
	h.json("POST", "/v1/chat", tok, map[string]any{"provider": "openai", "model": "m",
		"messages": []map[string]string{{"role": "user", "content": "hi"}}}, 400, nil)

	// Connect a provider: the key is checked, stored, and never shown again.
	h.json("POST", "/v1/connections", tok, map[string]string{"provider": "custom", "api_key": "wrong-key", "label": "Lab",
		"base_url": prov.URL + "/v1"}, 400, nil)
	var conn connections.View
	h.json("POST", "/v1/connections", tok, map[string]string{"provider": "custom", "api_key": providerKey, "label": "Lab",
		"base_url": prov.URL + "/v1"}, 201, &conn)
	if conn.Provider != "custom-lab" || conn.Last4 != "9876" {
		t.Fatalf("connection: %+v", conn)
	}
	_, raw := h.do("GET", "/v1/connections", tok, nil)
	if strings.Contains(string(raw), providerKey) {
		t.Fatal("key leaked in the connection list")
	}

	var models struct {
		Models []providers.Model `json:"models"`
	}
	h.json("GET", "/v1/models", tok, nil, 200, &models)
	if len(models.Models) != 1 || models.Models[0].Provider != "custom-lab" || models.Models[0].ID != "fake-model" {
		t.Fatalf("models: %+v", models)
	}

	// Stream a chat.
	raw, _ = json.Marshal(map[string]any{"provider": "custom-lab", "model": "fake-model",
		"messages": []map[string]string{{"role": "user", "content": "Say hello"}}})
	req, _ := http.NewRequest("POST", h.srv.URL+"/v1/chat", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q", ct)
	}
	events := readEvents(t, resp.Body)
	resp.Body.Close()
	var names []string
	var text strings.Builder
	var convID int64
	for _, e := range events {
		names = append(names, e.name)
		switch e.name {
		case "delta":
			var d struct{ Text string }
			json.Unmarshal([]byte(e.data), &d)
			text.WriteString(d.Text)
		case "done":
			var d struct {
				ConversationID int64 `json:"conversation_id"`
			}
			json.Unmarshal([]byte(e.data), &d)
			convID = d.ConversationID
		}
	}
	if strings.Join(names, ",") != "start,delta,delta,usage,done" || text.String() != "Hello world" || convID == 0 {
		t.Fatalf("events %v text %q conv %d", names, text.String(), convID)
	}

	// History is on the server; a follow-up goes into the same conversation.
	var conv struct {
		Messages []struct{ Role, Content string } `json:"messages"`
	}
	h.json("GET", "/v1/conversations/"+itoa(convID), tok, nil, 200, &conv)
	if len(conv.Messages) != 2 || conv.Messages[1].Content != "Hello world" {
		t.Fatalf("conversation: %+v", conv)
	}
	var list struct {
		Conversations []struct{ ID int64 } `json:"conversations"`
	}
	h.json("GET", "/v1/conversations?q=hello", tok, nil, 200, &list)
	if len(list.Conversations) != 1 {
		t.Fatalf("search: %+v", list)
	}
	h.json("GET", "/v1/conversations?q=nothing-like-this", tok, nil, 200, &list)
	if len(list.Conversations) != 0 {
		t.Fatalf("search miss: %+v", list)
	}

	// Usage was counted.
	var usage struct {
		Usage []struct {
			Requests     int64 `json:"requests"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	waitFor(t, func() bool {
		h.json("GET", "/v1/usage", tok, nil, 200, &usage)
		return len(usage.Usage) == 1 && usage.Usage[0].Requests >= 1
	})

	// AIHub API key → OpenAI-compatible API.
	var created struct {
		Key  string       `json:"key"`
		Info apikeys.View `json:"info"`
	}
	h.json("POST", "/v1/apikeys", tok, map[string]any{"name": "cli"}, 201, &created)
	if !strings.HasPrefix(created.Key, "ahk_live_") {
		t.Fatalf("key %q", created.Key)
	}
	_, raw = h.do("GET", "/v1/apikeys", tok, nil)
	if strings.Contains(string(raw), created.Key) {
		t.Fatal("full AIHub key shown again")
	}
	var oaiModels struct {
		Data []struct{ ID string } `json:"data"`
	}
	h.json("GET", "/openai/v1/models", created.Key, nil, 200, &oaiModels)
	if len(oaiModels.Data) != 1 || oaiModels.Data[0].ID != "custom-lab/fake-model" {
		t.Fatalf("openai models: %+v", oaiModels)
	}
	var completion struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	h.json("POST", "/openai/v1/chat/completions", created.Key, map[string]any{"model": "custom-lab/fake-model",
		"messages": []map[string]any{{"role": "system", "content": "be nice"}, {"role": "user", "content": []map[string]string{{"type": "text", "text": "hi"}}}}},
		200, &completion)
	if completion.Choices[0].Message.Content != "Hello world" || completion.Usage.TotalTokens != 9 {
		t.Fatalf("completion: %+v", completion)
	}
	resp, raw = h.do("POST", "/openai/v1/chat/completions", created.Key, map[string]any{"model": "custom-lab/fake-model", "stream": true,
		"stream_options": map[string]bool{"include_usage": true}, "messages": []map[string]string{{"role": "user", "content": "hi"}}})
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"content":"Hello"`) || !strings.HasSuffix(strings.TrimSpace(string(raw)), "data: [DONE]") ||
		!strings.Contains(string(raw), `"total_tokens":9`) {
		t.Fatalf("stream: %d %s", resp.StatusCode, raw)
	}
	h.json("POST", "/openai/v1/chat/completions", "ahk_live_wrong", map[string]any{"model": "x/y"}, 401, nil)
	h.json("DELETE", "/v1/apikeys/"+itoa(created.Info.ID), tok, nil, 204, nil)
	h.json("GET", "/openai/v1/models", created.Key, nil, 401, nil)

	// Admin limits: one request a day, already used.
	var me struct {
		User struct{ ID int64 } `json:"user"`
	}
	h.json("GET", "/v1/me", tok, nil, 200, &me)
	h.json("PUT", "/v1/admin/users/"+itoa(me.User.ID)+"/limits", tok, map[string]any{"daily_requests": 1}, 200, nil)
	h.json("POST", "/v1/chat", tok, map[string]any{"provider": "custom-lab", "model": "fake-model",
		"messages": []map[string]string{{"role": "user", "content": "again"}}}, 429, nil)

	// Tunnel is off in this setup.
	h.json("POST", "/v1/tunnel/channels", tok, nil, 503, nil)

	// A second user is not admin; refresh and logout work.
	var reg2 struct {
		Tokens auth.Tokens `json:"tokens"`
	}
	h.json("POST", "/v1/auth/register", "", map[string]string{"email": "two@example.com", "password": "a-long-password-2", "device_id": "d2"}, 201, &reg2)
	h.json("GET", "/v1/admin/users", reg2.Tokens.AccessToken, nil, 403, nil)
	h.json("GET", "/v1/conversations/"+itoa(convID), reg2.Tokens.AccessToken, nil, 404, nil)
	var refreshed auth.Tokens
	h.json("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": reg2.Tokens.RefreshToken}, 200, &refreshed)
	h.json("POST", "/v1/auth/logout", refreshed.AccessToken, nil, 204, nil)
	h.json("POST", "/v1/auth/refresh", "", map[string]string{"refresh_token": refreshed.RefreshToken}, 401, nil)
}

func TestAuthRateLimit(t *testing.T) {
	h := newHarness(t)
	limited := false
	for i := 0; i < 30; i++ {
		resp, _ := h.do("POST", "/v1/auth/login", "", map[string]string{"email": "x@example.com", "password": "whatever-123", "device_id": "d"})
		if resp.StatusCode == 429 {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("auth endpoints are not rate limited")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestImagesInChat(t *testing.T) {
	h := newHarness(t)
	prov := fakeOpenAI(t)
	var reg struct {
		Tokens auth.Tokens `json:"tokens"`
	}
	h.json("POST", "/v1/auth/register", "", map[string]string{"email": "img@example.com", "password": "a-long-password-1", "device_id": "d"}, 201, &reg)
	tok := reg.Tokens.AccessToken
	h.json("POST", "/v1/connections", tok, map[string]string{"provider": "custom", "api_key": providerKey, "label": "Lab",
		"base_url": prov.URL + "/v1"}, 201, nil)

	img := map[string]string{"type": "image", "media_type": "image/png", "data": pngB64}
	resp, raw := h.do("POST", "/v1/chat", tok, map[string]any{"provider": "custom-lab", "model": "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "", "attachments": []any{img}}}})
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "event: done") {
		t.Fatalf("chat with image: %d %s", resp.StatusCode, raw)
	}
	if body, _ := lastProviderBody.Load().(string); !strings.Contains(body, "data:image/png;base64,"+pngB64) {
		t.Fatalf("image not sent to the provider: %s", body)
	}
	var list struct {
		Conversations []struct {
			ID    int64  `json:"id"`
			Title string `json:"title"`
		} `json:"conversations"`
	}
	h.json("GET", "/v1/conversations", tok, nil, 200, &list)
	convID := list.Conversations[0].ID

	// The image is stored and comes back with the history and on its own.
	var conv struct {
		Messages []struct {
			Attachments []struct {
				ID        int64  `json:"id"`
				MediaType string `json:"media_type"`
				URL       string `json:"url"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	h.json("GET", "/v1/conversations/"+itoa(convID), tok, nil, 200, &conv)
	if len(conv.Messages) != 2 || len(conv.Messages[0].Attachments) != 1 || conv.Messages[0].Attachments[0].MediaType != "image/png" {
		t.Fatalf("history: %+v", conv)
	}
	resp, raw = h.do("GET", conv.Messages[0].Attachments[0].URL, tok, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" || !bytes.HasPrefix(raw, []byte("\x89PNG")) {
		t.Fatalf("attachment: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}

	// A follow-up turn sends the earlier image again as context.
	lastProviderBody.Store("")
	h.do("POST", "/v1/chat", tok, map[string]any{"conversation_id": convID, "provider": "custom-lab", "model": "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "and now?"}}})
	if body, _ := lastProviderBody.Load().(string); !strings.Contains(body, pngB64) || !strings.Contains(body, "and now?") {
		t.Fatalf("history image missing on the next turn: %s", body)
	}

	// Another user cannot read it.
	var reg2 struct {
		Tokens auth.Tokens `json:"tokens"`
	}
	h.json("POST", "/v1/auth/register", "", map[string]string{"email": "other@example.com", "password": "a-long-password-2", "device_id": "d"}, 201, &reg2)
	h.json("GET", conv.Messages[0].Attachments[0].URL, reg2.Tokens.AccessToken, nil, 404, nil)

	// Bad images are refused before anything is stored.
	h.json("POST", "/v1/chat", tok, map[string]any{"provider": "custom-lab", "model": "fake-model",
		"messages": []map[string]any{{"role": "user", "content": "x", "attachments": []any{map[string]string{"type": "image", "media_type": "image/jpeg", "data": pngB64}}}}}, 400, nil)

	// OpenAI-compatible API takes data URLs, not remote URLs.
	var created struct {
		Key string `json:"key"`
	}
	h.json("POST", "/v1/apikeys", tok, map[string]any{"name": "cli"}, 201, &created)
	parts := []map[string]any{{"type": "text", "text": "describe"}, {"type": "image_url", "image_url": map[string]string{"url": "data:image/png;base64," + pngB64}}}
	h.json("POST", "/openai/v1/chat/completions", created.Key, map[string]any{"model": "custom-lab/fake-model",
		"messages": []map[string]any{{"role": "user", "content": parts}}}, 200, nil)
	if body, _ := lastProviderBody.Load().(string); !strings.Contains(body, pngB64) {
		t.Fatal("openai-compatible image not forwarded")
	}
	remote := []map[string]any{{"type": "image_url", "image_url": map[string]string{"url": "http://169.254.169.254/x.png"}}}
	h.json("POST", "/openai/v1/chat/completions", created.Key, map[string]any{"model": "custom-lab/fake-model",
		"messages": []map[string]any{{"role": "user", "content": remote}}}, 400, nil)
}
