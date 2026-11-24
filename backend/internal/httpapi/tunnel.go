package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
	"github.com/ItzChelove4ik/AIHub/backend/internal/tunnel"
)

// --- channels (every user, for their devices) ---

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	uid := principal(r).UserID
	cs, err := s.Tunnel.List(r.Context(), &uid)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []tunnel.ChannelView{}
	}
	writeJSON(w, 200, map[string]any{"channels": cs, "enabled": s.Tunnel.Enabled()})
}

// createChannel issues the calling device its channel. The response holds
// the openflux:// link (it carries the channel key) and is not stored or
// logged anywhere: the device keeps it in its Keychain.
func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	iss, err := s.Tunnel.CreateForDevice(r.Context(), p.UserID, p.DeviceID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, iss)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := principal(r)
	if err := s.Tunnel.Delete(r.Context(), p.UserID, s.isAdmin(r), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) rotateChannel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := principal(r)
	iss, err := s.Tunnel.Rotate(r.Context(), p.UserID, s.isAdmin(r), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, iss)
}

func (s *Server) isAdmin(r *http.Request) bool {
	u, err := s.Store.UserByID(r.Context(), principal(r).UserID)
	return err == nil && u.IsAdmin
}

// --- admin ---

func (s *Server) adminUsers(w http.ResponseWriter, r *http.Request) {
	us, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := make([]userView, 0, len(us))
	for i := range us {
		out = append(out, toUserView(&us[i]))
	}
	writeJSON(w, 200, map[string]any{"users": out})
}

type limitsBody struct {
	DailyRequests *int64 `json:"daily_requests"`
	DailyTokens   *int64 `json:"daily_tokens"`
}

func (s *Server) adminGetLimits(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := s.Store.GetLimits(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	req, tok, err := s.Chat.EffectiveLimits(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"limits": limitsBody{DailyRequests: l.DailyRequests, DailyTokens: l.DailyTokens},
		"effective": map[string]int64{"daily_requests": req, "daily_tokens": tok}})
}

// adminSetLimits sets a user's daily limits: null = server default,
// 0 = unlimited.
func (s *Server) adminSetLimits(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in limitsBody
	if !decode(w, r, 1<<10, &in) {
		return
	}
	if (in.DailyRequests != nil && *in.DailyRequests < 0) || (in.DailyTokens != nil && *in.DailyTokens < 0) {
		writeError(w, 400, "invalid", "лимиты не могут быть отрицательными")
		return
	}
	if _, err := s.Store.UserByID(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.SetLimits(r.Context(), id, store.Limits{DailyRequests: in.DailyRequests, DailyTokens: in.DailyTokens}); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, in)
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && v > 0 && v <= 366 {
		days = v
	}
	var uid int64
	if v, err := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64); err == nil {
		uid = v
	}
	from := time.Now().UTC().AddDate(0, 0, -days+1).Format("2006-01-02")
	rows, err := s.Store.ListUsage(r.Context(), uid, from)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 200, map[string]any{"usage": usageViews(rows)})
}

func (s *Server) adminListCarriers(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Tunnel.ListCarriers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []tunnel.CarrierView{}
	}
	writeJSON(w, 200, map[string]any{"carriers": cs})
}

func (s *Server) adminAddCarrier(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if !decode(w, r, 4<<10, &in) {
		return
	}
	v, err := s.Tunnel.AddCarrier(r.Context(), in.Type, in.URL)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) adminSetCarrier(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if !decode(w, r, 1<<10, &in) {
		return
	}
	if err := s.Tunnel.SetCarrierStatus(r.Context(), id, in.Status); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) adminDeleteCarrier(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.Tunnel.DeleteCarrier(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) adminListChannels(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Tunnel.List(r.Context(), nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if cs == nil {
		cs = []tunnel.ChannelView{}
	}
	writeJSON(w, 200, map[string]any{"channels": cs, "enabled": s.Tunnel.Enabled()})
}

// adminInvite issues a channel for a person who has no account yet: the
// link (and QR) gets their phone through the whitelist, and registering
// with the invite code binds the channel to their account.
func (s *Server) adminInvite(w http.ResponseWriter, r *http.Request) {
	iss, err := s.Tunnel.CreateInvite(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, 201, iss)
}
