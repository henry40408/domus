-- Devices registered by the Home Assistant Companion apps (mobile_app). The webhook id is the
-- credential of the app's webhook calls, so only its hash is stored.
-- `refresh_id` ties a registration to the app login (refresh token) that made it, so logging
-- the app out also removes the registration.
CREATE TABLE registrations (
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
CREATE INDEX registrations_user ON registrations (user_id);
CREATE INDEX registrations_refresh ON registrations (refresh_id);
-- One registration per physical device and app: registering again replaces it.
CREATE UNIQUE INDEX registrations_device ON registrations (user_id, app_id, device_id)
    WHERE device_id IS NOT NULL;
