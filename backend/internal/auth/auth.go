// Package auth handles AIHub accounts: registration, sign-in with
// argon2id passwords, short-lived access JWTs and rotating refresh tokens
// bound to a device.
package auth

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 60 * 24 * time.Hour
	issuer     = "aihub"
)

var (
	ErrInvalidCredentials = errors.New("неверный email или пароль")
	ErrLocked             = errors.New("слишком много неудачных попыток входа, попробуйте позже")
	ErrEmailTaken         = errors.New("этот email уже зарегистрирован")
	ErrInvalidToken       = errors.New("токен недействителен")
	ErrDeviceRevoked      = errors.New("устройство отозвано")
	ErrInviteRequired     = errors.New("регистрация только по приглашению")
	ErrInvalidInvite      = errors.New("приглашение недействительно")
)

// ValidationError is a bad request field.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

// Claims are the access token's claims.
type Claims struct {
	Device int64 `json:"did"`
	Admin  bool  `json:"adm,omitempty"`
	jwt.RegisteredClaims
}

// Principal is who an authenticated request comes from.
type Principal struct {
	UserID   int64
	DeviceID int64 // devices.id; 0 for AIHub API keys
	Admin    bool
	APIKey   *store.APIKey
}

// InviteBinder binds an admin's invite (an openflux:// channel handed out
// before the account existed) to a new account. The tunnel service
// implements it.
type InviteBinder interface {
	// CheckInvite reports the channel row of an unused invite code.
	CheckInvite(ctx context.Context, code string) (int64, error)
	BindInvite(ctx context.Context, channelID, userID, deviceID int64) error
}

type Service struct {
	store        store.Store
	jwtKey       []byte
	adminEmail   string
	registration string // open | invite
	invites      InviteBinder
	now          func() time.Time
	guard        *loginGuard
	registerMu   sync.Mutex
}

func New(s store.Store, masterKey []byte, adminEmail, registration string) *Service {
	return &Service{
		store:        s,
		jwtKey:       secure.DeriveKey(masterKey, "jwt"),
		adminEmail:   strings.ToLower(strings.TrimSpace(adminEmail)),
		registration: registration,
		now:          time.Now,
		guard:        newLoginGuard(5, 15*time.Minute),
	}
}

// SetInviteBinder connects the tunnel service (it is created later).
func (s *Service) SetInviteBinder(b InviteBinder) { s.invites = b }

// Tokens are what sign-in and refresh return.
type Tokens struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	TokenType    string    `json:"token_type"`
}

type RegisterInput struct {
	Email      string
	Password   string
	DeviceID   string
	DeviceName string
	InviteCode string
}

func normalizeEmail(e string) (string, error) {
	e = strings.ToLower(strings.TrimSpace(e))
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || len(e) > 254 {
		return "", &ValidationError{"некорректный email"}
	}
	return e, nil
}

func checkPassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < 10 {
		return &ValidationError{"пароль должен быть не короче 10 символов"}
	}
	if len(pw) > 256 {
		return &ValidationError{"пароль слишком длинный"}
	}
	return nil
}

func checkDevice(id, name string) (string, string, error) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	if id == "" || len(id) > 128 {
		return "", "", &ValidationError{"нужен device_id (до 128 символов)"}
	}
	if len(name) > 128 {
		name = name[:128]
	}
	return id, name, nil
}

// Register creates an account and signs it in on the device. The first
// account (or ADMIN_EMAIL) is admin.
func (s *Service) Register(ctx context.Context, in RegisterInput) (*store.User, *Tokens, error) {
	email, err := normalizeEmail(in.Email)
	if err != nil {
		return nil, nil, err
	}
	if err := checkPassword(in.Password); err != nil {
		return nil, nil, err
	}
	deviceID, deviceName, err := checkDevice(in.DeviceID, in.DeviceName)
	if err != nil {
		return nil, nil, err
	}
	hash, err := secure.HashPassword(in.Password)
	if err != nil {
		return nil, nil, err
	}

	// Serialise registrations so "the first user is admin" holds.
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	count, err := s.store.CountUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	first := count == 0
	isAdmin := (s.adminEmail == "" && first) || (s.adminEmail != "" && email == s.adminEmail)

	var inviteChannel int64
	if code := strings.TrimSpace(in.InviteCode); code != "" {
		if s.invites == nil {
			return nil, nil, ErrInvalidInvite
		}
		if inviteChannel, err = s.invites.CheckInvite(ctx, code); err != nil {
			return nil, nil, ErrInvalidInvite
		}
	} else if s.registration == "invite" && !isAdmin && (!first || s.adminEmail != "") {
		// Invite-only: besides invitees, only the admin may register (the
		// ADMIN_EMAIL account, or without it the very first account).
		return nil, nil, ErrInviteRequired
	}

	u := &store.User{Email: email, PasswordHash: hash, IsAdmin: isAdmin, CreatedAt: s.now()}
	if err := s.store.CreateUser(ctx, u); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, nil, ErrEmailTaken
		}
		return nil, nil, err
	}
	dev, err := s.store.UpsertDevice(ctx, u.ID, deviceID, deviceName, s.now())
	if err != nil {
		return nil, nil, err
	}
	if inviteChannel != 0 {
		if err := s.invites.BindInvite(ctx, inviteChannel, u.ID, dev.ID); err != nil {
			return nil, nil, fmt.Errorf("bind invite: %w", err)
		}
	}
	tok, err := s.issue(ctx, u, dev.ID)
	return u, tok, err
}

type LoginInput struct {
	Email      string
	Password   string
	DeviceID   string
	DeviceName string
}

// Login checks the password and signs the device in.
func (s *Service) Login(ctx context.Context, in LoginInput) (*store.User, *Tokens, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))
	deviceID, deviceName, err := checkDevice(in.DeviceID, in.DeviceName)
	if err != nil {
		return nil, nil, err
	}
	if !s.guard.allowed(email, s.now()) {
		return nil, nil, ErrLocked
	}
	u, err := s.store.UserByEmail(ctx, email)
	if errors.Is(err, store.ErrNotFound) {
		secure.DummyPasswordCheck(in.Password)
		s.guard.fail(email, s.now())
		return nil, nil, ErrInvalidCredentials
	}
	if err != nil {
		return nil, nil, err
	}
	if !secure.VerifyPassword(in.Password, u.PasswordHash) {
		s.guard.fail(email, s.now())
		return nil, nil, ErrInvalidCredentials
	}
	s.guard.success(email)
	dev, err := s.store.UpsertDevice(ctx, u.ID, deviceID, deviceName, s.now())
	if err != nil {
		return nil, nil, err
	}
	tok, err := s.issue(ctx, u, dev.ID)
	return u, tok, err
}

func (s *Service) issue(ctx context.Context, u *store.User, deviceID int64) (*Tokens, error) {
	now := s.now()
	access, exp, err := s.signAccess(u, deviceID, now)
	if err != nil {
		return nil, err
	}
	raw, err := secure.RandomURLToken(32)
	if err != nil {
		return nil, err
	}
	rt := &store.RefreshToken{UserID: u.ID, DeviceID: deviceID, Hash: secure.HashToken(raw), ExpiresAt: now.Add(RefreshTTL)}
	if err := s.store.CreateRefreshToken(ctx, rt); err != nil {
		return nil, err
	}
	return &Tokens{AccessToken: access, RefreshToken: raw, ExpiresAt: exp, TokenType: "Bearer"}, nil
}

func (s *Service) signAccess(u *store.User, deviceID int64, now time.Time) (string, time.Time, error) {
	exp := now.Add(AccessTTL)
	claims := Claims{
		Device: deviceID,
		Admin:  u.IsAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Subject:   fmt.Sprint(u.ID),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(s.jwtKey)
	return tok, exp, err
}

// Refresh rotates a refresh token. Presenting a token that was already
// rotated means it leaked: every token of that device is revoked.
func (s *Service) Refresh(ctx context.Context, raw string) (*Tokens, error) {
	now := s.now()
	rt, err := s.store.RefreshTokenByHash(ctx, secure.HashToken(strings.TrimSpace(raw)))
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, err
	}
	if rt.RevokedAt != nil {
		if rt.ReplacedBy != nil {
			_ = s.store.RevokeDeviceTokens(ctx, rt.DeviceID, now)
		}
		return nil, ErrInvalidToken
	}
	if now.After(rt.ExpiresAt) {
		return nil, ErrInvalidToken
	}
	dev, err := s.store.DeviceByID(ctx, rt.DeviceID)
	if err != nil {
		return nil, err
	}
	if dev.RevokedAt != nil {
		return nil, ErrDeviceRevoked
	}
	u, err := s.store.UserByID(ctx, rt.UserID)
	if err != nil {
		return nil, err
	}
	access, exp, err := s.signAccess(u, dev.ID, now)
	if err != nil {
		return nil, err
	}
	next, err := secure.RandomURLToken(32)
	if err != nil {
		return nil, err
	}
	nrt := &store.RefreshToken{UserID: u.ID, DeviceID: dev.ID, Hash: secure.HashToken(next), ExpiresAt: now.Add(RefreshTTL)}
	if err := s.store.RotateRefreshToken(ctx, rt.ID, nrt, now); err != nil {
		if errors.Is(err, store.ErrNotFound) { // lost a race with another refresh
			return nil, ErrInvalidToken
		}
		return nil, err
	}
	_ = s.store.TouchDevice(ctx, dev.ID, now)
	return &Tokens{AccessToken: access, RefreshToken: next, ExpiresAt: exp, TokenType: "Bearer"}, nil
}

// Logout revokes the device's refresh tokens (the access token expires on
// its own within 15 minutes, and the device check below stops it sooner
// when the device is revoked).
func (s *Service) Logout(ctx context.Context, p *Principal) error {
	return s.store.RevokeDeviceTokens(ctx, p.DeviceID, s.now())
}

// Authenticate checks an access token and that its device is still live.
func (s *Service) Authenticate(ctx context.Context, token string) (*Principal, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) { return s.jwtKey, nil },
		jwt.WithValidMethods([]string{"HS256"}), jwt.WithIssuer(issuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(s.now))
	if err != nil {
		return nil, ErrInvalidToken
	}
	var uid int64
	if _, err := fmt.Sscan(claims.Subject, &uid); err != nil || uid <= 0 {
		return nil, ErrInvalidToken
	}
	dev, err := s.store.DeviceByID(ctx, claims.Device)
	if err != nil || dev.UserID != uid {
		return nil, ErrInvalidToken
	}
	if dev.RevokedAt != nil {
		return nil, ErrDeviceRevoked
	}
	return &Principal{UserID: uid, DeviceID: dev.ID, Admin: claims.Admin}, nil
}

// --- brute-force guard ---

// loginGuard locks an email after max failed attempts within window.
type loginGuard struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newLoginGuard(max int, window time.Duration) *loginGuard {
	return &loginGuard{max: max, window: window, fails: map[string][]time.Time{}}
}

func (g *loginGuard) recent(email string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range g.fails[email] {
		if now.Sub(t) < g.window {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(g.fails, email)
	} else {
		g.fails[email] = keep
	}
	return keep
}

func (g *loginGuard) allowed(email string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.recent(email, now)) < g.max
}

func (g *loginGuard) fail(email string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fails[email] = append(g.recent(email, now), now)
	if len(g.fails) > 100000 { // bound memory under a spray attack
		for k := range g.fails {
			delete(g.fails, k)
			break
		}
	}
}

func (g *loginGuard) success(email string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.fails, email)
}
