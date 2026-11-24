// Package sqlite implements store.Store on SQLite (modernc.org/sqlite, pure
// Go, no cgo).
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct{ db *sql.DB }

var _ store.Store = (*DB)(nil)

// Open opens (creating if needed) the database file and migrates it.
func Open(ctx context.Context, path string) (*DB, error) {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One writer at a time is what SQLite does anyway; a single connection
	// keeps transactions simple and avoids SQLITE_BUSY between our own
	// connections.
	db.SetMaxOpenConns(1)
	s := &DB{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *DB) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)`, version, ms(time.Now())); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *DB) Close() error                   { return s.db.Close() }
func (s *DB) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// --- helpers ---

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func fromNullMS(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := fromMS(v.Int64)
	return &t
}

func nullInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	return err
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// --- users ---

func (s *DB) CountUsers(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

func (s *DB) CreateUser(ctx context.Context, u *store.User) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO users(email, password_hash, is_admin, created_at) VALUES(?,?,?,?)`,
		u.Email, u.PasswordHash, u.IsAdmin, ms(u.CreatedAt))
	if isUnique(err) {
		return store.ErrConflict
	}
	if err != nil {
		return err
	}
	u.ID, err = res.LastInsertId()
	return err
}

const userCols = `id, email, password_hash, is_admin, created_at`

func scanUser(row interface{ Scan(...any) error }) (*store.User, error) {
	var u store.User
	var created int64
	if err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.IsAdmin, &created); err != nil {
		return nil, notFound(err)
	}
	u.CreatedAt = fromMS(created)
	return &u, nil
}

func (s *DB) UserByEmail(ctx context.Context, email string) (*store.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
}

func (s *DB) UserByID(ctx context.Context, id int64) (*store.User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

func (s *DB) ListUsers(ctx context.Context) ([]store.User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// --- devices ---

const deviceCols = `id, user_id, device_id, name, created_at, last_seen_at, revoked_at`

func scanDevice(row interface{ Scan(...any) error }) (*store.Device, error) {
	var d store.Device
	var created, seen int64
	var revoked sql.NullInt64
	if err := row.Scan(&d.ID, &d.UserID, &d.DeviceID, &d.Name, &created, &seen, &revoked); err != nil {
		return nil, notFound(err)
	}
	d.CreatedAt, d.LastSeenAt, d.RevokedAt = fromMS(created), fromMS(seen), fromNullMS(revoked)
	return &d, nil
}

// UpsertDevice registers a device or, for a known one, refreshes its name
// and last sign-in and lifts an earlier revocation (signing in again with
// the password is a new grant).
func (s *DB) UpsertDevice(ctx context.Context, userID int64, deviceID, name string, now time.Time) (*store.Device, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO devices(user_id, device_id, name, created_at, last_seen_at) VALUES(?,?,?,?,?)
		ON CONFLICT(user_id, device_id) DO UPDATE SET name = excluded.name, last_seen_at = excluded.last_seen_at, revoked_at = NULL`,
		userID, deviceID, name, ms(now), ms(now))
	if err != nil {
		return nil, err
	}
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE user_id = ? AND device_id = ?`, userID, deviceID))
}

func (s *DB) DeviceByID(ctx context.Context, id int64) (*store.Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

func (s *DB) ListDevices(ctx context.Context, userID int64) ([]store.Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE user_id = ? ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

func (s *DB) RevokeDevice(ctx context.Context, userID, id int64, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := affected(tx.ExecContext(ctx, `UPDATE devices SET revoked_at = ? WHERE id = ? AND user_id = ?`, ms(now), id, userID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, ms(now), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DB) TouchDevice(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_seen_at = ? WHERE id = ?`, ms(now), id)
	return err
}

// --- refresh tokens ---

func (s *DB) CreateRefreshToken(ctx context.Context, t *store.RefreshToken) error {
	return createRefresh(ctx, s.db, t, time.Now())
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func createRefresh(ctx context.Context, db execer, t *store.RefreshToken, now time.Time) error {
	res, err := db.ExecContext(ctx, `INSERT INTO refresh_tokens(user_id, device_id, token_hash, expires_at, created_at) VALUES(?,?,?,?,?)`,
		t.UserID, t.DeviceID, t.Hash, ms(t.ExpiresAt), ms(now))
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

func (s *DB) RefreshTokenByHash(ctx context.Context, hash string) (*store.RefreshToken, error) {
	var t store.RefreshToken
	var exp int64
	var revoked, replaced sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, device_id, token_hash, expires_at, revoked_at, replaced_by FROM refresh_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.UserID, &t.DeviceID, &t.Hash, &exp, &revoked, &replaced)
	if err != nil {
		return nil, notFound(err)
	}
	t.ExpiresAt, t.RevokedAt = fromMS(exp), fromNullMS(revoked)
	if replaced.Valid {
		t.ReplacedBy = &replaced.Int64
	}
	return &t, nil
}

func (s *DB) RotateRefreshToken(ctx context.Context, oldID int64, next *store.RefreshToken, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := createRefresh(ctx, tx, next, now); err != nil {
		return err
	}
	if err := affected(tx.ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = ?, replaced_by = ? WHERE id = ? AND revoked_at IS NULL`,
		ms(now), next.ID, oldID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DB) RevokeDeviceTokens(ctx context.Context, deviceID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE refresh_tokens SET revoked_at = ? WHERE device_id = ? AND revoked_at IS NULL`, ms(now), deviceID)
	return err
}

// --- credentials ---

const credCols = `id, user_id, slug, provider, label, base_url, kind, sealed_key, last4, created_at, updated_at`

func scanCred(row interface{ Scan(...any) error }) (*store.Credential, error) {
	var c store.Credential
	var created, updated int64
	if err := row.Scan(&c.ID, &c.UserID, &c.Slug, &c.Provider, &c.Label, &c.BaseURL, &c.Kind, &c.SealedKey, &c.Last4, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	c.CreatedAt, c.UpdatedAt = fromMS(created), fromMS(updated)
	return &c, nil
}

// UpsertCredential stores a credential; one with the same slug is replaced.
func (s *DB) UpsertCredential(ctx context.Context, c *store.Credential) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO credentials(user_id, slug, provider, label, base_url, kind, sealed_key, last4, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(user_id, slug) DO UPDATE SET provider = excluded.provider, label = excluded.label, base_url = excluded.base_url,
			kind = excluded.kind, sealed_key = excluded.sealed_key, last4 = excluded.last4, updated_at = excluded.updated_at`,
		c.UserID, c.Slug, c.Provider, c.Label, c.BaseURL, c.Kind, c.SealedKey, c.Last4, ms(c.CreatedAt), ms(c.UpdatedAt))
	if err != nil {
		return err
	}
	got, err := s.CredentialBySlug(ctx, c.UserID, c.Slug)
	if err != nil {
		return err
	}
	*c = *got
	return nil
}

func (s *DB) ListCredentials(ctx context.Context, userID int64) ([]store.Credential, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+credCols+` FROM credentials WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Credential
	for rows.Next() {
		c, err := scanCred(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *DB) CredentialBySlug(ctx context.Context, userID int64, slug string) (*store.Credential, error) {
	return scanCred(s.db.QueryRowContext(ctx, `SELECT `+credCols+` FROM credentials WHERE user_id = ? AND slug = ?`, userID, slug))
}

func (s *DB) DeleteCredential(ctx context.Context, userID, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM credentials WHERE id = ? AND user_id = ?`, id, userID))
}

// --- oauth states ---

func (s *DB) CreateOAuthState(ctx context.Context, st *store.OAuthState) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM oauth_states WHERE expires_at < ?`, ms(time.Now())); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO oauth_states(state_hash, user_id, provider, sealed_verifier, expires_at) VALUES(?,?,?,?,?)`,
		st.Hash, st.UserID, st.Provider, st.SealedVerifier, ms(st.ExpiresAt))
	return err
}

func (s *DB) TakeOAuthState(ctx context.Context, hash string, now time.Time) (*store.OAuthState, error) {
	var st store.OAuthState
	var exp int64
	err := s.db.QueryRowContext(ctx, `DELETE FROM oauth_states WHERE state_hash = ? RETURNING state_hash, user_id, provider, sealed_verifier, expires_at`, hash).
		Scan(&st.Hash, &st.UserID, &st.Provider, &st.SealedVerifier, &exp)
	if err != nil {
		return nil, notFound(err)
	}
	st.ExpiresAt = fromMS(exp)
	if now.After(st.ExpiresAt) {
		return nil, store.ErrNotFound
	}
	return &st, nil
}

// --- conversations ---

const convCols = `id, user_id, title, provider, model, system, created_at, updated_at`

func scanConv(row interface{ Scan(...any) error }) (*store.Conversation, error) {
	var c store.Conversation
	var created, updated int64
	if err := row.Scan(&c.ID, &c.UserID, &c.Title, &c.Provider, &c.Model, &c.System, &created, &updated); err != nil {
		return nil, notFound(err)
	}
	c.CreatedAt, c.UpdatedAt = fromMS(created), fromMS(updated)
	return &c, nil
}

func (s *DB) CreateConversation(ctx context.Context, c *store.Conversation) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO conversations(user_id, title, provider, model, system, created_at, updated_at) VALUES(?,?,?,?,?,?,?)`,
		c.UserID, c.Title, c.Provider, c.Model, c.System, ms(c.CreatedAt), ms(c.UpdatedAt))
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *DB) ConversationByID(ctx context.Context, userID, id int64) (*store.Conversation, error) {
	return scanConv(s.db.QueryRowContext(ctx, `SELECT `+convCols+` FROM conversations WHERE id = ? AND user_id = ?`, id, userID))
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func (s *DB) ListConversations(ctx context.Context, userID int64, q store.ConversationQuery) ([]store.Conversation, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	where := []string{"c.user_id = ?"}
	args := []any{userID}
	if q.Before > 0 {
		where = append(where, "(c.updated_at, c.id) < (SELECT updated_at, id FROM conversations WHERE id = ?)")
		args = append(args, q.Before)
	}
	if s := strings.TrimSpace(q.Search); s != "" {
		like := escapeLike(s)
		where = append(where, `(c.title LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.content LIKE ? ESCAPE '\'))`)
		args = append(args, like, like)
	}
	args = append(args, q.Limit)
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.user_id, c.title, c.provider, c.model, c.system, c.created_at, c.updated_at
		FROM conversations c WHERE `+strings.Join(where, " AND ")+` ORDER BY c.updated_at DESC, c.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Conversation
	for rows.Next() {
		c, err := scanConv(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *DB) UpdateConversation(ctx context.Context, c *store.Conversation) error {
	return affected(s.db.ExecContext(ctx, `UPDATE conversations SET title = ?, provider = ?, model = ?, system = ?, updated_at = ? WHERE id = ? AND user_id = ?`,
		c.Title, c.Provider, c.Model, c.System, ms(c.UpdatedAt), c.ID, c.UserID))
}

func (s *DB) DeleteConversation(ctx context.Context, userID, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM conversations WHERE id = ? AND user_id = ?`, id, userID))
}

func (s *DB) AddMessage(ctx context.Context, m *store.Message) error {
	if m.Status == "" {
		m.Status = "complete"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO messages(conversation_id, role, content, provider, model, input_tokens, output_tokens, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, m.ConversationID, m.Role, m.Content, m.Provider, m.Model, m.InputTokens, m.OutputTokens, m.Status, ms(m.CreatedAt))
	if err != nil {
		return err
	}
	m.ID, err = res.LastInsertId()
	return err
}

func (s *DB) ListMessages(ctx context.Context, conversationID int64) ([]store.Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id, role, content, provider, model, input_tokens, output_tokens, status, created_at
		FROM messages WHERE conversation_id = ? ORDER BY id`, conversationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Message
	for rows.Next() {
		var m store.Message
		var created int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Content, &m.Provider, &m.Model, &m.InputTokens, &m.OutputTokens, &m.Status, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = fromMS(created)
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- usage and limits ---

func (s *DB) AddUsage(ctx context.Context, userID int64, provider, day string, requests, in, out int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO usage_daily(user_id, provider, day, requests, input_tokens, output_tokens) VALUES(?,?,?,?,?,?)
		ON CONFLICT(user_id, provider, day) DO UPDATE SET requests = requests + excluded.requests,
			input_tokens = input_tokens + excluded.input_tokens, output_tokens = output_tokens + excluded.output_tokens`,
		userID, provider, day, requests, in, out)
	return err
}

func (s *DB) UsageForDay(ctx context.Context, userID int64, day string) (int64, int64, error) {
	var req, tok int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0), COALESCE(SUM(input_tokens + output_tokens),0) FROM usage_daily WHERE user_id = ? AND day = ?`,
		userID, day).Scan(&req, &tok)
	return req, tok, err
}

func (s *DB) ListUsage(ctx context.Context, userID int64, fromDay string) ([]store.UsageRow, error) {
	query := `SELECT user_id, provider, day, requests, input_tokens, output_tokens FROM usage_daily WHERE day >= ?`
	args := []any{fromDay}
	if userID > 0 {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY day DESC, user_id, provider`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.UsageRow
	for rows.Next() {
		var u store.UsageRow
		if err := rows.Scan(&u.UserID, &u.Provider, &u.Day, &u.Requests, &u.InputTokens, &u.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *DB) GetLimits(ctx context.Context, userID int64) (store.Limits, error) {
	var req, tok sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT daily_requests, daily_tokens FROM user_limits WHERE user_id = ?`, userID).Scan(&req, &tok)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Limits{}, nil
	}
	if err != nil {
		return store.Limits{}, err
	}
	var l store.Limits
	if req.Valid {
		l.DailyRequests = &req.Int64
	}
	if tok.Valid {
		l.DailyTokens = &tok.Int64
	}
	return l, nil
}

func (s *DB) SetLimits(ctx context.Context, userID int64, l store.Limits) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO user_limits(user_id, daily_requests, daily_tokens) VALUES(?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET daily_requests = excluded.daily_requests, daily_tokens = excluded.daily_tokens`,
		userID, nullInt(l.DailyRequests), nullInt(l.DailyTokens))
	return err
}

// --- api keys ---

const apiKeyCols = `id, user_id, name, prefix, key_hash, scopes, rpm, last_used_at, created_at, revoked_at`

func scanAPIKey(row interface{ Scan(...any) error }) (*store.APIKey, error) {
	var k store.APIKey
	var scopes string
	var created int64
	var used, revoked sql.NullInt64
	if err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.Prefix, &k.Hash, &scopes, &k.RPM, &used, &created, &revoked); err != nil {
		return nil, notFound(err)
	}
	if scopes != "" {
		k.Scopes = strings.Split(scopes, ",")
	}
	k.LastUsedAt, k.CreatedAt, k.RevokedAt = fromNullMS(used), fromMS(created), fromNullMS(revoked)
	return &k, nil
}

func (s *DB) CreateAPIKey(ctx context.Context, k *store.APIKey) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO api_keys(user_id, name, prefix, key_hash, scopes, rpm, created_at) VALUES(?,?,?,?,?,?,?)`,
		k.UserID, k.Name, k.Prefix, k.Hash, strings.Join(k.Scopes, ","), k.RPM, ms(k.CreatedAt))
	if err != nil {
		return err
	}
	k.ID, err = res.LastInsertId()
	return err
}

func (s *DB) ListAPIKeys(ctx context.Context, userID int64) ([]store.APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

func (s *DB) APIKeyByHash(ctx context.Context, hash string) (*store.APIKey, error) {
	return scanAPIKey(s.db.QueryRowContext(ctx, `SELECT `+apiKeyCols+` FROM api_keys WHERE key_hash = ?`, hash))
}

func (s *DB) RevokeAPIKey(ctx context.Context, userID, id int64, now time.Time) error {
	return affected(s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`, ms(now), id, userID))
}

func (s *DB) TouchAPIKey(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at = ? WHERE id = ?`, ms(now), id)
	return err
}

// --- channels ---

const channelCols = `id, user_id, device_id, channel, port, transport_types, sealed_transports, invite_hash, status, created_at`

func scanChannel(row interface{ Scan(...any) error }) (*store.Channel, error) {
	var c store.Channel
	var user, device sql.NullInt64
	var invite sql.NullString
	var created int64
	if err := row.Scan(&c.ID, &user, &device, &c.Channel, &c.Port, &c.TransportTypes, &c.SealedTransports, &invite, &c.Status, &created); err != nil {
		return nil, notFound(err)
	}
	if user.Valid {
		c.UserID = &user.Int64
	}
	if device.Valid {
		c.DeviceID = &device.Int64
	}
	if invite.Valid {
		c.InviteHash = &invite.String
	}
	c.CreatedAt = fromMS(created)
	return &c, nil
}

func (s *DB) CreateChannel(ctx context.Context, c *store.Channel) error {
	var invite any
	if c.InviteHash != nil {
		invite = *c.InviteHash
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO channels(user_id, device_id, channel, port, transport_types, sealed_transports, invite_hash, status, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, nullInt(c.UserID), nullInt(c.DeviceID), c.Channel, c.Port, c.TransportTypes, c.SealedTransports, invite, c.Status, ms(c.CreatedAt))
	if isUnique(err) {
		return store.ErrConflict
	}
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *DB) ChannelByID(ctx context.Context, id int64) (*store.Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
}

func (s *DB) ChannelByInvite(ctx context.Context, inviteHash string) (*store.Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE invite_hash = ? AND status = 'active'`, inviteHash))
}

func (s *DB) ActiveChannelForDevice(ctx context.Context, deviceID int64) (*store.Channel, error) {
	return scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE device_id = ? AND status = 'active' ORDER BY id DESC LIMIT 1`, deviceID))
}

func (s *DB) ListChannels(ctx context.Context, userID *int64) ([]store.Channel, error) {
	query := `SELECT ` + channelCols + ` FROM channels WHERE status = 'active'`
	var args []any
	if userID != nil {
		query += ` AND user_id = ?`
		args = append(args, *userID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// BindChannel gives an invite channel to a user and device; the invite is
// used up.
func (s *DB) BindChannel(ctx context.Context, id, userID, deviceID int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE channels SET user_id = ?, device_id = ?, invite_hash = NULL WHERE id = ? AND user_id IS NULL`, userID, deviceID, id))
}

func (s *DB) SetChannelStatus(ctx context.Context, id int64, status string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE channels SET status = ?, invite_hash = CASE WHEN ? = 'active' THEN invite_hash ELSE NULL END WHERE id = ?`, status, status, id))
}

func (s *DB) UsedPorts(ctx context.Context) (map[int]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT port FROM channels WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// --- carriers ---

const carrierCols = `id, type, sealed_url, url_hash, hint, status, channel_id, created_at`

func scanCarrier(row interface{ Scan(...any) error }) (*store.Carrier, error) {
	var c store.Carrier
	var ch sql.NullInt64
	var created int64
	if err := row.Scan(&c.ID, &c.Type, &c.SealedURL, &c.URLHash, &c.Hint, &c.Status, &ch, &created); err != nil {
		return nil, notFound(err)
	}
	if ch.Valid {
		c.ChannelID = &ch.Int64
	}
	c.CreatedAt = fromMS(created)
	return &c, nil
}

func (s *DB) AddCarrier(ctx context.Context, c *store.Carrier) error {
	if c.Status == "" {
		c.Status = "free"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO carriers(type, sealed_url, url_hash, hint, status, created_at) VALUES(?,?,?,?,?,?)`,
		c.Type, c.SealedURL, c.URLHash, c.Hint, c.Status, ms(c.CreatedAt))
	if isUnique(err) {
		return store.ErrConflict
	}
	if err != nil {
		return err
	}
	c.ID, err = res.LastInsertId()
	return err
}

func (s *DB) listCarriers(ctx context.Context, where string, args ...any) ([]store.Carrier, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+carrierCols+` FROM carriers `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Carrier
	for rows.Next() {
		c, err := scanCarrier(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func (s *DB) ListCarriers(ctx context.Context) ([]store.Carrier, error) {
	return s.listCarriers(ctx, "")
}

func (s *DB) CarriersForChannel(ctx context.Context, channelID int64) ([]store.Carrier, error) {
	return s.listCarriers(ctx, "WHERE channel_id = ?", channelID)
}

func (s *DB) DeleteCarrier(ctx context.Context, id int64) error {
	return affected(s.db.ExecContext(ctx, `DELETE FROM carriers WHERE id = ? AND status != 'assigned'`, id))
}

func (s *DB) SetCarrierStatus(ctx context.Context, id int64, status string) error {
	return affected(s.db.ExecContext(ctx, `UPDATE carriers SET status = ?, channel_id = CASE WHEN ? = 'assigned' THEN channel_id ELSE NULL END WHERE id = ?`, status, status, id))
}

func (s *DB) ReserveCarrier(ctx context.Context, typ string) (*store.Carrier, error) {
	return scanCarrier(s.db.QueryRowContext(ctx, `UPDATE carriers SET status = 'assigned'
		WHERE id = (SELECT id FROM carriers WHERE type = ? AND status = 'free' ORDER BY id LIMIT 1)
		RETURNING `+carrierCols, typ))
}

func (s *DB) AttachCarrier(ctx context.Context, carrierID, channelID int64) error {
	return affected(s.db.ExecContext(ctx, `UPDATE carriers SET channel_id = ? WHERE id = ? AND status = 'assigned'`, channelID, carrierID))
}

func (s *DB) ReleaseCarrier(ctx context.Context, carrierID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE carriers SET status = 'free', channel_id = NULL WHERE id = ? AND status = 'assigned'`, carrierID)
	return err
}

func (s *DB) ReleaseChannelCarriers(ctx context.Context, channelID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE carriers SET status = 'free', channel_id = NULL WHERE channel_id = ? AND status = 'assigned'`, channelID)
	return err
}
