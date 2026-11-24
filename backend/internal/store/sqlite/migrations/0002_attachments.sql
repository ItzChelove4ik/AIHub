-- Images attached to chat messages. Kept with the conversation so the
-- history sent to the provider on later turns still has them.
CREATE TABLE attachments (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    type       TEXT NOT NULL,          -- image
    media_type TEXT NOT NULL,
    size       INTEGER NOT NULL,
    data       BLOB NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX attachments_message ON attachments(message_id, id);
