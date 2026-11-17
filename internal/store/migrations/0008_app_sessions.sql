-- Sessions started from an app's access token (the Companion WebView). Such a session carries the
-- id of the app login it came from and ends together with it; `elevated_until` is how long after a
-- password re-entry it may do sensitive things (NULL for ordinary password-login sessions).
ALTER TABLE sessions ADD COLUMN refresh_id INTEGER;
ALTER TABLE sessions ADD COLUMN elevated_until INTEGER;
CREATE INDEX sessions_refresh ON sessions (refresh_id);
