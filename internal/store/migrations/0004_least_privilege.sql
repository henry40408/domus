-- Least privilege: a per-user group limit, read-only tokens and token expiry.
ALTER TABLE users ADD COLUMN scope TEXT;
ALTER TABLE tokens ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tokens ADD COLUMN expires INTEGER;
