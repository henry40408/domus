//! SQLite persistence: owner, sessions, long-lived tokens, Hue bridge, groups.

use serde::Serialize;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions};
use sqlx::{Row, SqlitePool};
use std::path::Path;
use std::str::FromStr;

use crate::util::now_secs;

pub const SESSION_TTL_SECS: i64 = 7 * 24 * 3600;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct TokenInfo {
    pub id: i64,
    pub name: String,
    pub created: i64,
    /// Unix seconds of the last authenticated request (coarse: refreshed at most once a minute).
    pub last_used: Option<i64>,
    /// Group names the token may use; `None` means unrestricted.
    pub scope: Option<Vec<String>>,
}

/// What a token may touch: `None` is everything, otherwise only the listed groups and their members.
pub type Scope = Option<Vec<String>>;

fn parse_scope(raw: Option<String>) -> Scope {
    // An unreadable value must not widen access: it becomes an empty scope (nothing allowed).
    raw.map(|r| serde_json::from_str(&r).unwrap_or_default())
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct HueBridge {
    pub ip: String,
    pub key: String,
}

pub struct Store {
    pool: SqlitePool,
}

impl Store {
    pub async fn open(path: &Path) -> sqlx::Result<Self> {
        let options = SqliteConnectOptions::new()
            .filename(path)
            .create_if_missing(true)
            .journal_mode(SqliteJournalMode::Wal);
        Self::init(SqlitePoolOptions::new().connect_with(options).await?).await
    }

    pub async fn open_memory() -> sqlx::Result<Self> {
        Self::init(memory_pool().await?).await
    }

    async fn init(pool: SqlitePool) -> sqlx::Result<Self> {
        upgrade_unversioned(&pool).await?;
        sqlx::migrate!("./migrations")
            .run(&pool)
            .await
            .map_err(sqlx::Error::from)?;
        Ok(Self { pool })
    }

    // owner

    pub async fn owner_hash(&self) -> Option<String> {
        sqlx::query_scalar("SELECT password_hash FROM owner WHERE id = 1")
            .fetch_optional(&self.pool)
            .await
            .ok()
            .flatten()
    }

    /// Returns false if an owner already exists.
    pub async fn set_owner_if_absent(&self, password_hash: &str) -> bool {
        sqlx::query("INSERT OR IGNORE INTO owner (id, password_hash) VALUES (1, ?1)")
            .bind(password_hash)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() == 1)
            .unwrap_or(false)
    }

    // sessions

    pub async fn create_session(&self, token_hash: &str) {
        let expires = now_secs() + SESSION_TTL_SECS;
        let _ = sqlx::query("DELETE FROM sessions WHERE expires < ?1")
            .bind(now_secs())
            .execute(&self.pool)
            .await;
        let _ =
            sqlx::query("INSERT OR REPLACE INTO sessions (token_hash, expires) VALUES (?1, ?2)")
                .bind(token_hash)
                .bind(expires)
                .execute(&self.pool)
                .await;
    }

    pub async fn session_valid(&self, token_hash: &str) -> bool {
        sqlx::query_scalar::<_, i64>(
            "SELECT 1 FROM sessions WHERE token_hash = ?1 AND expires > ?2",
        )
        .bind(token_hash)
        .bind(now_secs())
        .fetch_optional(&self.pool)
        .await
        .map(|o| o.is_some())
        .unwrap_or(false)
    }

    pub async fn delete_session(&self, token_hash: &str) {
        let _ = sqlx::query("DELETE FROM sessions WHERE token_hash = ?1")
            .bind(token_hash)
            .execute(&self.pool)
            .await;
    }

    // long-lived tokens (only the hash is stored)

    pub async fn create_token(
        &self,
        name: &str,
        token_hash: &str,
        scope: Option<&[String]>,
    ) -> Option<i64> {
        let scope = scope.map(|s| serde_json::to_string(s).unwrap_or_else(|_| "[]".into()));
        sqlx::query("INSERT INTO tokens (name, token_hash, created, scope) VALUES (?1, ?2, ?3, ?4)")
            .bind(name)
            .bind(token_hash)
            .bind(now_secs())
            .bind(scope)
            .execute(&self.pool)
            .await
            .ok()
            .map(|r| r.last_insert_rowid())
    }

    pub async fn list_tokens(&self) -> Vec<TokenInfo> {
        sqlx::query("SELECT id, name, created, last_used, scope FROM tokens ORDER BY id")
            .fetch_all(&self.pool)
            .await
            .map(|rows| {
                rows.iter()
                    .map(|r| TokenInfo {
                        id: r.get(0),
                        name: r.get(1),
                        created: r.get(2),
                        last_used: r.get(3),
                        scope: parse_scope(r.get(4)),
                    })
                    .collect()
            })
            .unwrap_or_default()
    }

    pub async fn revoke_token(&self, id: i64) -> bool {
        sqlx::query("DELETE FROM tokens WHERE id = ?1")
            .bind(id)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    /// Authenticates a token by its hash: `None` if unknown, otherwise its scope.
    pub async fn token_scope(&self, token_hash: &str) -> Option<Scope> {
        // Outer Option: is there such a token; inner: its (nullable) scope column.
        let row: Option<Option<String>> =
            sqlx::query_scalar("SELECT scope FROM tokens WHERE token_hash = ?1")
                .bind(token_hash)
                .fetch_optional(&self.pool)
                .await
                .ok()
                .flatten();
        let scope = row.map(parse_scope);
        if scope.is_some() {
            // Throttled so a polling client does not write on every request.
            let now = now_secs();
            let _ = sqlx::query(
                "UPDATE tokens SET last_used = ?2
                 WHERE token_hash = ?1 AND (last_used IS NULL OR last_used < ?3)",
            )
            .bind(token_hash)
            .bind(now)
            .bind(now - 60)
            .execute(&self.pool)
            .await;
        }
        scope
    }

    // Hue bridge

    pub async fn hue_get(&self) -> Option<HueBridge> {
        sqlx::query("SELECT ip, app_key FROM hue WHERE id = 1")
            .fetch_optional(&self.pool)
            .await
            .ok()
            .flatten()
            .map(|r| HueBridge {
                ip: r.get(0),
                key: r.get(1),
            })
    }

    pub async fn hue_set(&self, bridge: &HueBridge) {
        let _ = sqlx::query("INSERT OR REPLACE INTO hue (id, ip, app_key) VALUES (1, ?1, ?2)")
            .bind(&bridge.ip)
            .bind(&bridge.key)
            .execute(&self.pool)
            .await;
    }

    // groups

    pub async fn group_exists(&self, name: &str) -> bool {
        sqlx::query_scalar::<_, i64>("SELECT 1 FROM groups WHERE name = ?1")
            .bind(name)
            .fetch_optional(&self.pool)
            .await
            .map(|o| o.is_some())
            .unwrap_or(false)
    }

    pub async fn group_members(&self, name: &str) -> Vec<String> {
        sqlx::query_scalar("SELECT entity_id FROM group_members WHERE group_name = ?1 ORDER BY pos")
            .bind(name)
            .fetch_all(&self.pool)
            .await
            .unwrap_or_default()
    }

    pub async fn group_names(&self) -> Vec<String> {
        sqlx::query_scalar("SELECT name FROM groups ORDER BY name")
            .fetch_all(&self.pool)
            .await
            .unwrap_or_default()
    }

    /// Creates the group if needed and replaces its members (order preserved, duplicates dropped).
    pub async fn group_set(&self, name: &str, members: &[String]) -> sqlx::Result<()> {
        let mut tx = self.pool.begin().await?;
        sqlx::query("INSERT OR IGNORE INTO groups (name) VALUES (?1)")
            .bind(name)
            .execute(&mut *tx)
            .await?;
        sqlx::query("DELETE FROM group_members WHERE group_name = ?1")
            .bind(name)
            .execute(&mut *tx)
            .await?;
        let mut pos = 0i64;
        for m in members {
            let n = sqlx::query(
                "INSERT OR IGNORE INTO group_members (group_name, entity_id, pos) VALUES (?1, ?2, ?3)",
            )
            .bind(name)
            .bind(m)
            .bind(pos)
            .execute(&mut *tx)
            .await?
            .rows_affected();
            pos += n as i64;
        }
        tx.commit().await
    }

    /// Whether the group is also exposed as a `light.domus_group_<name>` entity.
    pub async fn group_exposed(&self, name: &str) -> bool {
        sqlx::query_scalar::<_, i64>("SELECT expose_light FROM groups WHERE name = ?1")
            .bind(name)
            .fetch_optional(&self.pool)
            .await
            .ok()
            .flatten()
            .is_some_and(|v| v != 0)
    }

    /// Returns false if the group does not exist.
    pub async fn group_set_exposed(&self, name: &str, exposed: bool) -> bool {
        sqlx::query("UPDATE groups SET expose_light = ?2 WHERE name = ?1")
            .bind(name)
            .bind(exposed)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    pub async fn exposed_group_names(&self) -> Vec<String> {
        sqlx::query_scalar("SELECT name FROM groups WHERE expose_light != 0 ORDER BY name")
            .fetch_all(&self.pool)
            .await
            .unwrap_or_default()
    }

    pub async fn group_delete(&self, name: &str) -> bool {
        sqlx::query("DELETE FROM groups WHERE name = ?1")
            .bind(name)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }
}

/// A single-connection in-memory pool; the connection never expires, or the data would vanish.
async fn memory_pool() -> sqlx::Result<SqlitePool> {
    SqlitePoolOptions::new()
        .max_connections(1)
        .idle_timeout(None)
        .max_lifetime(None)
        .connect_with(SqliteConnectOptions::from_str("sqlite::memory:")?)
        .await
}

/// Databases created before versioned migrations lack two columns that the baseline
/// migration only declares for fresh databases (`CREATE TABLE IF NOT EXISTS` skips them).
async fn upgrade_unversioned(pool: &SqlitePool) -> sqlx::Result<()> {
    if table_lacks_column(pool, "tokens", "last_used").await? {
        sqlx::query("ALTER TABLE tokens ADD COLUMN last_used INTEGER")
            .execute(pool)
            .await?;
    }
    if table_lacks_column(pool, "groups", "expose_light").await? {
        sqlx::query("ALTER TABLE groups ADD COLUMN expose_light INTEGER NOT NULL DEFAULT 0")
            .execute(pool)
            .await?;
    }
    Ok(())
}

/// True only when the table exists but has no such column (a fresh database has no table yet,
/// and the migration creates it with the column).
async fn table_lacks_column(pool: &SqlitePool, table: &str, column: &str) -> sqlx::Result<bool> {
    let table_exists = sqlx::query_scalar::<_, i64>("SELECT 1 FROM pragma_table_info(?1) LIMIT 1")
        .bind(table)
        .fetch_optional(pool)
        .await?
        .is_some();
    if !table_exists {
        return Ok(false);
    }
    let has_column =
        sqlx::query_scalar::<_, i64>("SELECT 1 FROM pragma_table_info(?1) WHERE name = ?2")
            .bind(table)
            .bind(column)
            .fetch_optional(pool)
            .await?
            .is_some();
    Ok(!has_column)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn owner_set_once() {
        let s = Store::open_memory().await.unwrap();
        assert!(s.owner_hash().await.is_none());
        assert!(s.set_owner_if_absent("a").await);
        assert!(!s.set_owner_if_absent("b").await);
        assert_eq!(s.owner_hash().await.as_deref(), Some("a"));
    }

    #[tokio::test]
    async fn token_lifecycle() {
        let s = Store::open_memory().await.unwrap();
        let id = s.create_token("watch", "h1", None).await.unwrap();
        assert!(s.list_tokens().await[0].last_used.is_none());
        assert!(s.token_scope("h1").await.is_some());
        assert!(s.list_tokens().await[0].last_used.is_some());
        assert!(!s.token_scope("h2").await.is_some());
        assert_eq!(s.list_tokens().await.len(), 1);
        assert_eq!(s.list_tokens().await[0].name, "watch");
        assert!(s.revoke_token(id).await);
        assert!(!s.token_scope("h1").await.is_some());
        assert!(!s.revoke_token(id).await);
    }

    #[tokio::test]
    async fn token_scope_roundtrip() {
        let s = Store::open_memory().await.unwrap();
        s.create_token("open", "h-open", None).await.unwrap();
        let scope = ["a".to_string(), "b".to_string()];
        s.create_token("scoped", "h-scoped", Some(&scope))
            .await
            .unwrap();
        assert_eq!(s.token_scope("h-open").await, Some(None));
        assert_eq!(s.token_scope("h-scoped").await, Some(Some(scope.to_vec())));
        assert_eq!(s.token_scope("nope").await, None);
        let list = s.list_tokens().await;
        assert_eq!(list[0].scope, None);
        assert_eq!(list[1].scope.as_deref(), Some(&scope[..]));
    }

    #[test]
    fn unparseable_scope_allows_nothing() {
        assert_eq!(parse_scope(None), None);
        assert_eq!(parse_scope(Some("[\"a\"]".into())), Some(vec!["a".into()]));
        assert_eq!(parse_scope(Some("not json".into())), Some(Vec::new()));
    }

    #[tokio::test]
    async fn session_lifecycle() {
        let s = Store::open_memory().await.unwrap();
        assert!(!s.session_valid("x").await);
        s.create_session("x").await;
        assert!(s.session_valid("x").await);
        s.delete_session("x").await;
        assert!(!s.session_valid("x").await);
    }

    #[tokio::test]
    async fn groups_keep_order_and_dedupe() {
        let s = Store::open_memory().await.unwrap();
        assert!(!s.group_exists("garmin").await);
        s.group_set(
            "garmin",
            &["light.b".into(), "light.a".into(), "light.b".into()],
        )
        .await
        .unwrap();
        assert!(s.group_exists("garmin").await);
        assert_eq!(s.group_members("garmin").await, vec!["light.b", "light.a"]);
        s.group_set("garmin", &["light.c".into()]).await.unwrap();
        assert_eq!(s.group_members("garmin").await, vec!["light.c"]);
        assert_eq!(s.group_names().await, vec!["garmin"]);
        assert!(!s.group_exposed("garmin").await);
        assert!(s.group_set_exposed("garmin", true).await);
        assert!(s.group_exposed("garmin").await);
        assert_eq!(s.exposed_group_names().await, vec!["garmin"]);
        s.group_set("garmin", &["light.d".into()]).await.unwrap();
        assert!(
            s.group_exposed("garmin").await,
            "editing members keeps the flag"
        );
        assert!(!s.group_set_exposed("nope", true).await);
        assert!(s.group_delete("garmin").await);
        assert!(!s.group_exposed("garmin").await);
        assert!(s.group_members("garmin").await.is_empty());
    }

    #[tokio::test]
    async fn hue_bridge_roundtrip() {
        let s = Store::open_memory().await.unwrap();
        assert!(s.hue_get().await.is_none());
        let b = HueBridge {
            ip: "192.168.1.2".into(),
            key: "k".into(),
        };
        s.hue_set(&b).await;
        assert_eq!(s.hue_get().await, Some(b));
    }

    #[tokio::test]
    async fn legacy_groups_table_is_migrated() {
        let pool = memory_pool().await.unwrap();
        sqlx::raw_sql(
            "CREATE TABLE groups (name TEXT PRIMARY KEY);
             INSERT INTO groups (name) VALUES ('old');",
        )
        .execute(&pool)
        .await
        .unwrap();
        let s = Store::init(pool).await.unwrap();
        assert!(s.group_exists("old").await);
        assert!(!s.group_exposed("old").await);
        assert!(s.group_set_exposed("old", true).await);
    }

    #[tokio::test]
    async fn v0_1_0_database_keeps_its_data() {
        let pool = memory_pool().await.unwrap();
        sqlx::raw_sql(
            "CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1), password_hash TEXT NOT NULL);
             CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);
             CREATE TABLE tokens (
                 id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL,
                 token_hash TEXT NOT NULL UNIQUE, created INTEGER NOT NULL, last_used INTEGER);
             CREATE TABLE hue (id INTEGER PRIMARY KEY CHECK (id = 1), ip TEXT NOT NULL, app_key TEXT NOT NULL);
             CREATE TABLE groups (name TEXT PRIMARY KEY, expose_light INTEGER NOT NULL DEFAULT 0);
             CREATE TABLE group_members (
                 group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
                 entity_id TEXT NOT NULL, pos INTEGER NOT NULL, PRIMARY KEY (group_name, entity_id));
             INSERT INTO owner VALUES (1, 'hash');
             INSERT INTO tokens (name, token_hash, created) VALUES ('watch', 'th', 1);
             INSERT INTO hue VALUES (1, '10.0.0.2', 'key');
             INSERT INTO groups VALUES ('garmin', 1);
             INSERT INTO group_members VALUES ('garmin', 'light.a', 0);",
        )
        .execute(&pool)
        .await
        .unwrap();
        let s = Store::init(pool).await.unwrap();
        assert_eq!(s.owner_hash().await.as_deref(), Some("hash"));
        assert!(s.token_scope("th").await.is_some());
        assert_eq!(s.hue_get().await.unwrap().ip, "10.0.0.2");
        assert!(s.group_exposed("garmin").await);
        assert_eq!(s.group_members("garmin").await, vec!["light.a"]);
    }

    #[tokio::test]
    async fn file_database_reopens_without_rerunning_migrations() {
        let dir = std::env::temp_dir().join(format!("domus-store-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let path = dir.join("domus.db");
        {
            let s = Store::open(&path).await.unwrap();
            s.set_owner_if_absent("a").await;
        }
        let s = Store::open(&path).await.unwrap();
        assert_eq!(s.owner_hash().await.as_deref(), Some("a"));
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
