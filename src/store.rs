//! SQLite persistence: owner, sessions, long-lived tokens, Hue bridge, groups.

use rusqlite::{Connection, OptionalExtension, params};
use serde::Serialize;
use std::path::Path;
use std::sync::Mutex;

use crate::util::now_secs;

pub const SESSION_TTL_SECS: i64 = 7 * 24 * 3600;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct TokenInfo {
    pub id: i64,
    pub name: String,
    pub created: i64,
    /// Unix seconds of the last authenticated request (coarse: refreshed at most once a minute).
    pub last_used: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HueBridge {
    pub ip: String,
    pub key: String,
}

pub struct Store {
    conn: Mutex<Connection>,
}

impl Store {
    pub fn open(path: &Path) -> rusqlite::Result<Self> {
        Self::init(Connection::open(path)?)
    }

    pub fn open_memory() -> rusqlite::Result<Self> {
        Self::init(Connection::open_in_memory()?)
    }

    fn init(conn: Connection) -> rusqlite::Result<Self> {
        conn.execute_batch(
            "PRAGMA foreign_keys = ON;
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
                 created INTEGER NOT NULL
             );
             CREATE TABLE IF NOT EXISTS hue (
                 id INTEGER PRIMARY KEY CHECK (id = 1),
                 ip TEXT NOT NULL,
                 app_key TEXT NOT NULL
             );
             CREATE TABLE IF NOT EXISTS groups (
                 name TEXT PRIMARY KEY
             );
             CREATE TABLE IF NOT EXISTS group_members (
                 group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
                 entity_id TEXT NOT NULL,
                 pos INTEGER NOT NULL,
                 PRIMARY KEY (group_name, entity_id)
             );",
        )?;
        // Databases created before token usage tracking lack this column.
        let has_last_used = conn
            .prepare("SELECT 1 FROM pragma_table_info('tokens') WHERE name = 'last_used'")?
            .exists([])?;
        if !has_last_used {
            conn.execute("ALTER TABLE tokens ADD COLUMN last_used INTEGER", [])?;
        }
        // Databases created before group lights existed lack this column.
        let has_expose = conn
            .prepare("SELECT 1 FROM pragma_table_info('groups') WHERE name = 'expose_light'")?
            .exists([])?;
        if !has_expose {
            conn.execute(
                "ALTER TABLE groups ADD COLUMN expose_light INTEGER NOT NULL DEFAULT 0",
                [],
            )?;
        }
        Ok(Self {
            conn: Mutex::new(conn),
        })
    }

    fn conn(&self) -> std::sync::MutexGuard<'_, Connection> {
        self.conn.lock().unwrap_or_else(|e| e.into_inner())
    }

    // owner

    pub fn owner_hash(&self) -> Option<String> {
        self.conn()
            .query_row("SELECT password_hash FROM owner WHERE id = 1", [], |r| {
                r.get(0)
            })
            .optional()
            .ok()
            .flatten()
    }

    /// Returns false if an owner already exists.
    pub fn set_owner_if_absent(&self, password_hash: &str) -> bool {
        self.conn()
            .execute(
                "INSERT OR IGNORE INTO owner (id, password_hash) VALUES (1, ?1)",
                params![password_hash],
            )
            .map(|n| n == 1)
            .unwrap_or(false)
    }

    // sessions

    pub fn create_session(&self, token_hash: &str) {
        let expires = now_secs() + SESSION_TTL_SECS;
        let conn = self.conn();
        let _ = conn.execute(
            "DELETE FROM sessions WHERE expires < ?1",
            params![now_secs()],
        );
        let _ = conn.execute(
            "INSERT OR REPLACE INTO sessions (token_hash, expires) VALUES (?1, ?2)",
            params![token_hash, expires],
        );
    }

    pub fn session_valid(&self, token_hash: &str) -> bool {
        self.conn()
            .query_row(
                "SELECT 1 FROM sessions WHERE token_hash = ?1 AND expires > ?2",
                params![token_hash, now_secs()],
                |_| Ok(()),
            )
            .optional()
            .map(|o| o.is_some())
            .unwrap_or(false)
    }

    pub fn delete_session(&self, token_hash: &str) {
        let _ = self.conn().execute(
            "DELETE FROM sessions WHERE token_hash = ?1",
            params![token_hash],
        );
    }

    // long-lived tokens (only the hash is stored)

    pub fn create_token(&self, name: &str, token_hash: &str) -> Option<i64> {
        let conn = self.conn();
        conn.execute(
            "INSERT INTO tokens (name, token_hash, created) VALUES (?1, ?2, ?3)",
            params![name, token_hash, now_secs()],
        )
        .ok()?;
        Some(conn.last_insert_rowid())
    }

    pub fn list_tokens(&self) -> Vec<TokenInfo> {
        let conn = self.conn();
        let Ok(mut stmt) =
            conn.prepare("SELECT id, name, created, last_used FROM tokens ORDER BY id")
        else {
            return Vec::new();
        };
        stmt.query_map([], |r| {
            Ok(TokenInfo {
                id: r.get(0)?,
                name: r.get(1)?,
                created: r.get(2)?,
                last_used: r.get(3)?,
            })
        })
        .map(|rows| rows.filter_map(Result::ok).collect())
        .unwrap_or_default()
    }

    pub fn revoke_token(&self, id: i64) -> bool {
        self.conn()
            .execute("DELETE FROM tokens WHERE id = ?1", params![id])
            .map(|n| n > 0)
            .unwrap_or(false)
    }

    pub fn token_valid(&self, token_hash: &str) -> bool {
        let conn = self.conn();
        let valid = conn
            .query_row(
                "SELECT 1 FROM tokens WHERE token_hash = ?1",
                params![token_hash],
                |_| Ok(()),
            )
            .optional()
            .map(|o| o.is_some())
            .unwrap_or(false);
        if valid {
            // Throttled so a polling client does not write on every request.
            let now = now_secs();
            let _ = conn.execute(
                "UPDATE tokens SET last_used = ?2
                 WHERE token_hash = ?1 AND (last_used IS NULL OR last_used < ?3)",
                params![token_hash, now, now - 60],
            );
        }
        valid
    }

    // Hue bridge

    pub fn hue_get(&self) -> Option<HueBridge> {
        self.conn()
            .query_row("SELECT ip, app_key FROM hue WHERE id = 1", [], |r| {
                Ok(HueBridge {
                    ip: r.get(0)?,
                    key: r.get(1)?,
                })
            })
            .optional()
            .ok()
            .flatten()
    }

    pub fn hue_set(&self, bridge: &HueBridge) {
        let _ = self.conn().execute(
            "INSERT OR REPLACE INTO hue (id, ip, app_key) VALUES (1, ?1, ?2)",
            params![bridge.ip, bridge.key],
        );
    }

    // groups

    pub fn group_exists(&self, name: &str) -> bool {
        self.conn()
            .query_row(
                "SELECT 1 FROM groups WHERE name = ?1",
                params![name],
                |_| Ok(()),
            )
            .optional()
            .map(|o| o.is_some())
            .unwrap_or(false)
    }

    pub fn group_members(&self, name: &str) -> Vec<String> {
        let conn = self.conn();
        let Ok(mut stmt) =
            conn.prepare("SELECT entity_id FROM group_members WHERE group_name = ?1 ORDER BY pos")
        else {
            return Vec::new();
        };
        stmt.query_map(params![name], |r| r.get(0))
            .map(|rows| rows.filter_map(Result::ok).collect())
            .unwrap_or_default()
    }

    pub fn group_names(&self) -> Vec<String> {
        let conn = self.conn();
        let Ok(mut stmt) = conn.prepare("SELECT name FROM groups ORDER BY name") else {
            return Vec::new();
        };
        stmt.query_map([], |r| r.get(0))
            .map(|rows| rows.filter_map(Result::ok).collect())
            .unwrap_or_default()
    }

    /// Creates the group if needed and replaces its members (order preserved, duplicates dropped).
    pub fn group_set(&self, name: &str, members: &[String]) -> rusqlite::Result<()> {
        let mut conn = self.conn();
        let tx = conn.transaction()?;
        tx.execute(
            "INSERT OR IGNORE INTO groups (name) VALUES (?1)",
            params![name],
        )?;
        tx.execute(
            "DELETE FROM group_members WHERE group_name = ?1",
            params![name],
        )?;
        let mut pos = 0i64;
        for m in members {
            let n = tx.execute(
                "INSERT OR IGNORE INTO group_members (group_name, entity_id, pos) VALUES (?1, ?2, ?3)",
                params![name, m, pos],
            )?;
            pos += n as i64;
        }
        tx.commit()
    }

    /// Whether the group is also exposed as a `light.domus_group_<name>` entity.
    pub fn group_exposed(&self, name: &str) -> bool {
        self.conn()
            .query_row(
                "SELECT expose_light FROM groups WHERE name = ?1",
                params![name],
                |r| r.get::<_, i64>(0),
            )
            .optional()
            .ok()
            .flatten()
            .is_some_and(|v| v != 0)
    }

    /// Returns false if the group does not exist.
    pub fn group_set_exposed(&self, name: &str, exposed: bool) -> bool {
        self.conn()
            .execute(
                "UPDATE groups SET expose_light = ?2 WHERE name = ?1",
                params![name, exposed],
            )
            .map(|n| n > 0)
            .unwrap_or(false)
    }

    pub fn exposed_group_names(&self) -> Vec<String> {
        let conn = self.conn();
        let Ok(mut stmt) =
            conn.prepare("SELECT name FROM groups WHERE expose_light != 0 ORDER BY name")
        else {
            return Vec::new();
        };
        stmt.query_map([], |r| r.get(0))
            .map(|rows| rows.filter_map(Result::ok).collect())
            .unwrap_or_default()
    }

    pub fn group_delete(&self, name: &str) -> bool {
        self.conn()
            .execute("DELETE FROM groups WHERE name = ?1", params![name])
            .map(|n| n > 0)
            .unwrap_or(false)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn owner_set_once() {
        let s = Store::open_memory().unwrap();
        assert!(s.owner_hash().is_none());
        assert!(s.set_owner_if_absent("a"));
        assert!(!s.set_owner_if_absent("b"));
        assert_eq!(s.owner_hash().as_deref(), Some("a"));
    }

    #[test]
    fn token_lifecycle() {
        let s = Store::open_memory().unwrap();
        let id = s.create_token("watch", "h1").unwrap();
        assert!(s.list_tokens()[0].last_used.is_none());
        assert!(s.token_valid("h1"));
        assert!(s.list_tokens()[0].last_used.is_some());
        assert!(!s.token_valid("h2"));
        assert_eq!(s.list_tokens().len(), 1);
        assert_eq!(s.list_tokens()[0].name, "watch");
        assert!(s.revoke_token(id));
        assert!(!s.token_valid("h1"));
        assert!(!s.revoke_token(id));
    }

    #[test]
    fn session_lifecycle() {
        let s = Store::open_memory().unwrap();
        assert!(!s.session_valid("x"));
        s.create_session("x");
        assert!(s.session_valid("x"));
        s.delete_session("x");
        assert!(!s.session_valid("x"));
    }

    #[test]
    fn groups_keep_order_and_dedupe() {
        let s = Store::open_memory().unwrap();
        assert!(!s.group_exists("garmin"));
        s.group_set(
            "garmin",
            &["light.b".into(), "light.a".into(), "light.b".into()],
        )
        .unwrap();
        assert!(s.group_exists("garmin"));
        assert_eq!(s.group_members("garmin"), vec!["light.b", "light.a"]);
        s.group_set("garmin", &["light.c".into()]).unwrap();
        assert_eq!(s.group_members("garmin"), vec!["light.c"]);
        assert_eq!(s.group_names(), vec!["garmin"]);
        assert!(!s.group_exposed("garmin"));
        assert!(s.group_set_exposed("garmin", true));
        assert!(s.group_exposed("garmin"));
        assert_eq!(s.exposed_group_names(), vec!["garmin"]);
        s.group_set("garmin", &["light.d".into()]).unwrap();
        assert!(s.group_exposed("garmin"), "editing members keeps the flag");
        assert!(!s.group_set_exposed("nope", true));
        assert!(s.group_delete("garmin"));
        assert!(!s.group_exposed("garmin"));
        assert!(s.group_members("garmin").is_empty());
    }

    #[test]
    fn hue_bridge_roundtrip() {
        let s = Store::open_memory().unwrap();
        assert!(s.hue_get().is_none());
        let b = HueBridge {
            ip: "192.168.1.2".into(),
            key: "k".into(),
        };
        s.hue_set(&b);
        assert_eq!(s.hue_get(), Some(b));
    }

    #[test]
    fn legacy_groups_table_is_migrated() {
        let conn = Connection::open_in_memory().unwrap();
        conn.execute_batch(
            "CREATE TABLE groups (name TEXT PRIMARY KEY);
             INSERT INTO groups (name) VALUES ('old');",
        )
        .unwrap();
        let s = Store::init(conn).unwrap();
        assert!(s.group_exists("old"));
        assert!(!s.group_exposed("old"));
        assert!(s.group_set_exposed("old", true));
    }
}
