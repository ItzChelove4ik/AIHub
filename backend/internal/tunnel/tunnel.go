// Package tunnel hands out OpenFlux channels. One channel (carriers + key
// + exit instance) serves one connected device at a time, so every device
// gets its own. Yandex and Mail.ru documents come from a pool the admin
// fills; cups.online rooms are created on the fly. The channel's
// openflux:// link carries its key: it is returned once and never logged
// or stored.
package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/p1neappleXpress/OpenFlux/provision"
	"github.com/p1neappleXpress/OpenFlux/share"
	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"

	"github.com/ItzChelove4ik/AIHub/backend/internal/nodectl"
	"github.com/ItzChelove4ik/AIHub/backend/internal/secure"
	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

const LinkName = "AIHub"

var (
	ErrDisabled      = errors.New("туннель на сервере не настроен (NODECTL_MODE, TUNNEL_PUBLIC_HOST)")
	ErrChannelExists = errors.New("у этого устройства уже есть канал: удалите или пересоздайте его")
	ErrNotFound      = errors.New("канал не найден")
	ErrNoPort        = errors.New("нет свободного порта для канала")
)

type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

type Options struct {
	PublicHost string
	PortMin    int
	PortMax    int
	CupsOnline bool
}

type Service struct {
	store store.Store
	box   *secure.Box
	ctl   nodectl.Client // nil: tunnel management off
	opt   Options
	log   *slog.Logger
	now   func() time.Time
	// createRooms makes cups.online rooms; replaced in tests.
	createRooms func(ctx context.Context) (string, error)
}

func New(s store.Store, box *secure.Box, ctl nodectl.Client, opt Options, log *slog.Logger) *Service {
	return &Service{store: s, box: box, ctl: ctl, opt: opt, log: log, now: time.Now, createRooms: cupsonline.CreateRoomList}
}

func (s *Service) Enabled() bool { return s.ctl != nil && s.opt.PublicHost != "" }

// --- carrier pool (admin) ---

type CarrierView struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Hint      string    `json:"hint"`
	Status    string    `json:"status"`
	ChannelID *int64    `json:"channel_id"`
	CreatedAt time.Time `json:"created_at"`
}

func carrierView(c store.Carrier) CarrierView {
	return CarrierView{ID: c.ID, Type: c.Type, Hint: c.Hint, Status: c.Status, ChannelID: c.ChannelID, CreatedAt: c.CreatedAt}
}

// AddCarrier puts a Yandex or Mail.ru document into the pool. The link is
// checked with OpenFlux's own rules and stored sealed.
func (s *Service) AddCarrier(ctx context.Context, typ, url string) (*CarrierView, error) {
	if typ != "vyandex" && typ != "mailru" {
		return nil, &ValidationError{"тип носителя: vyandex или mailru (cups.online создаётся автоматически)"}
	}
	ts, err := provision.CheckTransports([]provision.ChannelTransport{{Type: typ, URL: url}})
	if err != nil {
		return nil, &ValidationError{err.Error()}
	}
	clean := ts[0].URL
	sealed, err := s.box.SealString(clean, "carrier")
	if err != nil {
		return nil, err
	}
	c := &store.Carrier{Type: typ, SealedURL: sealed, URLHash: secure.HashToken(clean), Hint: "…" + secure.Last4(clean),
		Status: "free", CreatedAt: s.now()}
	if err := s.store.AddCarrier(ctx, c); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &ValidationError{"этот документ уже в пуле"}
		}
		return nil, err
	}
	v := carrierView(*c)
	return &v, nil
}

func (s *Service) ListCarriers(ctx context.Context) ([]CarrierView, error) {
	cs, err := s.store.ListCarriers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]CarrierView, 0, len(cs))
	for _, c := range cs {
		out = append(out, carrierView(c))
	}
	return out, nil
}

func (s *Service) DeleteCarrier(ctx context.Context, id int64) error {
	err := s.store.DeleteCarrier(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return &ValidationError{"носитель не найден или выдан каналу"}
	}
	return err
}

// SetCarrierStatus marks a carrier free or broken.
func (s *Service) SetCarrierStatus(ctx context.Context, id int64, status string) error {
	if status != "free" && status != "broken" {
		return &ValidationError{"статус: free или broken"}
	}
	return s.store.SetCarrierStatus(ctx, id, status)
}

// --- channels ---

type ChannelView struct {
	ID         int64     `json:"id"`
	Channel    string    `json:"channel"`
	Port       int       `json:"port"`
	Transports []string  `json:"transports"` // carrier types; direct is always there too
	UserID     *int64    `json:"user_id,omitempty"`
	DeviceID   *int64    `json:"device_id,omitempty"`
	Invite     bool      `json:"invite"`
	State      string    `json:"state"` // systemd state from node-install.sh, "unknown" when unreachable
	CreatedAt  time.Time `json:"created_at"`
}

// Issued is a new channel with its one-time link.
type Issued struct {
	ChannelView
	Link       string `json:"link"`
	QRPNG      []byte `json:"qr_png"`                // JSON: base64
	InviteCode string `json:"invite_code,omitempty"` // admin invites only
}

func channelView(c store.Channel, state string) ChannelView {
	var types []string
	if c.TransportTypes != "" {
		types = strings.Split(c.TransportTypes, ",")
	}
	types = append(types, "direct")
	return ChannelView{ID: c.ID, Channel: c.Channel, Port: c.Port, Transports: types, UserID: c.UserID, DeviceID: c.DeviceID,
		Invite: c.InviteHash != nil, State: state, CreatedAt: c.CreatedAt}
}

// CreateForDevice issues the device its channel.
func (s *Service) CreateForDevice(ctx context.Context, userID, deviceID int64) (*Issued, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	if _, err := s.store.ActiveChannelForDevice(ctx, deviceID); err == nil {
		return nil, ErrChannelExists
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return s.create(ctx, &userID, &deviceID, nil)
}

// CreateInvite issues a channel bound to no one yet, with a one-time
// invite code: the account registered with that code (on the device that
// imported the link) gets the channel.
func (s *Service) CreateInvite(ctx context.Context) (*Issued, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	code, err := secure.RandomString(24)
	if err != nil {
		return nil, err
	}
	hash := secure.HashToken(code)
	iss, err := s.create(ctx, nil, nil, &hash)
	if err != nil {
		return nil, err
	}
	iss.InviteCode = code
	return iss, nil
}

func (s *Service) create(ctx context.Context, userID, deviceID *int64, inviteHash *string) (iss *Issued, err error) {
	name, err := provision.NewChannelID()
	if err != nil {
		return nil, err
	}
	key, err := provision.NewKey()
	if err != nil {
		return nil, err
	}

	// Carriers from the pool, released again if anything below fails.
	var reserved []store.Carrier
	defer func() {
		if err != nil {
			for _, c := range reserved {
				_ = s.store.ReleaseCarrier(context.WithoutCancel(ctx), c.ID)
			}
		}
	}()
	var ts []provision.ChannelTransport
	for _, typ := range []string{"vyandex", "mailru"} {
		c, rerr := s.store.ReserveCarrier(ctx, typ)
		if errors.Is(rerr, store.ErrNotFound) {
			continue
		}
		if rerr != nil {
			return nil, rerr
		}
		reserved = append(reserved, *c)
		url, oerr := s.box.OpenString(c.SealedURL, "carrier")
		if oerr != nil {
			return nil, oerr
		}
		ts = append(ts, provision.ChannelTransport{Type: typ, URL: url})
	}
	if s.opt.CupsOnline && s.createRooms != nil {
		rctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		rooms, rerr := s.createRooms(rctx)
		cancel()
		if rerr != nil {
			s.log.Warn("cups.online rooms not created, channel goes without them", "err", rerr)
		} else {
			ts = append(ts, provision.ChannelTransport{Type: "cupsonline", URL: rooms})
		}
	}
	if ts, err = provision.CheckTransports(ts); err != nil {
		return nil, err
	}

	port, err := s.applyWithPort(ctx, name, key, ts)
	if err != nil {
		return nil, err
	}
	// From here the node runs: undo it if we cannot record it.
	defer func() {
		if err != nil {
			_, _ = s.ctl.Run(context.WithoutCancel(ctx), nodectl.Request{Op: "remove", Channel: name})
		}
	}()

	sealedTS, err := s.sealTransports(name, ts)
	if err != nil {
		return nil, err
	}
	types := make([]string, len(ts))
	for i, t := range ts {
		types[i] = t.Type
	}
	ch := &store.Channel{UserID: userID, DeviceID: deviceID, Channel: name, Port: port, TransportTypes: strings.Join(types, ","),
		SealedTransports: sealedTS, InviteHash: inviteHash, Status: "active", CreatedAt: s.now()}
	if err = s.store.CreateChannel(ctx, ch); err != nil {
		return nil, err
	}
	for _, c := range reserved {
		if err = s.store.AttachCarrier(ctx, c.ID, ch.ID); err != nil {
			return nil, err
		}
	}
	link, err := provision.ShareLink(LinkName, key, s.opt.PublicHost, port, ts)
	if err != nil {
		return nil, err
	}
	png, err := share.PNG(link, 512)
	if err != nil {
		return nil, err
	}
	s.log.Info("channel created", "channel", name, "port", port, "transports", ch.TransportTypes, "invite", inviteHash != nil)
	return &Issued{ChannelView: channelView(*ch, "active"), Link: link, QRPNG: png}, nil
}

func (s *Service) sealTransports(name string, ts []provision.ChannelTransport) ([]byte, error) {
	raw, err := json.Marshal(ts)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(raw, "channel:"+name)
}

// buildConfig is node-install.sh's config for the channel.
func buildConfig(name, key string, port int, ts []provision.ChannelTransport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "channel=%s\nkey=%s\nport=%d\n", name, key, port)
	for _, t := range ts {
		b.WriteString(t.Type + "=" + t.URL + "\n")
	}
	// The core updater keeps exits on the newest OpenFlux release, which
	// matters when carriers change their protocols.
	b.WriteString("autoupdate=yes\n")
	return b.String()
}

// applyWithPort installs the node on a free port from the range, trying
// another port when the script reports the chosen one busy.
func (s *Service) applyWithPort(ctx context.Context, name, key string, ts []provision.ChannelTransport) (int, error) {
	used, err := s.store.UsedPorts(ctx)
	if err != nil {
		return 0, err
	}
	span := s.opt.PortMax - s.opt.PortMin + 1
	start := rand.IntN(span)
	tries := 0
	for i := 0; i < span && tries < 4; i++ {
		port := s.opt.PortMin + (start+i)%span
		if used[port] {
			continue
		}
		tries++
		_, err := s.ctl.Run(ctx, nodectl.Request{Op: "apply", Config: buildConfig(name, key, port, ts)})
		if err == nil {
			return port, nil
		}
		var se *nodectl.ScriptError
		if errors.As(err, &se) && strings.Contains(se.Message, "порт") && strings.Contains(se.Message, "занят") {
			continue
		}
		return 0, fmt.Errorf("установка канала: %w", err)
	}
	return 0, ErrNoPort
}

// liveStates asks node-install.sh list for every channel's state.
func (s *Service) liveStates(ctx context.Context) map[string]string {
	if s.ctl == nil {
		return nil
	}
	out, err := s.ctl.Run(ctx, nodectl.Request{Op: "list"})
	if err != nil {
		s.log.Warn("node-install.sh list failed", "err", err)
		return nil
	}
	var l struct {
		Channels []struct {
			Channel string `json:"channel"`
			State   string `json:"state"`
		} `json:"channels"`
	}
	if json.Unmarshal(out, &l) != nil {
		return nil
	}
	m := make(map[string]string, len(l.Channels))
	for _, c := range l.Channels {
		m[c.Channel] = c.State
	}
	return m
}

// List returns the user's channels (all channels for userID nil) with the
// live state of each.
func (s *Service) List(ctx context.Context, userID *int64) ([]ChannelView, error) {
	cs, err := s.store.ListChannels(ctx, userID)
	if err != nil {
		return nil, err
	}
	states := s.liveStates(ctx)
	out := make([]ChannelView, 0, len(cs))
	for _, c := range cs {
		state := "unknown"
		if states != nil {
			if st, ok := states[c.Channel]; ok {
				state = st
			} else {
				state = "missing"
			}
		}
		out = append(out, channelView(c, state))
	}
	return out, nil
}

func (s *Service) owned(ctx context.Context, userID int64, admin bool, id int64) (*store.Channel, error) {
	c, err := s.store.ChannelByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && c.Status != "active") {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if !admin && (c.UserID == nil || *c.UserID != userID) {
		return nil, ErrNotFound
	}
	return c, nil
}

// Delete removes the channel's node and returns its carriers to the pool.
func (s *Service) Delete(ctx context.Context, userID int64, admin bool, id int64) error {
	if s.ctl == nil {
		return ErrDisabled
	}
	c, err := s.owned(ctx, userID, admin, id)
	if err != nil {
		return err
	}
	if _, err := s.ctl.Run(ctx, nodectl.Request{Op: "remove", Channel: c.Channel}); err != nil {
		var se *nodectl.ScriptError
		// Already gone from the server: still forget it here.
		if !(errors.As(err, &se) && strings.Contains(se.Message, "нет на сервере")) {
			return fmt.Errorf("удаление канала: %w", err)
		}
	}
	if err := s.store.SetChannelStatus(ctx, c.ID, "removed"); err != nil {
		return err
	}
	s.log.Info("channel removed", "channel", c.Channel)
	return s.store.ReleaseChannelCarriers(ctx, c.ID)
}

// Rotate replaces the channel with a new one (new name and key) for the
// same owner: the only way to change a channel's key.
func (s *Service) Rotate(ctx context.Context, userID int64, admin bool, id int64) (*Issued, error) {
	if !s.Enabled() {
		return nil, ErrDisabled
	}
	c, err := s.owned(ctx, userID, admin, id)
	if err != nil {
		return nil, err
	}
	if err := s.Delete(ctx, userID, admin, id); err != nil {
		return nil, err
	}
	if c.InviteHash != nil { // an unused invite: issue a fresh invite
		return s.CreateInvite(ctx)
	}
	return s.create(ctx, c.UserID, c.DeviceID, nil)
}

// CheckInvite implements auth.InviteBinder.
func (s *Service) CheckInvite(ctx context.Context, code string) (int64, error) {
	c, err := s.store.ChannelByInvite(ctx, secure.HashToken(strings.TrimSpace(code)))
	if err != nil {
		return 0, err
	}
	return c.ID, nil
}

// BindInvite implements auth.InviteBinder.
func (s *Service) BindInvite(ctx context.Context, channelID, userID, deviceID int64) error {
	return s.store.BindChannel(ctx, channelID, userID, deviceID)
}
