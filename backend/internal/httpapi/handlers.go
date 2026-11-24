package httpapi

import (
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/apikeys"
	"github.com/ItzChelove4ik/AIHub/backend/internal/auth"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

// --- auth ---

type userView struct {
	ID        int64     `json:"id"`
	Email     string    `json:"email"`
	IsAdmin   bool      `json:"is_admin"`
	CreatedAt time.Time `json:"created_at"`
}

func toUserView(u *store.User) userView {
	return userView{ID: u.ID, Email: u.Email, IsAdmin: u.IsAdmin, CreatedAt: u.CreatedAt}
}

type authResponse struct {
	User   userView     `json:"user"`
	Tokens *auth.Tokens `json:"tokens"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
		InviteCode string `json:"invite_code"`
	}
	if !decode(w, r, 16<<10, &in) {
		return
	}
	u, tok, err := s.Auth.Register(r.Context(), auth.RegisterInput{Email: in.Email, Password: in.Password,
		DeviceID: in.DeviceID, DeviceName: in.DeviceName, InviteCode: in.InviteCode})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Log.Info("user registered", "user", u.ID, "admin", u.IsAdmin)
	writeJSON(w, 201, authResponse{User: toUserView(u), Tokens: tok})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email      string `json:"email"`
		Password   string `json:"password"`
		DeviceID   string `json:"device_id"`
		DeviceName string `json:"device_name"`
	}
	if !decode(w, r, 16<<10, &in) {
		return
	}
	u, tok, err := s.Auth.Login(r.Context(), auth.LoginInput{Email: in.Email, Password: in.Password, DeviceID: in.DeviceID, DeviceName: in.DeviceName})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, authResponse{User: toUserView(u), Tokens: tok})
}

func (s *Server) refresh(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RefreshToken string `json:"refresh_token"`
	}
	if !decode(w, r, 4<<10, &in) {
		return
	}
	tok, err := s.Auth.Refresh(r.Context(), in.RefreshToken)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, tok)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(r.Context(), principal(r)); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	u, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"user": toUserView(u), "device_id": p.DeviceID})
}

type deviceView struct {
	ID         int64      `json:"id"`
	DeviceID   string     `json:"device_id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastSeenAt time.Time  `json:"last_seen_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	Current    bool       `json:"current"`
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	ds, err := s.Store.ListDevices(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]deviceView, 0, len(ds))
	for _, d := range ds {
		out = append(out, deviceView{ID: d.ID, DeviceID: d.DeviceID, Name: d.Name, CreatedAt: d.CreatedAt,
			LastSeenAt: d.LastSeenAt, RevokedAt: d.RevokedAt, Current: d.ID == p.DeviceID})
	}
	writeJSON(w, 200, map[string]any{"devices": out})
}

func (s *Server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Store.RevokeDevice(r.Context(), principal(r).UserID, id, time.Now()); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

// --- providers and connections ---

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	out := make([]providers.Spec, 0, len(providers.Specs))
	for _, sp := range providers.Specs {
		out = append(out, sp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, 200, map[string]any{"providers": out})
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	vs, err := s.Conn.List(r.Context(), principal(r).UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if vs == nil {
		vs = []connections.View{}
	}
	writeJSON(w, 200, map[string]any{"connections": vs})
}

func (s *Server) addConnection(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
		Label    string `json:"label"`
		BaseURL  string `json:"base_url"`
	}
	if !decode(w, r, 8<<10, &in) {
		return
	}
	v, err := s.Conn.Add(r.Context(), principal(r).UserID, connections.AddInput{Provider: in.Provider, Key: in.APIKey,
		Label: in.Label, BaseURL: in.BaseURL})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Conn.Delete(r.Context(), principal(r).UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) openrouterStart(w http.ResponseWriter, r *http.Request) {
	u, err := s.OAuth.Start(r.Context(), principal(r).UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]string{"url": u})
}

var oauthPage = template.Must(template.New("oauth").Parse(`<!doctype html>
<html lang="ru"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>AIHub</title><style>body{font:17px -apple-system,system-ui,sans-serif;margin:0;display:flex;min-height:100vh;align-items:center;justify-content:center;text-align:center;padding:24px}</style></head>
<body><div id="aihub-oauth" data-status="{{.Status}}"><h2>{{.Title}}</h2><p>{{.Text}}</p></div></body></html>`))

func (s *Server) openrouterCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	_, err := s.OAuth.Callback(r.Context(), q.Get("code"), q.Get("state"))
	data := struct{ Status, Title, Text string }{"ok", "OpenRouter подключён", "Можно вернуться в AIHub."}
	if err != nil {
		s.Log.Warn("openrouter oauth failed", "err", err)
		w.WriteHeader(400)
		data = struct{ Status, Title, Text string }{"error", "Не удалось подключить OpenRouter", err.Error()}
	}
	_ = oauthPage.Execute(w, data)
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	ms, errs, err := s.Conn.Catalog(r.Context(), principal(r).UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ms == nil {
		ms = []providers.Model{}
	}
	if errs == nil {
		errs = []connections.ProviderError{}
	}
	writeJSON(w, 200, map[string]any{"models": ms, "errors": errs})
}

// --- usage ---

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	days := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 366 {
		days = v
	}
	from := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	rows, err := s.Store.ListUsage(r.Context(), p.UserID, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	maxReq, maxTok, err := s.Chat.EffectiveLimits(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"usage": usageViews(rows), "limits": map[string]int64{"daily_requests": maxReq, "daily_tokens": maxTok}})
}

type usageView struct {
	UserID       int64  `json:"user_id,omitempty"`
	Provider     string `json:"provider"`
	Day          string `json:"day"`
	Requests     int64  `json:"requests"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
}

func usageViews(rows []store.UsageRow) []usageView {
	out := make([]usageView, 0, len(rows))
	for _, u := range rows {
		out = append(out, usageView{UserID: u.UserID, Provider: u.Provider, Day: u.Day, Requests: u.Requests,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens})
	}
	return out
}

// --- AIHub API keys ---

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.APIKeys.List(r.Context(), principal(r).UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if ks == nil {
		ks = []apikeys.View{}
	}
	writeJSON(w, 200, map[string]any{"keys": ks})
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	var in apikeys.CreateInput
	if !decode(w, r, 4<<10, &in) {
		return
	}
	full, v, err := s.APIKeys.Create(r.Context(), principal(r).UserID, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, map[string]any{"key": full, "info": v})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.APIKeys.Revoke(r.Context(), principal(r).UserID, id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}
