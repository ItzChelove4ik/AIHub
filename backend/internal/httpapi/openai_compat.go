package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/apikeys"
	"github.com/ItzChelove4ik/AIHub/backend/internal/chat"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

// OpenAI-compatible API on AIHub keys (ahk_live_...). Models are named
// "provider/model" and run on the key owner's connected providers.

func openaiError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func (s *Server) apiKeyAuth(w http.ResponseWriter, r *http.Request, scope string) *store.APIKey {
	k, err := s.APIKeys.Authenticate(r.Context(), bearer(r), scope)
	switch {
	case err == nil:
		return k
	case errors.Is(err, apikeys.ErrRateLimited):
		openaiError(w, 429, "rate_limit_error", "rate_limited", err.Error())
	case errors.Is(err, apikeys.ErrScope):
		openaiError(w, 403, "permission_error", "insufficient_scope", err.Error())
	case errors.Is(err, apikeys.ErrInvalid):
		openaiError(w, 401, "invalid_request_error", "invalid_api_key", err.Error())
	default:
		s.Log.Error("api key auth", "err", err)
		openaiError(w, 500, "server_error", "internal", "внутренняя ошибка")
	}
	return nil
}

func (s *Server) openaiModels(w http.ResponseWriter, r *http.Request) {
	k := s.apiKeyAuth(w, r, apikeys.ScopeModels)
	if k == nil {
		return
	}
	ms, _, err := s.Conn.Catalog(r.Context(), k.UserID)
	if err != nil {
		openaiError(w, 500, "server_error", "internal", "внутренняя ошибка")
		return
	}
	data := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		data = append(data, map[string]any{"id": m.Provider + "/" + m.ID, "object": "model", "created": 0, "owned_by": m.Provider})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

type oaiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	Name    string          `json:"name,omitempty"`
}

type oaiRequest struct {
	Model               string       `json:"model"`
	Messages            []oaiMessage `json:"messages"`
	Stream              bool         `json:"stream"`
	Temperature         *float64     `json:"temperature"`
	MaxTokens           int          `json:"max_tokens"`
	MaxCompletionTokens int          `json:"max_completion_tokens"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// parseContent accepts a string or an array of text and image_url parts.
// Images must be data: URLs; the server does not fetch remote URLs.
func parseContent(raw json.RawMessage) (string, []providers.Attachment, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil, errors.New("content: строка или массив частей")
	}
	var b strings.Builder
	var atts []providers.Attachment
	for _, p := range parts {
		switch p.Type {
		case "text":
			b.WriteString(p.Text)
		case "image_url":
			if p.ImageURL == nil {
				return "", nil, errors.New("image_url без url")
			}
			a, err := parseDataURL(p.ImageURL.URL)
			if err != nil {
				return "", nil, err
			}
			atts = append(atts, a)
		default:
			return "", nil, errors.New("поддерживаются части text и image_url")
		}
	}
	return b.String(), atts, nil
}

func parseDataURL(u string) (providers.Attachment, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return providers.Attachment{}, errors.New("изображения принимаются только как data URL (data:image/png;base64,<данные>)")
	}
	meta, data, ok := strings.Cut(rest, ",")
	mediaType, isB64 := strings.CutSuffix(meta, ";base64")
	if !ok || !isB64 {
		return providers.Attachment{}, errors.New("data URL изображения должен быть в base64")
	}
	a := providers.Attachment{Type: "image", MediaType: mediaType, Data: data}
	if _, err := providers.ValidateAttachment(a); err != nil {
		return providers.Attachment{}, err
	}
	return a, nil
}

func (s *Server) openaiChat(w http.ResponseWriter, r *http.Request) {
	k := s.apiKeyAuth(w, r, apikeys.ScopeChat)
	if k == nil {
		return
	}
	var in oaiRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxChatBody))
	if err := dec.Decode(&in); err != nil { // unknown fields are allowed here, as OpenAI clients send many
		openaiError(w, 400, "invalid_request_error", "bad_json", "некорректный JSON")
		return
	}
	provider, model, ok := strings.Cut(in.Model, "/")
	if !ok || provider == "" || model == "" {
		openaiError(w, 400, "invalid_request_error", "model_not_found", "модель указывается как provider/model, например anthropic/<id>")
		return
	}
	var system []string
	var msgs []providers.Message
	images, imageBytes := 0, 0
	for _, m := range in.Messages {
		text, atts, err := parseContent(m.Content)
		if err != nil {
			openaiError(w, 400, "invalid_request_error", "bad_content", err.Error())
			return
		}
		images += len(atts)
		for _, a := range atts {
			imageBytes += len(a.Data) * 3 / 4
		}
		if len(atts) > 0 && m.Role != "user" {
			openaiError(w, 400, "invalid_request_error", "bad_content", "изображения можно прикреплять только к сообщениям user")
			return
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, text)
		case "user", "assistant":
			msgs = append(msgs, providers.Message{Role: m.Role, Content: text, Attachments: atts})
		default:
			openaiError(w, 400, "invalid_request_error", "unsupported_role", "роль "+m.Role+" не поддерживается")
			return
		}
	}
	if images > providers.MaxImagesPerRequest || imageBytes > providers.MaxImageBytesPerRequest {
		openaiError(w, 400, "invalid_request_error", "too_many_images", "слишком много изображений")
		return
	}
	if len(msgs) == 0 || msgs[len(msgs)-1].Role != "user" {
		openaiError(w, 400, "invalid_request_error", "bad_messages", "последнее сообщение должно быть от user")
		return
	}
	if err := s.Chat.CheckLimits(r.Context(), k.UserID); err != nil {
		if errors.Is(err, chat.ErrLimitExceeded) {
			openaiError(w, 429, "rate_limit_error", "limit_exceeded", err.Error())
		} else {
			openaiError(w, 500, "server_error", "internal", "внутренняя ошибка")
		}
		return
	}
	maxTokens := in.MaxCompletionTokens
	if maxTokens == 0 {
		maxTokens = in.MaxTokens
	}
	req := providers.ChatRequest{Model: model, System: strings.Join(system, "\n\n"), Messages: msgs, Temperature: in.Temperature, MaxTokens: maxTokens}
	rnd, _ := secure.RandomString(24)
	id := "chatcmpl-" + rnd
	created := time.Now().Unix()

	if !in.Stream {
		var b strings.Builder
		usage, err := s.Conn.Stream(r.Context(), k.UserID, provider, req, func(d string) error { b.WriteString(d); return nil })
		s.Chat.Record(k.UserID, provider, usage)
		if err != nil {
			s.openaiFail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": in.Model,
			"choices": []map[string]any{{"index": 0, "message": map[string]string{"role": "assistant", "content": b.String()}, "finish_reason": "stop"}},
			"usage":   oaiUsage(usage),
		})
		return
	}

	sse := newSSE(w)
	defer sse.close()
	chunk := func(delta map[string]string, finish *string) error {
		return sse.data(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": in.Model,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}}})
	}
	first := true
	usage, err := s.Conn.Stream(r.Context(), k.UserID, provider, req, func(d string) error {
		delta := map[string]string{"content": d}
		if first {
			delta["role"] = "assistant"
			first = false
		}
		return chunk(delta, nil)
	})
	s.Chat.Record(k.UserID, provider, usage)
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		if !sse.started {
			s.openaiFail(w, err)
			return
		}
		_ = sse.data(map[string]any{"error": map[string]any{"message": err.Error(), "type": "server_error"}})
		_ = sse.raw("[DONE]")
		return
	}
	stop := "stop"
	_ = chunk(map[string]string{}, &stop)
	if in.StreamOptions != nil && in.StreamOptions.IncludeUsage {
		_ = sse.data(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": in.Model,
			"choices": []any{}, "usage": oaiUsage(usage)})
	}
	_ = sse.raw("[DONE]")
}

func oaiUsage(u providers.Usage) map[string]int64 {
	return map[string]int64{"prompt_tokens": u.InputTokens, "completion_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens}
}

func (s *Server) openaiFail(w http.ResponseWriter, err error) {
	var pe *providers.Error
	switch {
	case errors.Is(err, connections.ErrNotConnected), errors.Is(err, connections.ErrUnknownProvider):
		openaiError(w, 404, "invalid_request_error", "model_not_found", err.Error())
	case errors.Is(err, providers.ErrAttachmentsUnsupported):
		openaiError(w, 400, "invalid_request_error", "bad_content", err.Error())
	case errors.As(err, &pe):
		status := 502
		if pe.Status >= 400 && pe.Status < 500 {
			status = pe.Status
		}
		openaiError(w, status, "upstream_error", "provider_error", pe.Error())
	default:
		s.Log.Error("openai-compatible chat failed", "err", err)
		openaiError(w, 500, "server_error", "internal", "внутренняя ошибка")
	}
}

// data writes an unnamed SSE event (OpenAI's stream format).
func (s *sseWriter) data(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.raw(string(raw))
}

func (s *sseWriter) raw(line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begin()
	if _, err := s.w.Write([]byte("data: " + line + "\n\n")); err != nil {
		return err
	}
	return s.rc.Flush()
}
