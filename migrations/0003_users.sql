-- Multiple users. The single owner password becomes user 1 (admin); existing tokens belong to it.
-- Sessions are dropped so everyone logs in again with a username.

CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created INTEGER NOT NULL
);

INSERT INTO users (id, username, password_hash, is_admin, created)
    SELECT 1, 'admin', password_hash, 1, CAST(strftime('%s', 'now') AS INTEGER) FROM owner;

DROP TABLE owner;

DELETE FROM sessions;
ALTER TABLE sessions ADD COLUMN user_id INTEGER;
ALTER TABLE tokens ADD COLUMN user_id INTEGER;
UPDATE tokens SET user_id = 1;
