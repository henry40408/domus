-- OAuth refresh tokens for the Home Assistant Companion apps. Only hashes are stored.
-- Access tokens minted from a refresh token are `tokens` rows carrying its id in `refresh_id`,
-- so rotating or revoking either side can clean up the other.
CREATE TABLE refresh_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    client_id TEXT NOT NULL,
    created INTEGER NOT NULL,
    last_used INTEGER
);
CREATE INDEX refresh_tokens_user ON refresh_tokens (user_id);
ALTER TABLE tokens ADD COLUMN refresh_id INTEGER;
CREATE INDEX tokens_refresh ON tokens (refresh_id);
