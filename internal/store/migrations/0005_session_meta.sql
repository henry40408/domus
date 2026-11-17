-- Session metadata for the "your sessions" list and the idle timeout.
ALTER TABLE sessions ADD COLUMN created INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN user_agent TEXT;
UPDATE sessions SET created = CAST(strftime('%s', 'now') AS INTEGER), last_seen = CAST(strftime('%s', 'now') AS INTEGER);
