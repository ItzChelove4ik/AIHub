package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store/sqlite"
)

func TestOpenRouterPKCE(t *testing.T) {
	ctx := context.Background()
	const userKey = "sk-or-v1-user-key-abcd"
	var gotVerifier string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/keys":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["code"] != "the-code" || body["code_challenge_method"] != "S256" {
				w.WriteHeader(403)
				io.WriteString(w, `{"error":{"message":"Invalid code or code_verifier"}}`)
				return
			}
			gotVerifier = body["code_verifier"]
			io.WriteString(w, `{"key":"`+userKey+`"}`)
		case "/models":
			if r.Header.Get("Authorization") != "Bearer "+userKey {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `{"data":[{"id":"anthropic/claude-x","name":"Claude X","context_length":1000}]}`)
		}
	}))
	defer fake.Close()
	orig := providers.Specs["openrouter"]
	spec := orig
	spec.BaseURL = fake.URL
	providers.Specs["openrouter"] = spec
	defer func() { providers.Specs["openrouter"] = orig }()

	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	u := &store.User{Email: "a@example.com", PasswordHash: "x"}
	db.CreateUser(ctx, u)
	box, _ := secure.NewBox(bytes.Repeat([]byte{2}, 32))
	clients := providers.NewClients(true)
	svc := New(db, box, providers.NewRegistry(clients), true)
	o := NewOpenRouterOAuth(svc, "https://aihub.example.com", clients.Trusted)
	o.exchangeURL = fake.URL + "/auth/keys"

	start, err := o.Start(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	su, _ := url.Parse(start)
	q := su.Query()
	if su.Host != "openrouter.ai" || su.Path != "/auth" || q.Get("code_challenge_method") != "S256" ||
		q.Get("callback_url") != "https://aihub.example.com/v1/oauth/openrouter/callback" || q.Get("state") == "" {
		t.Fatalf("auth url: %s", start)
	}

	if _, err := o.Callback(ctx, "the-code", "forged-state"); err != ErrOAuthState {
		t.Fatalf("forged state: %v", err)
	}
	v, err := o.Callback(ctx, "the-code", q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	if s256(gotVerifier) != q.Get("code_challenge") {
		t.Fatal("verifier does not match the challenge")
	}
	if v.Provider != "openrouter" || v.Kind != "oauth" || v.Last4 != "abcd" {
		t.Fatalf("view: %+v", v)
	}
	if _, err := o.Callback(ctx, "the-code", q.Get("state")); err != ErrOAuthState {
		t.Fatalf("state reuse: %v", err)
	}
	cred, _, err := svc.Resolve(ctx, u.ID, "openrouter")
	if err != nil || cred.Key != userKey {
		t.Fatalf("stored key: %v", err)
	}
	ms, errs, err := svc.Catalog(ctx, u.ID)
	if err != nil || len(errs) != 0 || len(ms) != 1 || ms[0].ID != "anthropic/claude-x" {
		t.Fatalf("catalog: %+v %+v %v", ms, errs, err)
	}
}
