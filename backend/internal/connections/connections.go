// Package connections manages a user's AI provider credentials (BYOK keys
// and OpenRouter OAuth keys) and the model catalog built from them. Keys
// are stored sealed with AES-256-GCM and never leave the server again.
package connections

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

const CatalogTTL = time.Hour

var (
	ErrUnknownProvider = errors.New("неизвестный провайдер")
	ErrNotConnected    = errors.New("этот провайдер не подключён")
)

// ValidationError is a bad input.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// View is what the API shows of a credential: never the key itself.
type View struct {
	ID        int64     `json:"id"`
	Provider  string    `json:"provider"` // the slug models and chats refer to
	Type      string    `json:"type"`     // provider spec: openai, anthropic, ..., custom
	Name      string    `json:"name"`
	Label     string    `json:"label"`
	BaseURL   string    `json:"base_url,omitempty"`
	Kind      string    `json:"kind"` // api_key | oauth
	Last4     string    `json:"last4"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type cacheEntry struct {
	version time.Time
	at      time.Time
	models  []providers.Model
}

type Service struct {
	store        store.Store
	box          *secure.Box
	reg          *providers.Registry
	allowPrivate bool
	now          func() time.Time

	mu    sync.Mutex
	cache map[int64]cacheEntry // by credential id
}

func New(s store.Store, box *secure.Box, reg *providers.Registry, allowPrivate bool) *Service {
	return &Service{store: s, box: box, reg: reg, allowPrivate: allowPrivate, now: time.Now, cache: map[int64]cacheEntry{}}
}

func aad(userID int64, slug string) string { return fmt.Sprintf("cred:%d:%s", userID, slug) }

func view(c store.Credential) View {
	spec := providers.Specs[c.Provider]
	name := spec.Name
	if c.Label != "" && spec.Custom {
		name = c.Label
	}
	return View{ID: c.ID, Provider: c.Slug, Type: c.Provider, Name: name, Label: c.Label, BaseURL: c.BaseURL,
		Kind: c.Kind, Last4: c.Last4, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

var slugClean = regexp.MustCompile(`[^a-z0-9]+`)

func customSlug(label string) string {
	s := strings.Trim(slugClean.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(s) > 32 {
		s = strings.Trim(s[:32], "-")
	}
	return "custom-" + s
}

type AddInput struct {
	Provider string
	Key      string
	Label    string
	BaseURL  string
	Kind     string // api_key (default) | oauth
}

// Add verifies a key by listing the provider's models, then stores it
// sealed. A key for an already connected provider replaces it.
func (s *Service) Add(ctx context.Context, userID int64, in AddInput) (*View, error) {
	spec, ok := providers.Specs[in.Provider]
	if !ok {
		return nil, ErrUnknownProvider
	}
	key := strings.TrimSpace(in.Key)
	if key == "" || len(key) > 512 || strings.ContainsAny(key, " \r\n\t") {
		return nil, &ValidationError{"некорректный ключ"}
	}
	label := strings.TrimSpace(in.Label)
	if len(label) > 64 {
		return nil, &ValidationError{"метка не длиннее 64 символов"}
	}
	slug := spec.ID
	base := ""
	if spec.Custom {
		if label == "" {
			return nil, &ValidationError{"для своего провайдера нужна метка (label)"}
		}
		slug = customSlug(label)
		if slug == "custom-" {
			return nil, &ValidationError{"метка должна содержать буквы или цифры"}
		}
		var err error
		if base, err = providers.ValidateBaseURL(in.BaseURL, s.allowPrivate); err != nil {
			return nil, &ValidationError{err.Error()}
		}
	}
	kind := in.Kind
	if kind == "" {
		kind = "api_key"
	}
	cred := providers.Credential{Spec: spec, Key: key, BaseURL: base}
	models, err := s.reg.Adapter(spec).ListModels(ctx, cred)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Unauthorized() {
			return nil, &ValidationError{"провайдер отклонил ключ: " + pe.Message}
		}
		return nil, fmt.Errorf("проверка ключа: %w", err)
	}
	sealed, err := s.box.SealString(key, aad(userID, slug))
	if err != nil {
		return nil, err
	}
	now := s.now()
	c := &store.Credential{UserID: userID, Slug: slug, Provider: spec.ID, Label: label, BaseURL: base, Kind: kind,
		SealedKey: sealed, Last4: secure.Last4(key), CreatedAt: now, UpdatedAt: now}
	if err := s.store.UpsertCredential(ctx, c); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[c.ID] = cacheEntry{version: c.UpdatedAt, at: now, models: withSlug(models, slug)}
	s.mu.Unlock()
	v := view(*c)
	return &v, nil
}

func withSlug(ms []providers.Model, slug string) []providers.Model {
	out := make([]providers.Model, len(ms))
	for i, m := range ms {
		m.Provider = slug
		out[i] = m
	}
	return out
}

func (s *Service) List(ctx context.Context, userID int64) ([]View, error) {
	cs, err := s.store.ListCredentials(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(cs))
	for _, c := range cs {
		out = append(out, view(c))
	}
	return out, nil
}

func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	if err := s.store.DeleteCredential(ctx, userID, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.cache, id)
	s.mu.Unlock()
	return nil
}

// Resolve opens the user's credential for a provider slug.
func (s *Service) Resolve(ctx context.Context, userID int64, slug string) (providers.Credential, *store.Credential, error) {
	c, err := s.store.CredentialBySlug(ctx, userID, slug)
	if errors.Is(err, store.ErrNotFound) {
		return providers.Credential{}, nil, ErrNotConnected
	}
	if err != nil {
		return providers.Credential{}, nil, err
	}
	spec, ok := providers.Specs[c.Provider]
	if !ok {
		return providers.Credential{}, nil, ErrUnknownProvider
	}
	key, err := s.box.OpenString(c.SealedKey, aad(userID, c.Slug))
	if err != nil {
		return providers.Credential{}, nil, err
	}
	return providers.Credential{Spec: spec, Key: key, BaseURL: c.BaseURL}, c, nil
}

func (s *Service) modelsFor(ctx context.Context, userID int64, c store.Credential) ([]providers.Model, error) {
	s.mu.Lock()
	e, ok := s.cache[c.ID]
	s.mu.Unlock()
	if ok && e.version.Equal(c.UpdatedAt) && s.now().Sub(e.at) < CatalogTTL {
		return e.models, nil
	}
	cred, _, err := s.Resolve(ctx, userID, c.Slug)
	if err != nil {
		return nil, err
	}
	ms, err := s.reg.Adapter(cred.Spec).ListModels(ctx, cred)
	if err != nil {
		if ok { // serve the stale list rather than nothing
			return e.models, nil
		}
		return nil, err
	}
	ms = withSlug(ms, c.Slug)
	s.mu.Lock()
	s.cache[c.ID] = cacheEntry{version: c.UpdatedAt, at: s.now(), models: ms}
	s.mu.Unlock()
	return ms, nil
}

// ProviderError is one provider that could not list its models.
type ProviderError struct {
	Provider string `json:"provider"`
	Error    string `json:"error"`
}

// Catalog is every model of every connected provider (lists cached for an
// hour), sorted by provider and name.
func (s *Service) Catalog(ctx context.Context, userID int64) ([]providers.Model, []ProviderError, error) {
	cs, err := s.store.ListCredentials(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	type result struct {
		slug   string
		models []providers.Model
		err    error
	}
	results := make([]result, len(cs))
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ms, err := s.modelsFor(ctx, userID, c)
			results[i] = result{slug: c.Slug, models: ms, err: err}
		}()
	}
	wg.Wait()
	var models []providers.Model
	var errs []ProviderError
	for _, r := range results {
		if r.err != nil {
			errs = append(errs, ProviderError{Provider: r.slug, Error: r.err.Error()})
			continue
		}
		models = append(models, r.models...)
	}
	sort.SliceStable(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return strings.ToLower(models[i].DisplayName) < strings.ToLower(models[j].DisplayName)
	})
	return models, errs, nil
}

// ModelInfo returns the cached catalog entry of a model, nil if unknown.
func (s *Service) ModelInfo(ctx context.Context, userID int64, c *store.Credential, model string) *providers.Model {
	ms, err := s.modelsFor(ctx, userID, *c)
	if err != nil {
		return nil
	}
	for i := range ms {
		if ms[i].ID == model {
			return &ms[i]
		}
	}
	return nil
}

// Stream runs a chat on the user's credential for the provider slug.
func (s *Service) Stream(ctx context.Context, userID int64, slug string, req providers.ChatRequest, onDelta func(string) error) (providers.Usage, error) {
	cred, c, err := s.Resolve(ctx, userID, slug)
	if err != nil {
		return providers.Usage{}, err
	}
	if req.MaxTokens <= 0 && cred.Spec.Kind == providers.KindAnthropic {
		if m := s.ModelInfo(ctx, userID, c, req.Model); m != nil && m.MaxOutput > 0 {
			req.MaxTokens = min(m.MaxOutput, 32000)
		}
	}
	return s.reg.Adapter(cred.Spec).StreamChat(ctx, cred, req, onDelta)
}
