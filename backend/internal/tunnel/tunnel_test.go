package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"

	"github.com/ItzChelove4ik/AIHub/backend/internal/nodectl"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store/sqlite"
)

// fakeCtl records requests and plays node-install.sh.
type fakeCtl struct {
	mu       sync.Mutex
	reqs     []nodectl.Request
	busy     map[string]bool // "port=N" lines that fail as busy
	channels map[string]bool
}

func (f *fakeCtl) Run(_ context.Context, req nodectl.Request) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	switch req.Op {
	case "apply":
		if err := nodectl.ValidateConfig(req.Config); err != nil {
			return nil, err
		}
		for line := range f.busy {
			if strings.Contains(req.Config, line+"\n") {
				return nil, &nodectl.ScriptError{Step: "apply", Message: "порт занят"}
			}
		}
		name := strings.TrimPrefix(strings.SplitN(req.Config, "\n", 2)[0], "channel=")
		f.channels[name] = true
		return json.RawMessage(`{"ok":true}`), nil
	case "remove":
		delete(f.channels, req.Channel)
		return json.RawMessage(`{"ok":true}`), nil
	case "list":
		var parts []string
		for ch := range f.channels {
			parts = append(parts, `{"channel":"`+ch+`","state":"active","port":0}`)
		}
		return json.RawMessage(`{"ok":true,"channels":[` + strings.Join(parts, ",") + `]}`), nil
	}
	return nil, errors.New("unexpected op")
}

func newTunnel(t *testing.T, ctl nodectl.Client) (*Service, store.Store) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, _ := secure.NewBox(bytes.Repeat([]byte{3}, 32))
	s := New(db, box, ctl, Options{PublicHost: "aihub.example.com", PortMin: 20001, PortMax: 20003, CupsOnline: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.createRooms = func(context.Context) (string, error) { return "cm9vbXMtbGlzdC1hYmM", nil }
	return s, db
}

const yandexDoc = "https://docs.yandex.ru/edit/d/AbCdEfGhIjKlMnOpQrSt"

func TestCreateChannelLink(t *testing.T) {
	ctx := context.Background()
	ctl := &fakeCtl{channels: map[string]bool{}, busy: map[string]bool{"port=20001": true}}
	s, db := newTunnel(t, ctl)

	if _, err := s.AddCarrier(ctx, "vyandex", yandexDoc+"?share=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddCarrier(ctx, "vyandex", yandexDoc); err == nil {
		t.Fatal("duplicate document accepted")
	}
	var ve *ValidationError
	if _, err := s.AddCarrier(ctx, "vyandex", "https://evil.example.com/x"); !errors.As(err, &ve) {
		t.Fatalf("bad link: %v", err)
	}

	u := &store.User{Email: "a@example.com", PasswordHash: "x"}
	if err := db.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	dev, _ := db.UpsertDevice(ctx, u.ID, "dev", "iPhone", u.CreatedAt)

	iss, err := s.CreateForDevice(ctx, u.ID, dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if iss.Port == 20001 {
		t.Fatal("busy port used")
	}
	if strings.Join(iss.Transports, ",") != "vyandex,cupsonline,direct" || len(iss.QRPNG) == 0 {
		t.Fatalf("issued: %+v", iss.ChannelView)
	}

	// The link is a valid OpenFlux link with the channel's carriers, its
	// key and direct to our host as the backup.
	cfg, err := share.Decode(iss.Link)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Negotiate || len(cfg.Secret) != 64 || cfg.Name != LinkName {
		t.Fatalf("link config: %+v", cfg)
	}
	var types []string
	for _, tr := range cfg.Transports {
		types = append(types, tr.Type)
		if tr.Type == "direct" && !strings.HasSuffix(tr.Dial, ":"+itoa(iss.Port)) {
			t.Fatalf("direct dial %q", tr.Dial)
		}
		if tr.Type == "vyandex" && tr.URL != yandexDoc {
			t.Fatalf("doc url %q", tr.URL)
		}
	}
	if strings.Join(types, ",") != "vyandex,cupsonline,direct" {
		t.Fatalf("link transports %v", types)
	}
	wantCtx := provision.SessionContext([]provision.ChannelTransport{{Type: "vyandex", URL: yandexDoc}, {Type: "cupsonline", URL: "cm9vbXMtbGlzdC1hYmM"}})
	if cfg.Context != wantCtx {
		t.Fatalf("context %q, want %q", cfg.Context, wantCtx)
	}
	// The node got the same key as the link.
	var applied string
	for _, r := range ctl.reqs {
		if r.Op == "apply" && strings.Contains(r.Config, "port="+itoa(iss.Port)+"\n") {
			applied = r.Config
		}
	}
	if !strings.Contains(applied, "key="+cfg.Secret+"\n") || !strings.Contains(applied, "vyandex="+yandexDoc+"\n") {
		t.Fatalf("applied config does not match the link")
	}

	// One channel per device.
	if _, err := s.CreateForDevice(ctx, u.ID, dev.ID); !errors.Is(err, ErrChannelExists) {
		t.Fatalf("second channel: %v", err)
	}
	// The pool's only document is now assigned.
	cs, _ := s.ListCarriers(ctx)
	if cs[0].Status != "assigned" || cs[0].ChannelID == nil || *cs[0].ChannelID != iss.ID {
		t.Fatalf("carrier: %+v", cs[0])
	}
	list, _ := s.List(ctx, &u.ID)
	if len(list) != 1 || list[0].State != "active" {
		t.Fatalf("list: %+v", list)
	}

	// Rotation gives a new channel with a new key; delete frees the carrier.
	rot, err := s.Rotate(ctx, u.ID, false, iss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rot.Channel == iss.Channel || rot.Link == iss.Link {
		t.Fatal("rotation did not change the channel")
	}
	other := int64(9999)
	if err := s.Delete(ctx, other, false, rot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's channel: %v", err)
	}
	if err := s.Delete(ctx, u.ID, false, rot.ID); err != nil {
		t.Fatal(err)
	}
	cs, _ = s.ListCarriers(ctx)
	if cs[0].Status != "free" {
		t.Fatalf("carrier not released: %+v", cs[0])
	}
}

func TestInviteBinding(t *testing.T) {
	ctx := context.Background()
	s, db := newTunnel(t, &fakeCtl{channels: map[string]bool{}})
	s.opt.CupsOnline = false
	iss, err := s.CreateInvite(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if iss.InviteCode == "" || !iss.Invite {
		t.Fatalf("invite: %+v", iss)
	}
	if _, err := share.Decode(iss.Link); err != nil {
		t.Fatal(err)
	}
	id, err := s.CheckInvite(ctx, iss.InviteCode)
	if err != nil || id != iss.ID {
		t.Fatalf("check: %d %v", id, err)
	}
	u := &store.User{Email: "b@example.com", PasswordHash: "x"}
	db.CreateUser(ctx, u)
	dev, _ := db.UpsertDevice(ctx, u.ID, "dev", "", u.CreatedAt)
	if err := s.BindInvite(ctx, id, u.ID, dev.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CheckInvite(ctx, iss.InviteCode); err == nil {
		t.Fatal("invite reusable")
	}
	if _, err := s.CreateForDevice(ctx, u.ID, dev.ID); !errors.Is(err, ErrChannelExists) {
		t.Fatalf("bound invite should be the device's channel: %v", err)
	}
}

func TestDisabled(t *testing.T) {
	s, _ := newTunnel(t, nil)
	if _, err := s.CreateForDevice(context.Background(), 1, 1); !errors.Is(err, ErrDisabled) {
		t.Fatalf("%v", err)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
