-- AIHub initial schema. Times are unix milliseconds (UTC).

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_admin      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL
);

CREATE TABLE devices (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id    TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    revoked_at   INTEGER,
    UNIQUE (user_id, device_id)
);

CREATE TABLE refresh_tokens (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_id   INTEGER NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    expires_at  INTEGER NOT NULL,
    revoked_at  INTEGER,
    replaced_by INTEGER,
    created_at  INTEGER NOT NULL
);
CREATE INDEX refresh_tokens_device ON refresh_tokens(device_id);

CREATE TABLE credentials (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug       TEXT NOT NULL,
    provider   TEXT NOT NULL,
    label      TEXT NOT NULL DEFAULT '',
    base_url   TEXT NOT NULL DEFAULT '',
    kind       TEXT NOT NULL,          -- api_key | oauth
    sealed_key BLOB NOT NULL,          -- AES-256-GCM, nonce || ciphertext
    last4      TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    UNIQUE (user_id, slug)
);

CREATE TABLE oauth_states (
    state_hash      TEXT PRIMARY KEY,
    user_id         INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider        TEXT NOT NULL,
    sealed_verifier BLOB NOT NULL,
    expires_at      INTEGER NOT NULL
);

CREATE TABLE conversations (
    id         INTEGER PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      TEXT NOT NULL DEFAULT '',
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    system     TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);
CREATE INDEX conversations_user ON conversations(user_id, updated_at DESC);

CREATE TABLE messages (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    role            TEXT NOT NULL,     -- user | assistant
    content         TEXT NOT NULL,
    provider        TEXT NOT NULL DEFAULT '',
    model           TEXT NOT NULL DEFAULT '',
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'complete', -- complete | stopped | error
    created_at      INTEGER NOT NULL
);
CREATE INDEX messages_conversation ON messages(conversation_id, id);

CREATE TABLE usage_daily (
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    day           TEXT NOT NULL,       -- YYYY-MM-DD (UTC)
    requests      INTEGER NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id, provider, day)
);

CREATE TABLE user_limits (
    user_id        INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    daily_requests INTEGER,            -- NULL: server default; 0: unlimited
    daily_tokens   INTEGER
);

CREATE TABLE api_keys (
    id             INTEGER PRIMARY KEY,
    user_id        INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name           TEXT NOT NULL DEFAULT '',
    prefix         TEXT NOT NULL,
    key_hash       TEXT NOT NULL UNIQUE,
    scopes         TEXT NOT NULL,      -- comma separated: chat,models
    rpm            INTEGER NOT NULL DEFAULT 0,
    last_used_at   INTEGER,
    created_at     INTEGER NOT NULL,
    revoked_at     INTEGER
);
CREATE INDEX api_keys_user ON api_keys(user_id);

CREATE TABLE channels (
    id                INTEGER PRIMARY KEY,
    user_id           INTEGER REFERENCES users(id) ON DELETE SET NULL,
    device_id         INTEGER REFERENCES devices(id) ON DELETE SET NULL,
    channel           TEXT NOT NULL UNIQUE,
    port              INTEGER NOT NULL,
    transport_types   TEXT NOT NULL DEFAULT '',  -- e.g. "vyandex,cupsonline"
    sealed_transports BLOB NOT NULL,             -- JSON of the carriers (document URLs are secrets)
    invite_hash       TEXT UNIQUE,
    status            TEXT NOT NULL,             -- active | removed
    created_at        INTEGER NOT NULL
);

CREATE TABLE carriers (
    id         INTEGER PRIMARY KEY,
    type       TEXT NOT NULL,          -- vyandex | mailru
    sealed_url BLOB NOT NULL,
    url_hash   TEXT NOT NULL UNIQUE,
    hint       TEXT NOT NULL DEFAULT '', -- the last characters of the URL, for the admin
    status     TEXT NOT NULL,          -- free | assigned | broken
    channel_id INTEGER REFERENCES channels(id) ON DELETE SET NULL,
    created_at INTEGER NOT NULL
);
