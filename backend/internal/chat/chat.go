// Package chat runs conversations: it keeps the history on the server,
// streams answers from the providers, counts tokens and requests per user,
// provider and day, and enforces the limits the admin sets.
package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

var (
	ErrLimitExceeded = errors.New("дневной лимит исчерпан")
	ErrNotFound      = errors.New("чат не найден")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type Service struct {
	store         store.Store
	conn          *connections.Service
	log           *slog.Logger
	defaultReq    int64
	defaultTokens int64
	now           func() time.Time
}

func New(s store.Store, conn *connections.Service, log *slog.Logger, defaultDailyRequests, defaultDailyTokens int64) *Service {
	return &Service{store: s, conn: conn, log: log, defaultReq: defaultDailyRequests, defaultTokens: defaultDailyTokens, now: time.Now}
}

func (s *Service) day() string { return s.now().UTC().Format("2006-01-02") }

// EffectiveLimits are the user's limits with server defaults applied; 0 is
// unlimited.
func (s *Service) EffectiveLimits(ctx context.Context, userID int64) (requests, tokens int64, err error) {
	l, err := s.store.GetLimits(ctx, userID)
	if err != nil {
		return 0, 0, err
	}
	requests, tokens = s.defaultReq, s.defaultTokens
	if l.DailyRequests != nil {
		requests = *l.DailyRequests
	}
	if l.DailyTokens != nil {
		tokens = *l.DailyTokens
	}
	return requests, tokens, nil
}

// CheckLimits fails with ErrLimitExceeded when today's usage reached a limit.
func (s *Service) CheckLimits(ctx context.Context, userID int64) error {
	maxReq, maxTok, err := s.EffectiveLimits(ctx, userID)
	if err != nil {
		return err
	}
	if maxReq == 0 && maxTok == 0 {
		return nil
	}
	req, tok, err := s.store.UsageForDay(ctx, userID, s.day())
	if err != nil {
		return err
	}
	if (maxReq > 0 && req >= maxReq) || (maxTok > 0 && tok >= maxTok) {
		return ErrLimitExceeded
	}
	return nil
}

// Record counts one request and its tokens. It uses a fresh context so a
// cancelled stream is still counted.
func (s *Service) Record(userID int64, provider string, u providers.Usage) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.AddUsage(ctx, userID, provider, s.day(), 1, u.InputTokens, u.OutputTokens); err != nil {
		s.log.Error("record usage", "err", err)
	}
}

// Input is one POST /v1/chat.
type Input struct {
	ConversationID int64               `json:"conversation_id,omitempty"`
	Provider       string              `json:"provider"`
	Model          string              `json:"model"`
	Messages       []providers.Message `json:"messages"`
	System         *string             `json:"system,omitempty"`
	Temperature    *float64            `json:"temperature,omitempty"`
	MaxTokens      int                 `json:"max_tokens,omitempty"`
}

// Emitter receives the stream's events: start, delta, usage, done.
type Emitter interface {
	Start(conversationID, userMessageID int64) error
	Delta(text string) error
	Usage(u providers.Usage) error
	Done(conversationID, assistantMessageID int64, stopped bool) error
}

func validate(in *Input) error {
	in.Provider = strings.TrimSpace(in.Provider)
	in.Model = strings.TrimSpace(in.Model)
	if in.Provider == "" || in.Model == "" {
		return &ValidationError{"нужны provider и model"}
	}
	if len(in.Messages) == 0 {
		return &ValidationError{"messages пуст"}
	}
	if len(in.Messages) > 500 {
		return &ValidationError{"слишком много сообщений"}
	}
	for i := range in.Messages {
		m := &in.Messages[i]
		if m.Role != "user" && m.Role != "assistant" {
			return &ValidationError{"role сообщения: user или assistant"}
		}
		if len(m.Content) > 1<<20 {
			return &ValidationError{"сообщение слишком длинное"}
		}
		if len(m.Attachments) > 0 {
			return &ValidationError{providers.ErrAttachmentsUnsupported.Error()}
		}
	}
	if in.Messages[len(in.Messages)-1].Role != "user" {
		return &ValidationError{"последнее сообщение должно быть от user"}
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		return &ValidationError{"temperature от 0 до 2"}
	}
	if in.System != nil && len(*in.System) > 64<<10 {
		return &ValidationError{"system слишком длинный"}
	}
	return nil
}

// Run handles one chat request. With conversation_id the stored history is
// sent before the new messages; without it a conversation is created from
// the given messages. Messages and the answer (even a partial one, when the
// client disconnects) are saved.
func (s *Service) Run(ctx context.Context, userID int64, in Input, emit Emitter) error {
	if err := validate(&in); err != nil {
		return err
	}
	if err := s.CheckLimits(ctx, userID); err != nil {
		return err
	}
	// Fail before creating anything when the provider is not connected.
	if _, _, err := s.conn.Resolve(ctx, userID, in.Provider); err != nil {
		return err
	}
	now := s.now()
	var conv *store.Conversation
	var history []providers.Message
	if in.ConversationID > 0 {
		c, err := s.store.ConversationByID(ctx, userID, in.ConversationID)
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		conv = c
		stored, err := s.store.ListMessages(ctx, c.ID)
		if err != nil {
			return err
		}
		for _, m := range stored {
			if m.Content == "" {
				continue
			}
			history = append(history, providers.Message{Role: m.Role, Content: m.Content})
		}
		conv.Provider, conv.Model = in.Provider, in.Model
		if in.System != nil {
			conv.System = *in.System
		}
	} else {
		conv = &store.Conversation{UserID: userID, Title: fallbackTitle(in.Messages), Provider: in.Provider, Model: in.Model,
			CreatedAt: now, UpdatedAt: now}
		if in.System != nil {
			conv.System = *in.System
		}
		if err := s.store.CreateConversation(ctx, conv); err != nil {
			return err
		}
	}
	var lastUserID int64
	for _, m := range in.Messages {
		sm := &store.Message{ConversationID: conv.ID, Role: m.Role, Content: m.Content, CreatedAt: now}
		if err := s.store.AddMessage(ctx, sm); err != nil {
			return err
		}
		lastUserID = sm.ID
	}
	conv.UpdatedAt = now
	if err := s.store.UpdateConversation(ctx, conv); err != nil {
		return err
	}
	if err := emit.Start(conv.ID, lastUserID); err != nil {
		return err
	}

	isNew := in.ConversationID == 0
	req := providers.ChatRequest{Model: in.Model, System: conv.System, Messages: append(history, in.Messages...),
		Temperature: in.Temperature, MaxTokens: in.MaxTokens}
	var answer strings.Builder
	usage, err := s.conn.Stream(ctx, userID, in.Provider, req, func(d string) error {
		answer.WriteString(d)
		return emit.Delta(d)
	})
	stopped := ctx.Err() != nil
	s.Record(userID, in.Provider, usage)

	status := "complete"
	switch {
	case stopped:
		status = "stopped"
	case err != nil:
		status = "error"
	}
	saveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	am := &store.Message{ConversationID: conv.ID, Role: "assistant", Content: answer.String(), Provider: in.Provider,
		Model: in.Model, InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, Status: status, CreatedAt: s.now()}
	if answer.Len() > 0 || err == nil {
		if serr := s.store.AddMessage(saveCtx, am); serr != nil {
			s.log.Error("save answer", "err", serr)
		}
	}
	if stopped {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if err := emit.Usage(usage); err != nil {
		return err
	}
	if isNew && answer.Len() > 0 {
		go s.generateTitle(userID, conv.ID, in.Provider, in.Model, in.Messages, answer.String())
	}
	return emit.Done(conv.ID, am.ID, false)
}

func fallbackTitle(msgs []providers.Message) string {
	for _, m := range msgs {
		if m.Role == "user" && strings.TrimSpace(m.Content) != "" {
			t := strings.Join(strings.Fields(m.Content), " ")
			if utf8.RuneCountInString(t) > 60 {
				t = string([]rune(t)[:60]) + "…"
			}
			return t
		}
	}
	return "Новый чат"
}

// generateTitle asks the same model for a short title, in the background.
// The fallback title (the start of the first message) stays on failure.
func (s *Service) generateTitle(userID, convID int64, provider, model string, msgs []providers.Message, answer string) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	first := ""
	for _, m := range msgs {
		if m.Role == "user" {
			first = m.Content
			break
		}
	}
	if utf8.RuneCountInString(first) > 2000 {
		first = string([]rune(first)[:2000])
	}
	if utf8.RuneCountInString(answer) > 2000 {
		answer = string([]rune(answer)[:2000])
	}
	req := providers.ChatRequest{
		Model:     model,
		System:    "Придумай очень короткий заголовок (2-6 слов) для этого диалога на языке пользователя. Ответь только заголовком, без кавычек и точки.",
		Messages:  []providers.Message{{Role: "user", Content: fmt.Sprintf("Сообщение пользователя:\n%s\n\nОтвет ассистента:\n%s", first, answer)}},
		MaxTokens: 1024,
	}
	var b strings.Builder
	usage, err := s.conn.Stream(ctx, userID, provider, req, func(d string) error {
		b.WriteString(d)
		if b.Len() > 400 {
			return errors.New("title too long")
		}
		return nil
	})
	s.Record(userID, provider, usage)
	title := strings.Trim(strings.TrimSpace(strings.SplitN(b.String(), "\n", 2)[0]), `"«»'.`)
	if err != nil || title == "" {
		return
	}
	if utf8.RuneCountInString(title) > 80 {
		title = string([]rune(title)[:80])
	}
	c, err := s.store.ConversationByID(ctx, userID, convID)
	if err != nil {
		return
	}
	c.Title = title
	if err := s.store.UpdateConversation(ctx, c); err != nil {
		s.log.Warn("save title", "err", err)
	}
}

// --- conversations ---

func (s *Service) List(ctx context.Context, userID int64, q store.ConversationQuery) ([]store.Conversation, error) {
	return s.store.ListConversations(ctx, userID, q)
}

func (s *Service) Get(ctx context.Context, userID, id int64) (*store.Conversation, []store.Message, error) {
	c, err := s.store.ConversationByID(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	msgs, err := s.store.ListMessages(ctx, id)
	return c, msgs, err
}

func (s *Service) Rename(ctx context.Context, userID, id int64, title string) (*store.Conversation, error) {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > 200 {
		return nil, &ValidationError{"заголовок от 1 до 200 символов"}
	}
	c, err := s.store.ConversationByID(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.Title = title
	c.UpdatedAt = s.now()
	return c, s.store.UpdateConversation(ctx, c)
}

func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	err := s.store.DeleteConversation(ctx, userID, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	return err
}
