//! SQLite persistence: users, sessions, long-lived tokens, Hue bridge, groups.

use serde::Serialize;
use sqlx::sqlite::{SqliteConnectOptions, SqliteJournalMode, SqlitePoolOptions};
use sqlx::{Row, SqlitePool};
use std::path::Path;
use std::str::FromStr;

use crate::util::now_secs;

pub const SESSION_TTL_SECS: i64 = 7 * 24 * 3600;
/// A session unused for this long is dead, even before its absolute lifetime ends.
pub const SESSION_IDLE_SECS: i64 = 24 * 3600;
/// `last_seen` is refreshed at most this often, to avoid a write on every request.
const SESSION_TOUCH_SECS: i64 = 60;
const USER_AGENT_MAX: usize = 200;

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct TokenInfo {
    pub id: i64,
    pub name: String,
    pub created: i64,
    /// Unix seconds of the last authenticated request (coarse: refreshed at most once a minute).
    pub last_used: Option<i64>,
    /// Group names the token may use; `None` means unrestricted.
    pub scope: Option<Vec<String>>,
    pub read_only: bool,
    /// Unix seconds after which the token stops working; `None` never expires.
    pub expires: Option<i64>,
    pub user_id: Option<i64>,
    pub username: Option<String>,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct SessionInfo {
    /// Public id (the row id); the token hash never leaves the store.
    pub id: i64,
    pub created: i64,
    pub last_seen: i64,
    pub user_agent: String,
    pub current: bool,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct User {
    pub id: i64,
    pub username: String,
    pub is_admin: bool,
    pub created: i64,
    /// Groups the user may use; `None` is unrestricted. Always `None` for admins.
    pub scope: Option<Vec<String>>,
}

fn user_row(r: &sqlx::sqlite::SqliteRow) -> User {
    User {
        id: r.get(0),
        username: r.get(1),
        is_admin: r.get(2),
        created: r.get(3),
        scope: parse_scope(r.get(4)),
    }
}

/// What a token may touch: `None` is everything, otherwise only the listed groups and their members.
pub type Scope = Option<Vec<String>>;

fn parse_scope(raw: Option<String>) -> Scope {
    // An unreadable value must not widen access: it becomes an empty scope (nothing allowed).
    raw.map(|r| serde_json::from_str(&r).unwrap_or_default())
}

/// What an authenticated token may do: its own scope narrowed by its owner's, and read-only or not.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct TokenAuth {
    pub scope: Scope,
    pub read_only: bool,
}

/// The groups allowed by both scopes; `None` (unrestricted) yields to the other side.
pub fn intersect_scope(a: &Scope, b: &Scope) -> Scope {
    match (a, b) {
        (None, other) | (other, None) => other.clone(),
        (Some(x), Some(y)) => Some(x.iter().filter(|g| y.contains(g)).cloned().collect()),
    }
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

    // users

    pub async fn user_count(&self) -> i64 {
        sqlx::query_scalar("SELECT COUNT(*) FROM users")
            .fetch_one(&self.pool)
            .await
            .unwrap_or(0)
    }

    /// `None` if the username is taken (names are case-insensitive).
    pub async fn create_user(
        &self,
        username: &str,
        password_hash: &str,
        is_admin: bool,
    ) -> Option<i64> {
        sqlx::query(
            "INSERT INTO users (username, password_hash, is_admin, created) VALUES (?1, ?2, ?3, ?4)",
        )
        .bind(username)
        .bind(password_hash)
        .bind(is_admin)
        .bind(now_secs())
        .execute(&self.pool)
        .await
        .ok()
        .map(|r| r.last_insert_rowid())
    }

    /// Creates the first user as an admin, only while no user exists.
    pub async fn create_first_admin(&self, username: &str, password_hash: &str) -> Option<i64> {
        sqlx::query(
            "INSERT INTO users (username, password_hash, is_admin, created)
             SELECT ?1, ?2, 1, ?3 WHERE NOT EXISTS (SELECT 1 FROM users)",
        )
        .bind(username)
        .bind(password_hash)
        .bind(now_secs())
        .execute(&self.pool)
        .await
        .ok()
        .filter(|r| r.rows_affected() == 1)
        .map(|r| r.last_insert_rowid())
    }

    /// The user and their password hash, for logging in.
    pub async fn user_login(&self, username: &str) -> Option<(User, String)> {
        sqlx::query(
            "SELECT id, username, is_admin, created, scope, password_hash FROM users WHERE username = ?1",
        )
        .bind(username)
        .fetch_optional(&self.pool)
        .await
        .ok()
        .flatten()
        .map(|r| (user_row(&r), r.get(5)))
    }

    pub async fn user_hash(&self, id: i64) -> Option<String> {
        sqlx::query_scalar("SELECT password_hash FROM users WHERE id = ?1")
            .bind(id)
            .fetch_optional(&self.pool)
            .await
            .ok()
            .flatten()
    }

    pub async fn list_users(&self) -> Vec<User> {
        sqlx::query("SELECT id, username, is_admin, created, scope FROM users ORDER BY id")
            .fetch_all(&self.pool)
            .await
            .map(|rows| rows.iter().map(user_row).collect())
            .unwrap_or_default()
    }

    pub async fn admin_count(&self) -> i64 {
        sqlx::query_scalar("SELECT COUNT(*) FROM users WHERE is_admin = 1")
            .fetch_one(&self.pool)
            .await
            .unwrap_or(0)
    }

    pub async fn set_password(&self, id: i64, password_hash: &str) -> bool {
        sqlx::query("UPDATE users SET password_hash = ?2 WHERE id = ?1")
            .bind(id)
            .bind(password_hash)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    /// Sets the user's group limit; `None` removes it.
    pub async fn set_user_scope(&self, id: i64, scope: Option<&[String]>) -> bool {
        let scope = scope.map(|s| serde_json::to_string(s).unwrap_or_else(|_| "[]".into()));
        sqlx::query("UPDATE users SET scope = ?2 WHERE id = ?1")
            .bind(id)
            .bind(scope)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    pub async fn set_admin(&self, id: i64, is_admin: bool) -> bool {
        sqlx::query("UPDATE users SET is_admin = ?2 WHERE id = ?1")
            .bind(id)
            .bind(is_admin)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    /// Removes the user together with their sessions and tokens.
    pub async fn delete_user(&self, id: i64) -> bool {
        let _ = sqlx::query("DELETE FROM sessions WHERE user_id = ?1")
            .bind(id)
            .execute(&self.pool)
            .await;
        let _ = sqlx::query("DELETE FROM tokens WHERE user_id = ?1")
            .bind(id)
            .execute(&self.pool)
            .await;
        sqlx::query("DELETE FROM users WHERE id = ?1")
            .bind(id)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    // sessions

    pub async fn create_session(&self, token_hash: &str, user_id: i64, user_agent: &str) {
        let now = now_secs();
        let _ = sqlx::query("DELETE FROM sessions WHERE expires < ?1 OR last_seen < ?2")
            .bind(now)
            .bind(now - SESSION_IDLE_SECS)
            .execute(&self.pool)
            .await;
        let agent: String = user_agent.chars().take(USER_AGENT_MAX).collect();
        let _ = sqlx::query(
            "INSERT OR REPLACE INTO sessions (token_hash, expires, user_id, created, last_seen, user_agent)
             VALUES (?1, ?2, ?3, ?4, ?4, ?5)",
        )
        .bind(token_hash)
        .bind(now + SESSION_TTL_SECS)
        .bind(user_id)
        .bind(now)
        .bind(agent)
        .execute(&self.pool)
        .await;
    }

    /// The session's user, unless it expired or sat idle for too long.
    pub async fn session_user(&self, token_hash: &str) -> Option<User> {
        let now = now_secs();
        let user = sqlx::query(
            "SELECT u.id, u.username, u.is_admin, u.created, u.scope FROM sessions s
             JOIN users u ON u.id = s.user_id
             WHERE s.token_hash = ?1 AND s.expires > ?2 AND s.last_seen > ?3",
        )
        .bind(token_hash)
        .bind(now)
        .bind(now - SESSION_IDLE_SECS)
        .fetch_optional(&self.pool)
        .await
        .ok()
        .flatten()
        .map(|r| user_row(&r))?;
        let _ = sqlx::query(
            "UPDATE sessions SET last_seen = ?2 WHERE token_hash = ?1 AND last_seen < ?3",
        )
        .bind(token_hash)
        .bind(now)
        .bind(now - SESSION_TOUCH_SECS)
        .execute(&self.pool)
        .await;
        Some(user)
    }

    /// The user's live sessions, newest first; `current_hash` marks the caller's own.
    pub async fn list_sessions(&self, user_id: i64, current_hash: &str) -> Vec<SessionInfo> {
        let now = now_secs();
        sqlx::query(
            "SELECT rowid, created, last_seen, user_agent, token_hash = ?2 FROM sessions
             WHERE user_id = ?1 AND expires > ?3 AND last_seen > ?4 ORDER BY created DESC, rowid DESC",
        )
        .bind(user_id)
        .bind(current_hash)
        .bind(now)
        .bind(now - SESSION_IDLE_SECS)
        .fetch_all(&self.pool)
        .await
        .unwrap_or_default()
        .iter()
        .map(|r| SessionInfo {
            id: r.get(0),
            created: r.get(1),
            last_seen: r.get(2),
            user_agent: r.get::<Option<String>, _>(3).unwrap_or_default(),
            current: r.get(4),
        })
        .collect()
    }

    /// Ends one of the user's own sessions; false when it is not theirs or does not exist.
    pub async fn delete_session_by_id(&self, user_id: i64, id: i64) -> bool {
        sqlx::query("DELETE FROM sessions WHERE rowid = ?1 AND user_id = ?2")
            .bind(id)
            .bind(user_id)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    /// Ends every session of the user except the one with `keep_hash`; returns how many ended.
    pub async fn delete_other_sessions(&self, user_id: i64, keep_hash: &str) -> u64 {
        sqlx::query("DELETE FROM sessions WHERE user_id = ?1 AND token_hash <> ?2")
            .bind(user_id)
            .bind(keep_hash)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected())
            .unwrap_or(0)
    }

    pub async fn delete_session(&self, token_hash: &str) {
        let _ = sqlx::query("DELETE FROM sessions WHERE token_hash = ?1")
            .bind(token_hash)
            .execute(&self.pool)
            .await;
    }

    /// Ends every session of the user (after a password change).
    pub async fn delete_user_sessions(&self, user_id: i64) {
        let _ = sqlx::query("DELETE FROM sessions WHERE user_id = ?1")
            .bind(user_id)
            .execute(&self.pool)
            .await;
    }

    // long-lived tokens (only the hash is stored)

    pub async fn create_token(
        &self,
        user_id: i64,
        name: &str,
        token_hash: &str,
        scope: Option<&[String]>,
        read_only: bool,
        expires: Option<i64>,
    ) -> Option<i64> {
        let scope = scope.map(|s| serde_json::to_string(s).unwrap_or_else(|_| "[]".into()));
        let now = now_secs();
        // Expired tokens never authenticate; this just keeps the table tidy.
        let _ = sqlx::query("DELETE FROM tokens WHERE expires IS NOT NULL AND expires < ?1")
            .bind(now)
            .execute(&self.pool)
            .await;
        sqlx::query(
            "INSERT INTO tokens (name, token_hash, created, scope, user_id, read_only, expires)
             VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)",
        )
        .bind(name)
        .bind(token_hash)
        .bind(now)
        .bind(scope)
        .bind(user_id)
        .bind(read_only)
        .bind(expires)
        .execute(&self.pool)
        .await
        .ok()
        .map(|r| r.last_insert_rowid())
    }

    /// All tokens, or only those of `user_id` when given.
    pub async fn list_tokens(&self, user_id: Option<i64>) -> Vec<TokenInfo> {
        sqlx::query(
            "SELECT t.id, t.name, t.created, t.last_used, t.scope, t.user_id, u.username, t.read_only, t.expires
             FROM tokens t LEFT JOIN users u ON u.id = t.user_id
             WHERE ?1 IS NULL OR t.user_id = ?1 ORDER BY t.id",
        )
        .bind(user_id)
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
                    user_id: r.get(5),
                    username: r.get(6),
                    read_only: r.get(7),
                    expires: r.get(8),
                })
                .collect()
        })
        .unwrap_or_default()
    }

    /// Revokes a token; with `user_id` only if it belongs to that user.
    pub async fn revoke_token(&self, id: i64, user_id: Option<i64>) -> bool {
        sqlx::query("DELETE FROM tokens WHERE id = ?1 AND (?2 IS NULL OR user_id = ?2)")
            .bind(id)
            .bind(user_id)
            .execute(&self.pool)
            .await
            .map(|r| r.rows_affected() > 0)
            .unwrap_or(false)
    }

    /// Authenticates a token by its hash: `None` if unknown or expired. The scope is the token's
    /// own narrowed by its owner's, so shrinking a user's groups shrinks their tokens too.
    pub async fn token_auth(&self, token_hash: &str) -> Option<TokenAuth> {
        let row = sqlx::query(
            "SELECT t.scope, t.read_only, u.scope FROM tokens t LEFT JOIN users u ON u.id = t.user_id
             WHERE t.token_hash = ?1 AND (t.expires IS NULL OR t.expires > ?2)",
        )
        .bind(token_hash)
        .bind(now_secs())
        .fetch_optional(&self.pool)
        .await
        .ok()
        .flatten();
        let auth = row.map(|r| TokenAuth {
            scope: intersect_scope(&parse_scope(r.get(0)), &parse_scope(r.get(2))),
            read_only: r.get(1),
        });
        if auth.is_some() {
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
        auth
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
    async fn first_admin_only_while_no_user_exists() {
        let s = Store::open_memory().await.unwrap();
        assert_eq!(s.user_count().await, 0);
        let id = s.create_first_admin("Root", "a").await.unwrap();
        assert!(s.create_first_admin("other", "b").await.is_none());
        let (user, hash) = s.user_login("root").await.unwrap();
        assert_eq!((user.id, user.is_admin, hash.as_str()), (id, true, "a"));
        assert_eq!(s.user_login("root").await.unwrap().0.username, "Root");
    }

    #[tokio::test]
    async fn user_management() {
        let s = Store::open_memory().await.unwrap();
        let admin = s.create_first_admin("admin", "a").await.unwrap();
        let bob = s.create_user("bob", "b", false).await.unwrap();
        assert!(
            s.create_user("BOB", "x", false).await.is_none(),
            "case-insensitive"
        );
        assert_eq!(s.admin_count().await, 1);
        assert!(s.set_admin(bob, true).await);
        assert_eq!(s.admin_count().await, 2);
        assert!(s.set_password(bob, "new").await);
        assert_eq!(s.user_hash(bob).await.as_deref(), Some("new"));
        assert_eq!(s.list_users().await.len(), 2);

        s.create_session("sb", bob, "").await;
        s.create_token(bob, "watch", "hb", None, false, None)
            .await
            .unwrap();
        assert!(s.delete_user(bob).await);
        assert!(!s.delete_user(bob).await);
        assert!(s.session_user("sb").await.is_none());
        assert!(
            s.token_auth("hb").await.is_none(),
            "tokens go with the user"
        );
        assert_eq!(s.list_users().await[0].id, admin);
    }

    #[tokio::test]
    async fn token_lifecycle() {
        let s = Store::open_memory().await.unwrap();
        let (a, b) = (
            s.create_user("a", "x", false).await.unwrap(),
            s.create_user("b", "x", false).await.unwrap(),
        );
        let id = s
            .create_token(a, "watch", "h1", None, false, None)
            .await
            .unwrap();
        s.create_token(b, "other", "h3", None, false, None)
            .await
            .unwrap();
        assert!(s.list_tokens(Some(a)).await[0].last_used.is_none());
        assert!(s.token_auth("h1").await.is_some());
        assert!(s.list_tokens(Some(a)).await[0].last_used.is_some());
        assert!(!s.token_auth("h2").await.is_some());
        assert_eq!(s.list_tokens(Some(a)).await.len(), 1);
        assert_eq!(s.list_tokens(Some(a)).await[0].name, "watch");
        assert_eq!(s.list_tokens(None).await.len(), 2);
        assert_eq!(s.list_tokens(None).await[1].username.as_deref(), Some("b"));
        assert!(!s.revoke_token(id, Some(b)).await, "not b's token");
        assert!(s.revoke_token(id, Some(a)).await);
        assert!(!s.token_auth("h1").await.is_some());
        assert!(!s.revoke_token(id, None).await);
    }

    #[tokio::test]
    async fn token_scope_roundtrip() {
        let s = Store::open_memory().await.unwrap();
        s.create_token(1, "open", "h-open", None, false, None)
            .await
            .unwrap();
        let scope = ["a".to_string(), "b".to_string()];
        s.create_token(1, "scoped", "h-scoped", Some(&scope), false, None)
            .await
            .unwrap();
        let scope_of = async |h: &str| s.token_auth(h).await.map(|a| a.scope);
        assert_eq!(scope_of("h-open").await, Some(None));
        assert_eq!(scope_of("h-scoped").await, Some(Some(scope.to_vec())));
        assert_eq!(scope_of("nope").await, None);
        let list = s.list_tokens(None).await;
        assert_eq!(list[0].scope, None);
        assert_eq!(list[1].scope.as_deref(), Some(&scope[..]));
    }

    fn scope(groups: &[&str]) -> Scope {
        Some(groups.iter().map(|g| g.to_string()).collect())
    }

    #[test]
    fn scopes_intersect() {
        assert_eq!(intersect_scope(&None, &None), None);
        assert_eq!(intersect_scope(&None, &scope(&["a"])), scope(&["a"]));
        assert_eq!(intersect_scope(&scope(&["a"]), &None), scope(&["a"]));
        assert_eq!(
            intersect_scope(&scope(&["a", "b"]), &scope(&["b", "c"])),
            scope(&["b"])
        );
        // nothing in common: nothing allowed, never everything
        assert_eq!(intersect_scope(&scope(&["a"]), &scope(&["b"])), scope(&[]));
    }

    #[tokio::test]
    async fn read_only_expiry_and_user_scope_shape_a_token() {
        let s = Store::open_memory().await.unwrap();
        let now = now_secs();
        let u = s.create_user("u", "h", false).await.unwrap();
        let ga = ["a".to_string(), "b".to_string()];
        s.create_token(u, "ro", "h-ro", None, true, None)
            .await
            .unwrap();
        s.create_token(u, "live", "h-live", Some(&ga), false, Some(now + 100))
            .await
            .unwrap();
        s.create_token(u, "dead", "h-dead", None, false, Some(now - 1))
            .await
            .unwrap();
        assert!(s.token_auth("h-ro").await.unwrap().read_only);
        assert!(!s.token_auth("h-live").await.unwrap().read_only);
        assert!(s.token_auth("h-dead").await.is_none(), "expired");
        let list = s.list_tokens(None).await;
        assert_eq!(list[1].expires, Some(now + 100));
        assert!(list[0].read_only && list[0].expires.is_none());

        // shrinking the owner's groups shrinks the token with it
        assert!(s.set_user_scope(u, Some(&["b".to_string()])).await);
        assert_eq!(s.token_auth("h-live").await.unwrap().scope, scope(&["b"]));
        assert_eq!(s.token_auth("h-ro").await.unwrap().scope, scope(&["b"]));
        assert!(s.set_user_scope(u, None).await);
        assert_eq!(
            s.token_auth("h-live").await.unwrap().scope,
            scope(&["a", "b"])
        );
        assert_eq!(s.list_users().await[0].scope, None);

        // creating a token tidies expired ones away
        s.create_token(u, "new", "h-new", None, false, None)
            .await
            .unwrap();
        assert_eq!(s.list_tokens(None).await.len(), 3);
    }

    #[test]
    fn unparseable_scope_allows_nothing() {
        assert_eq!(parse_scope(None), None);
        assert_eq!(parse_scope(Some("[\"a\"]".into())), Some(vec!["a".into()]));
        assert_eq!(parse_scope(Some("not json".into())), Some(Vec::new()));
    }

    #[tokio::test]
    async fn idle_sessions_expire_and_activity_is_throttled() {
        let s = Store::open_memory().await.unwrap();
        let id = s.create_user("u", "h", false).await.unwrap();
        s.create_session("a", id, &"x".repeat(500)).await;
        let list = s.list_sessions(id, "a").await;
        assert_eq!(list.len(), 1);
        assert!(list[0].current);
        assert_eq!(list[0].user_agent.len(), 200, "user agent is truncated");

        let set_seen = |secs_ago: i64| {
            let pool = s.pool.clone();
            async move {
                sqlx::query("UPDATE sessions SET last_seen = ?1")
                    .bind(now_secs() - secs_ago)
                    .execute(&pool)
                    .await
                    .unwrap();
            }
        };
        let seen = || async { s.list_sessions(id, "a").await[0].last_seen };

        // a recent request does not write again
        set_seen(10).await;
        let before = seen().await;
        assert!(s.session_user("a").await.is_some());
        assert_eq!(seen().await, before);
        // after a minute it is refreshed
        set_seen(120).await;
        assert!(s.session_user("a").await.is_some());
        assert!(seen().await >= now_secs() - 1);

        // idle for a day: dead, hidden and swept by the next login
        set_seen(SESSION_IDLE_SECS + 5).await;
        assert!(s.session_user("a").await.is_none());
        assert!(s.list_sessions(id, "a").await.is_empty());
        s.create_session("b", id, "").await;
        assert_eq!(s.list_sessions(id, "b").await.len(), 1);
    }

    #[tokio::test]
    async fn session_lifecycle() {
        let s = Store::open_memory().await.unwrap();
        let id = s.create_user("u", "h", false).await.unwrap();
        assert!(s.session_user("x").await.is_none());
        s.create_session("x", id, "").await;
        assert_eq!(s.session_user("x").await.unwrap().username, "u");
        s.delete_session("x").await;
        assert!(s.session_user("x").await.is_none());
        s.create_session("y", id, "").await;
        s.delete_user_sessions(id).await;
        assert!(s.session_user("y").await.is_none());
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
        let (admin, hash) = s.user_login("admin").await.unwrap();
        assert_eq!((admin.id, admin.is_admin, hash.as_str()), (1, true, "hash"));
        assert_eq!(
            s.list_tokens(Some(1)).await.len(),
            1,
            "token now belongs to the admin"
        );
        assert!(s.token_auth("th").await.is_some());
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
            s.create_first_admin("root", "a").await;
        }
        let s = Store::open(&path).await.unwrap();
        assert_eq!(s.user_hash(1).await.as_deref(), Some("a"));
        std::fs::remove_dir_all(&dir).unwrap();
    }
}
