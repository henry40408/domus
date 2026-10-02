-- Baseline schema. IF NOT EXISTS lets databases created before versioned migrations
-- (v0.1.0) adopt this migration without touching their data.

CREATE TABLE IF NOT EXISTS owner (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    expires INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created INTEGER NOT NULL,
    last_used INTEGER
);

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
