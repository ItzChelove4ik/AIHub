package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/chat"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

// sseWriter writes server-sent events; a ticker sends comment pings so
// proxies (and the tunnel) keep an idle stream open.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	rc      *http.ResponseController
	started bool
	stop    chan struct{}
}

func newSSE(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w), stop: make(chan struct{})}
}

func (s *sseWriter) begin() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(200)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-t.C:
				s.mu.Lock()
				_, err := fmt.Fprint(s.w, ": ping\n\n")
				if err == nil {
					err = s.rc.Flush()
				}
				s.mu.Unlock()
				if err != nil {
					return
				}
			}
		}
	}()
}

func (s *sseWriter) close() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
}

func (s *sseWriter) event(name string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.begin()
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, raw); err != nil {
		return err
	}
	return s.rc.Flush()
}

const maxChatBody = 40 << 20

// chatEmitter adapts chat.Emitter to SSE.
type chatEmitter struct{ sse *sseWriter }

func (e chatEmitter) Start(conv, msg int64) error {
	return e.sse.event("start", map[string]int64{"conversation_id": conv, "user_message_id": msg})
}
func (e chatEmitter) Delta(text string) error {
	return e.sse.event("delta", map[string]string{"text": text})
}
func (e chatEmitter) Usage(u providers.Usage) error { return e.sse.event("usage", u) }
func (e chatEmitter) Done(conv, msg int64, stopped bool) error {
	return e.sse.event("done", map[string]any{"conversation_id": conv, "message_id": msg, "stopped": stopped})
}

// chat streams an answer as SSE: start, delta..., usage, done (or error).
// Errors before the stream starts are plain JSON errors. Closing the
// connection cancels the request to the provider.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	var in chat.Input
	// Images total at most 20 MB (27 MB as base64); the iOS client
	// downsizes photos first.
	if !decode(w, r, maxChatBody, &in) {
		return
	}
	sse := newSSE(w)
	defer sse.close()
	err := s.Chat.Run(r.Context(), principal(r).UserID, in, chatEmitter{sse: sse})
	if err == nil || r.Context().Err() != nil {
		return
	}
	if !sse.started {
		s.fail(w, r, err)
		return
	}
	code, msg := "internal", "внутренняя ошибка"
	var pe *providers.Error
	switch {
	case errors.As(err, &pe):
		code, msg = "provider_error", pe.Error()
	case errors.Is(err, providers.ErrAttachmentsUnsupported):
		code, msg = "invalid", err.Error()
	case errors.Is(err, connections.ErrNotConnected), errors.Is(err, connections.ErrUnknownProvider):
		code, msg = "not_connected", err.Error()
	default:
		s.Log.Error("chat stream failed", "err", err)
	}
	_ = sse.event("error", apiError{Code: code, Message: msg})
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	cs, err := s.Chat.List(r.Context(), principal(r).UserID, store.ConversationQuery{Search: q.Get("q"), Limit: limit, Before: before})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]conversationView, 0, len(cs))
	for _, c := range cs {
		out = append(out, toConversationView(c))
	}
	writeJSON(w, 200, map[string]any{"conversations": out})
}

type conversationView struct {
	ID        int64     `json:"id"`
	Title     string    `json:"title"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model"`
	System    string    `json:"system"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toConversationView(c store.Conversation) conversationView {
	return conversationView{ID: c.ID, Title: c.Title, Provider: c.Provider, Model: c.Model, System: c.System,
		CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

type attachmentView struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Size      int    `json:"size"`
	URL       string `json:"url"`
}

type messageView struct {
	ID           int64            `json:"id"`
	Role         string           `json:"role"`
	Content      string           `json:"content"`
	Attachments  []attachmentView `json:"attachments,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	Model        string           `json:"model,omitempty"`
	InputTokens  int64            `json:"input_tokens,omitempty"`
	OutputTokens int64            `json:"output_tokens,omitempty"`
	Status       string           `json:"status"`
	CreatedAt    time.Time        `json:"created_at"`
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	c, msgs, err := s.Chat.Get(r.Context(), principal(r).UserID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	atts, err := s.Chat.Attachments(r.Context(), c.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byMsg := map[int64][]attachmentView{}
	for _, a := range atts {
		byMsg[a.MessageID] = append(byMsg[a.MessageID], attachmentView{ID: a.ID, Type: a.Type, MediaType: a.MediaType,
			Size: a.Size, URL: "/v1/attachments/" + strconv.FormatInt(a.ID, 10)})
	}
	out := make([]messageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageView{ID: m.ID, Role: m.Role, Content: m.Content, Attachments: byMsg[m.ID], Provider: m.Provider,
			Model: m.Model, InputTokens: m.InputTokens, OutputTokens: m.OutputTokens, Status: m.Status, CreatedAt: m.CreatedAt})
	}
	writeJSON(w, 200, map[string]any{"conversation": toConversationView(*c), "messages": out})
}

func (s *Server) renameConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Title string `json:"title"`
	}
	if !decode(w, r, 4<<10, &in) {
		return
	}
	c, err := s.Chat.Rename(r.Context(), principal(r).UserID, id, in.Title)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, toConversationView(*c))
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Chat.Delete(r.Context(), principal(r).UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

// getAttachment returns an image of one of the user's conversations.
func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a, err := s.Chat.Attachment(r.Context(), principal(r).UserID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", a.MediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(a.Data)))
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	_, _ = w.Write(a.Data)
}
