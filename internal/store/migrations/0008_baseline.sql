-- The whole schema at version 8, in place of the eight files that built it. A new
-- database gets it from here. A database that already reached version 8 through
-- those files has nothing left to create, so every statement must stay safe to run
-- against it. Columns keep the order the old files left them in.
--
-- Times are Unix seconds. Only hashes of passwords, session cookies, tokens and
-- webhook ids are stored.

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created INTEGER NOT NULL,
    -- A JSON array of group names the user is limited to. NULL means unrestricted.
    scope TEXT
);

-- `refresh_id` marks a session started from an app's access token (the Companion
-- WebView): it ends together with that app login, and `elevated_until` is how long
-- after a password re-entry it may do sensitive things. Both are NULL for ordinary
-- password-login sessions.
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    expires INTEGER NOT NULL,
    user_id INTEGER,
    created INTEGER NOT NULL DEFAULT 0,
    last_seen INTEGER NOT NULL DEFAULT 0,
    user_agent TEXT,
    refresh_id INTEGER,
    elevated_until INTEGER
);
CREATE INDEX IF NOT EXISTS sessions_refresh ON sessions (refresh_id);

-- Long-lived tokens, and the access tokens minted from a refresh token, which carry
-- its id in `refresh_id` so that rotating or revoking either side cleans up the
-- other. `scope` is a JSON array of group names; NULL means unrestricted.
CREATE TABLE IF NOT EXISTS tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created INTEGER NOT NULL,
    last_used INTEGER,
    scope TEXT,
    user_id INTEGER,
    read_only INTEGER NOT NULL DEFAULT 0,
    expires INTEGER,
    refresh_id INTEGER
);
CREATE INDEX IF NOT EXISTS tokens_refresh ON tokens (refresh_id);

CREATE TABLE IF NOT EXISTS hue (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    ip TEXT NOT NULL,
    app_key TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS groups (
    name TEXT PRIMARY KEY,
    expose_light INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS group_members (
    group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
    entity_id TEXT NOT NULL,
    pos INTEGER NOT NULL,
    PRIMARY KEY (group_name, entity_id)
);

-- OAuth refresh tokens for the Home Assistant Companion apps.
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    client_id TEXT NOT NULL,
    created INTEGER NOT NULL,
    last_used INTEGER
);
CREATE INDEX IF NOT EXISTS refresh_tokens_user ON refresh_tokens (user_id);

-- Devices registered by the Companion apps (mobile_app). The webhook id is the
-- credential of the app's webhook calls. `refresh_id` ties a registration to the
-- app login that made it, so logging the app out also removes the registration.
CREATE TABLE IF NOT EXISTS registrations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    refresh_id INTEGER,
    app_id TEXT NOT NULL,
    app_name TEXT NOT NULL,
    app_version TEXT NOT NULL,
    device_id TEXT,
    device_name TEXT NOT NULL,
    manufacturer TEXT NOT NULL,
    model TEXT NOT NULL,
    os_name TEXT NOT NULL,
    os_version TEXT,
    created INTEGER NOT NULL,
    last_seen INTEGER
);
CREATE INDEX IF NOT EXISTS registrations_user ON registrations (user_id);
CREATE INDEX IF NOT EXISTS registrations_refresh ON registrations (refresh_id);
-- One registration per physical device and app: registering again replaces it.
CREATE UNIQUE INDEX IF NOT EXISTS registrations_device ON registrations (user_id, app_id, device_id)
    WHERE device_id IS NOT NULL;

-- domus 0.2 to 0.12 (the Rust server up to 0.10, the Go one from 0.11) check this
-- table and refuse to start when it names a migration they do not have. This row
-- makes them stop instead of running against a schema they do not know. success is
-- TRUE because a failed row makes them tell their user to delete it. The runner
-- reads the row too: it shows that this file ran, should user_version get lost.
CREATE TABLE IF NOT EXISTS _sqlx_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    installed_on TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    success BOOLEAN NOT NULL,
    checksum BLOB NOT NULL,
    execution_time BIGINT NOT NULL
);

INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time)
VALUES (99999999999999, 'domus versions its schema with PRAGMA user_version', TRUE, X'', 0)
ON CONFLICT (version) DO NOTHING;
