package auth

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/store/sqlite"
)

func newService(t *testing.T, adminEmail, registration string) *Service {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return New(db, bytes.Repeat([]byte{1}, 32), adminEmail, registration)
}

const pw = "a-long-password-1"

func TestRegisterFirstIsAdmin(t *testing.T) {
	ctx := context.Background()
	s := newService(t, "", "open")
	u1, tok, err := s.Register(ctx, RegisterInput{Email: "One@Example.com", Password: pw, DeviceID: "dev-1", DeviceName: "iPhone"})
	if err != nil {
		t.Fatal(err)
	}
	if !u1.IsAdmin || u1.Email != "one@example.com" || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("first user: %+v %+v", u1, tok)
	}
	u2, _, err := s.Register(ctx, RegisterInput{Email: "two@example.com", Password: pw, DeviceID: "dev-2"})
	if err != nil || u2.IsAdmin {
		t.Fatalf("second user: %+v %v", u2, err)
	}
	if _, _, err := s.Register(ctx, RegisterInput{Email: "one@example.com", Password: pw, DeviceID: "d"}); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("duplicate email: %v", err)
	}
	var ve *ValidationError
	if _, _, err := s.Register(ctx, RegisterInput{Email: "x@example.com", Password: "short", DeviceID: "d"}); !errors.As(err, &ve) {
		t.Fatalf("short password: %v", err)
	}
	if _, _, err := s.Register(ctx, RegisterInput{Email: "not-an-email", Password: pw, DeviceID: "d"}); !errors.As(err, &ve) {
		t.Fatalf("bad email: %v", err)
	}
}

func TestAdminEmailAndInviteOnly(t *testing.T) {
	ctx := context.Background()
	s := newService(t, "boss@example.com", "invite")
	u, _, err := s.Register(ctx, RegisterInput{Email: "first@example.com", Password: pw, DeviceID: "d"})
	if err != nil || u.IsAdmin {
		t.Fatalf("with ADMIN_EMAIL the first user is not admin: %+v %v", u, err)
	}
	if _, _, err := s.Register(ctx, RegisterInput{Email: "second@example.com", Password: pw, DeviceID: "d"}); !errors.Is(err, ErrInviteRequired) {
		t.Fatalf("invite-only: %v", err)
	}
	boss, _, err := s.Register(ctx, RegisterInput{Email: "boss@example.com", Password: pw, DeviceID: "d"})
	if err != nil || !boss.IsAdmin {
		t.Fatalf("admin email: %+v %v", boss, err)
	}
}

func TestLoginAuthenticateRefreshLogout(t *testing.T) {
	ctx := context.Background()
	s := newService(t, "", "open")
	if _, _, err := s.Register(ctx, RegisterInput{Email: "a@example.com", Password: pw, DeviceID: "dev"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Login(ctx, LoginInput{Email: "a@example.com", Password: "wrong-password", DeviceID: "dev"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := s.Login(ctx, LoginInput{Email: "nobody@example.com", Password: pw, DeviceID: "dev"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown email: %v", err)
	}
	_, tok, err := s.Login(ctx, LoginInput{Email: "A@example.com", Password: pw, DeviceID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(ctx, tok.AccessToken)
	if err != nil || p.UserID == 0 || p.DeviceID == 0 || !p.Admin {
		t.Fatalf("authenticate: %+v %v", p, err)
	}
	if _, err := s.Authenticate(ctx, tok.AccessToken+"x"); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("tampered token: %v", err)
	}

	// Rotation: the new refresh token works, the old one is now a leak
	// signal that revokes the device's tokens.
	tok2, err := s.Refresh(ctx, tok.RefreshToken)
	if err != nil || tok2.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := s.Refresh(ctx, tok.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("reuse of rotated token: %v", err)
	}
	if _, err := s.Refresh(ctx, tok2.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("after reuse detection the whole device is signed out: %v", err)
	}

	_, tok3, err := s.Login(ctx, LoginInput{Email: "a@example.com", Password: pw, DeviceID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	p3, _ := s.Authenticate(ctx, tok3.AccessToken)
	if err := s.Logout(ctx, p3); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Refresh(ctx, tok3.RefreshToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("refresh after logout: %v", err)
	}

	// Revoking the device stops its access token at once.
	if err := s.store.RevokeDevice(ctx, p3.UserID, p3.DeviceID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok3.AccessToken); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("revoked device: %v", err)
	}
}

func TestAccessTokenExpires(t *testing.T) {
	ctx := context.Background()
	s := newService(t, "", "open")
	_, tok, err := s.Register(ctx, RegisterInput{Email: "a@example.com", Password: pw, DeviceID: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Now().Add(AccessTTL + time.Minute) }
	if _, err := s.Authenticate(ctx, tok.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestBruteForceLock(t *testing.T) {
	ctx := context.Background()
	s := newService(t, "", "open")
	if _, _, err := s.Register(ctx, RegisterInput{Email: "a@example.com", Password: pw, DeviceID: "dev"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s.Login(ctx, LoginInput{Email: "a@example.com", Password: "nope-nope-nope", DeviceID: "dev"})
	}
	if _, _, err := s.Login(ctx, LoginInput{Email: "a@example.com", Password: pw, DeviceID: "dev"}); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected lock, got %v", err)
	}
	s.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	if _, _, err := s.Login(ctx, LoginInput{Email: "a@example.com", Password: pw, DeviceID: "dev"}); err != nil {
		t.Fatalf("lock should expire: %v", err)
	}
}
