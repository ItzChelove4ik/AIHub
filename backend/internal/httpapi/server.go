// Package httpapi is AIHub's HTTP API (see docs/api.yaml).
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ItzChelove4ik/AIHub/backend/internal/apikeys"
	"github.com/ItzChelove4ik/AIHub/backend/internal/auth"
	"github.com/ItzChelove4ik/AIHub/backend/internal/chat"
	"github.com/ItzChelove4ik/AIHub/backend/internal/connections"
	"github.com/ItzChelove4ik/AIHub/backend/internal/providers"
	"github.com/ItzChelove4ik/AIHub/backend/internal/ratelimit"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
	"github.com/ItzChelove4ik/AIHub/backend/internal/tunnel"
)

type Server struct {
	Store      store.Store
	Auth       *auth.Service
	Conn       *connections.Service
	OAuth      *connections.OpenRouterOAuth
	Chat       *chat.Service
	APIKeys    *apikeys.Service
	Tunnel     *tunnel.Service
	Log        *slog.Logger
	TrustProxy bool
	Version    string

	authLimiter *ratelimit.Keyed
}

// Handler builds the router.
func (s *Server) Handler() http.Handler {
	s.authLimiter = ratelimit.New(20, 10)
	r := chi.NewRouter()
	r.Use(s.recoverer, s.accessLog)

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	r.Get("/readyz", s.readyz)

	r.Route("/v1", func(r chi.Router) {
		r.Group(func(r chi.Router) {
			r.Use(s.rateLimitAuth)
			r.Post("/auth/register", s.register)
			r.Post("/auth/login", s.login)
			r.Post("/auth/refresh", s.refresh)
		})
		r.Get("/oauth/openrouter/callback", s.openrouterCallback)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Post("/auth/logout", s.logout)
			r.Get("/me", s.me)
			r.Get("/devices", s.listDevices)
			r.Delete("/devices/{id}", s.revokeDevice)

			r.Get("/providers", s.listProviders)
			r.Get("/connections", s.listConnections)
			r.Post("/connections", s.addConnection)
			r.Delete("/connections/{id}", s.deleteConnection)
			r.Get("/oauth/openrouter/start", s.openrouterStart)

			r.Get("/models", s.models)
			r.Post("/chat", s.chat)
			r.Get("/conversations", s.listConversations)
			r.Get("/conversations/{id}", s.getConversation)
			r.Patch("/conversations/{id}", s.renameConversation)
			r.Delete("/conversations/{id}", s.deleteConversation)
			r.Get("/usage", s.usage)

			r.Get("/apikeys", s.listAPIKeys)
			r.Post("/apikeys", s.createAPIKey)
			r.Delete("/apikeys/{id}", s.revokeAPIKey)

			r.Get("/tunnel/channels", s.listChannels)
			r.Post("/tunnel/channels", s.createChannel)
			r.Delete("/tunnel/channels/{id}", s.deleteChannel)
			r.Post("/tunnel/channels/{id}/rotate", s.rotateChannel)

			r.Route("/admin", func(r chi.Router) {
				r.Use(s.requireAdmin)
				r.Get("/users", s.adminUsers)
				r.Get("/users/{id}/limits", s.adminGetLimits)
				r.Put("/users/{id}/limits", s.adminSetLimits)
				r.Get("/usage", s.adminUsage)
				r.Get("/tunnel/carriers", s.adminListCarriers)
				r.Post("/tunnel/carriers", s.adminAddCarrier)
				r.Patch("/tunnel/carriers/{id}", s.adminSetCarrier)
				r.Delete("/tunnel/carriers/{id}", s.adminDeleteCarrier)
				r.Get("/tunnel/channels", s.adminListChannels)
				r.Post("/tunnel/invite", s.adminInvite)
			})
		})
	})

	r.Route("/openai/v1", func(r chi.Router) {
		r.Get("/models", s.openaiModels)
		r.Post("/chat/completions", s.openaiChat)
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 404, "not_found", "нет такого метода")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, 405, "method_not_allowed", "метод не поддерживается")
	})
	return r
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		writeError(w, 503, "not_ready", "база данных недоступна")
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ready", "version": s.Version, "tunnel": s.Tunnel.Enabled()})
}

// --- middleware ---

type ctxKey int

const principalKey ctxKey = 1

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(principalKey).(*auth.Principal)
	return p
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeError(w, 401, "unauthorized", "нужен токен доступа")
			return
		}
		p, err := s.Auth.Authenticate(r.Context(), tok)
		if err != nil {
			code := "unauthorized"
			if errors.Is(err, auth.ErrDeviceRevoked) {
				code = "device_revoked"
			}
			writeError(w, 401, code, err.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

// requireAdmin checks the database, not only the token's claim, so taking
// admin rights away works at once.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principal(r)
		u, err := s.Store.UserByID(r.Context(), p.UserID)
		if err != nil || !u.IsAdmin {
			writeError(w, 403, "forbidden", "нужны права администратора")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) rateLimitAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authLimiter.Allow(s.clientIP(r)) {
			w.Header().Set("Retry-After", "30")
			writeError(w, 429, "rate_limited", "слишком много запросов, попробуйте позже")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog logs method, route, status and duration. Never query strings
// (OAuth codes) or bodies (keys, passwords, links).
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		route := r.URL.Path
		if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
			route = rc.RoutePattern()
		}
		s.Log.Info("http", "method", r.Method, "route", route, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.Log.Error("panic", "value", v, "stack", string(debug.Stack()))
				writeError(w, 500, "internal", "внутренняя ошибка")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// --- helpers ---

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// decode reads a JSON body of at most limit bytes, rejecting unknown fields.
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeError(w, 413, "too_large", "слишком большой запрос")
		} else {
			writeError(w, 400, "bad_json", "некорректный JSON: "+err.Error())
		}
		return false
	}
	if dec.More() {
		writeError(w, 400, "bad_json", "лишние данные после JSON")
		return false
	}
	_, _ = io.Copy(io.Discard, r.Body)
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "bad_id", "некорректный id")
		return 0, false
	}
	return id, true
}

// fail maps service errors to responses; unexpected ones are logged and
// reported without detail.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var (
		authVal *auth.ValidationError
		connVal *connections.ValidationError
		chatVal *chat.ValidationError
		keyVal  *apikeys.ValidationError
		tunVal  *tunnel.ValidationError
		provErr *providers.Error
	)
	switch {
	case errors.As(err, &authVal), errors.As(err, &connVal), errors.As(err, &chatVal), errors.As(err, &keyVal), errors.As(err, &tunVal):
		writeError(w, 400, "invalid", err.Error())
	case errors.Is(err, auth.ErrInvalidCredentials), errors.Is(err, auth.ErrInvalidToken):
		writeError(w, 401, "unauthorized", err.Error())
	case errors.Is(err, auth.ErrDeviceRevoked):
		writeError(w, 401, "device_revoked", err.Error())
	case errors.Is(err, auth.ErrLocked):
		writeError(w, 429, "locked", err.Error())
	case errors.Is(err, auth.ErrEmailTaken):
		writeError(w, 409, "email_taken", err.Error())
	case errors.Is(err, auth.ErrInviteRequired), errors.Is(err, auth.ErrInvalidInvite):
		writeError(w, 403, "invite", err.Error())
	case errors.Is(err, connections.ErrUnknownProvider):
		writeError(w, 400, "unknown_provider", err.Error())
	case errors.Is(err, connections.ErrNotConnected):
		writeError(w, 400, "not_connected", err.Error())
	case errors.Is(err, connections.ErrOAuthState):
		writeError(w, 400, "oauth_state", err.Error())
	case errors.Is(err, chat.ErrLimitExceeded):
		writeError(w, 429, "limit_exceeded", err.Error())
	case errors.Is(err, chat.ErrNotFound), errors.Is(err, tunnel.ErrNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, 404, "not_found", "не найдено")
	case errors.Is(err, tunnel.ErrDisabled):
		writeError(w, 503, "tunnel_disabled", err.Error())
	case errors.Is(err, tunnel.ErrChannelExists):
		writeError(w, 409, "channel_exists", err.Error())
	case errors.Is(err, tunnel.ErrNoPort):
		writeError(w, 503, "no_port", err.Error())
	case errors.As(err, &provErr):
		status := 502
		if provErr.Unauthorized() {
			status = 400
		}
		writeError(w, status, "provider_error", provErr.Error())
	case errors.Is(err, context.Canceled):
		// client went away
	default:
		s.Log.Error("request failed", "route", r.URL.Path, "err", err)
		writeError(w, 500, "internal", "внутренняя ошибка")
	}
}
