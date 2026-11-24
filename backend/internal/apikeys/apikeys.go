// Package apikeys manages AIHub's own API keys (ahk_live_...), which let
// other programs use a user's connected providers through the
// OpenAI-compatible endpoints. Only a SHA-256 hash and a short prefix are
// stored; the full key is shown once.
package apikeys

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

const (
	Prefix      = "ahk_live_"
	ScopeChat   = "chat"
	ScopeModels = "models"
)

var (
	ErrInvalid     = errors.New("неверный API-ключ")
	ErrRateLimited = errors.New("превышен лимит запросов этого ключа")
	ErrScope       = errors.New("у ключа нет нужного разрешения")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// View is what the API shows of a key.
type View struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	RPM        int        `json:"rpm"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
}

func view(k store.APIKey) View {
	return View{ID: k.ID, Name: k.Name, Prefix: k.Prefix, Scopes: k.Scopes, RPM: k.RPM, LastUsedAt: k.LastUsedAt,
		CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt}
}

type Service struct {
	store store.Store
	now   func() time.Time

	mu       sync.Mutex
	limiters map[int64]*rate.Limiter
}

func New(s store.Store) *Service {
	return &Service{store: s, now: time.Now, limiters: map[int64]*rate.Limiter{}}
}

type CreateInput struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
	RPM    int      `json:"rpm"`
}

// Create makes a key and returns it in full, the only time it is shown.
func (s *Service) Create(ctx context.Context, userID int64, in CreateInput) (string, *View, error) {
	name := strings.TrimSpace(in.Name)
	if len(name) > 64 {
		return "", nil, &ValidationError{"имя не длиннее 64 символов"}
	}
	scopes := in.Scopes
	if len(scopes) == 0 {
		scopes = []string{ScopeChat, ScopeModels}
	}
	for _, sc := range scopes {
		if sc != ScopeChat && sc != ScopeModels {
			return "", nil, &ValidationError{"scopes: chat, models"}
		}
	}
	slices.Sort(scopes)
	scopes = slices.Compact(scopes)
	if in.RPM < 0 || in.RPM > 10000 {
		return "", nil, &ValidationError{"rpm от 0 (без лимита) до 10000"}
	}
	random, err := secure.RandomString(40)
	if err != nil {
		return "", nil, err
	}
	full := Prefix + random
	k := &store.APIKey{UserID: userID, Name: name, Prefix: full[:len(Prefix)+6], Hash: secure.HashToken(full),
		Scopes: scopes, RPM: in.RPM, CreatedAt: s.now()}
	if err := s.store.CreateAPIKey(ctx, k); err != nil {
		return "", nil, err
	}
	v := view(*k)
	return full, &v, nil
}

func (s *Service) List(ctx context.Context, userID int64) ([]View, error) {
	ks, err := s.store.ListAPIKeys(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(ks))
	for _, k := range ks {
		out = append(out, view(k))
	}
	return out, nil
}

func (s *Service) Revoke(ctx context.Context, userID, id int64) error {
	return s.store.RevokeAPIKey(ctx, userID, id, s.now())
}

// Authenticate checks a presented key, its scope and its rate limit.
func (s *Service) Authenticate(ctx context.Context, raw, scope string) (*store.APIKey, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, Prefix) || len(raw) > 128 {
		return nil, ErrInvalid
	}
	k, err := s.store.APIKeyByHash(ctx, secure.HashToken(raw))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalid
	}
	if err != nil {
		return nil, err
	}
	if k.RevokedAt != nil {
		return nil, ErrInvalid
	}
	if !slices.Contains(k.Scopes, scope) {
		return nil, ErrScope
	}
	if k.RPM > 0 && !s.limiter(k).Allow() {
		return nil, ErrRateLimited
	}
	_ = s.store.TouchAPIKey(ctx, k.ID, s.now())
	return k, nil
}

func (s *Service) limiter(k *store.APIKey) *rate.Limiter {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.limiters[k.ID]
	if !ok || l.Burst() != k.RPM {
		l = rate.NewLimiter(rate.Limit(float64(k.RPM)/60), k.RPM)
		s.limiters[k.ID] = l
	}
	return l
}
