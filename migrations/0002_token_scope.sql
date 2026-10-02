-- Optional access scope for a token: a JSON array of group names. NULL means unrestricted,
-- so tokens created before this migration keep full access.
ALTER TABLE tokens ADD COLUMN scope TEXT;
