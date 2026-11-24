package connections

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

// OpenRouter OAuth PKCE (https://openrouter.ai/docs/guides/overview/auth/oauth):
// the user authorises at openrouter.ai/auth, OpenRouter redirects to our
// callback with ?code=..&state=.., and we exchange the code together with
// the verifier only we hold for a user-controlled API key.
const (
	OpenRouterAuthURL     = "https://openrouter.ai/auth"
	OpenRouterKeyExchange = "https://openrouter.ai/api/v1/auth/keys"
	oauthStateTTL         = 10 * time.Minute // OpenRouter codes expire after 10 minutes too
)

var ErrOAuthState = errors.New("сессия входа устарела или недействительна, начните заново")

type OpenRouterOAuth struct {
	svc         *Service
	callbackURL string
	exchangeURL string
	http        *http.Client
}

func NewOpenRouterOAuth(svc *Service, publicURL string, client *http.Client) *OpenRouterOAuth {
	return &OpenRouterOAuth{svc: svc, callbackURL: strings.TrimRight(publicURL, "/") + "/v1/oauth/openrouter/callback",
		exchangeURL: OpenRouterKeyExchange, http: client}
}

func s256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Start creates a PKCE verifier and state for the user and returns the URL
// to open.
func (o *OpenRouterOAuth) Start(ctx context.Context, userID int64) (string, error) {
	verifier, err := secure.RandomURLToken(48)
	if err != nil {
		return "", err
	}
	state, err := secure.RandomURLToken(24)
	if err != nil {
		return "", err
	}
	hash := secure.HashToken(state)
	sealed, err := o.svc.box.SealString(verifier, "oauth:"+hash)
	if err != nil {
		return "", err
	}
	if err := o.svc.store.CreateOAuthState(ctx, &store.OAuthState{Hash: hash, UserID: userID, Provider: "openrouter",
		SealedVerifier: sealed, ExpiresAt: o.svc.now().Add(oauthStateTTL)}); err != nil {
		return "", err
	}
	q := url.Values{
		"callback_url":          {o.callbackURL},
		"code_challenge":        {s256(verifier)},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		"key_label":             {"AIHub"},
	}
	return OpenRouterAuthURL + "?" + q.Encode(), nil
}

// Callback exchanges the code and stores the key as the user's OpenRouter
// credential.
func (o *OpenRouterOAuth) Callback(ctx context.Context, code, state string) (*View, error) {
	if code == "" || state == "" {
		return nil, ErrOAuthState
	}
	hash := secure.HashToken(state)
	st, err := o.svc.store.TakeOAuthState(ctx, hash, o.svc.now())
	if err != nil {
		return nil, ErrOAuthState
	}
	verifier, err := o.svc.box.OpenString(st.SealedVerifier, "oauth:"+hash)
	if err != nil {
		return nil, ErrOAuthState
	}
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.exchangeURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, errors.New("OpenRouter недоступен")
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		msg := extractMessage(raw)
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return nil, errors.New("OpenRouter отклонил вход: " + msg)
	}
	var out struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(raw, &out) != nil || out.Key == "" {
		return nil, errors.New("OpenRouter не вернул ключ")
	}
	return o.svc.Add(ctx, st.UserID, AddInput{Provider: "openrouter", Key: out.Key, Kind: "oauth"})
}

func extractMessage(raw []byte) string {
	var v struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if v.Error.Message != "" {
		return v.Error.Message
	}
	return v.Message
}
