//! Admin API (`/api/domus/*`, session cookie) and the embedded admin page.

use axum::Extension;
use axum::extract::{Path, Request, State};
use axum::http::{HeaderMap, Method, StatusCode, Uri, header};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get, post, put};
use axum::{Json, Router};
use rust_embed::RustEmbed;
use serde::Deserialize;
use serde_json::{Value, json};

use crate::api::AppState;
use crate::core::{CallError, LightAction, group_light_id};
use crate::hue::{PairError, bridge_base, pair};
use crate::store::{HueBridge, SESSION_TTL_SECS, User};
use crate::throttle;
use crate::util::{hash_password, now_secs, random_hex, sha256_hex, verify_dummy, verify_password};

const COOKIE: &str = "domus_session";
const MIN_PASSWORD_LEN: usize = 12;
const MAX_PASSWORD_LEN: usize = 128;
const TOKEN_PREFIX: &str = "domus_";

#[derive(RustEmbed)]
#[folder = "web/"]
struct Assets;

fn message(status: StatusCode, msg: &str) -> Response {
    (status, Json(json!({ "message": msg }))).into_response()
}

fn session_token(headers: &HeaderMap) -> Option<String> {
    let raw = headers.get(header::COOKIE)?.to_str().ok()?;
    raw.split(';')
        .filter_map(|p| p.trim().split_once('='))
        .find(|(k, _)| *k == COOKIE)
        .map(|(_, v)| v.to_string())
}

async fn current_user(app: &AppState, headers: &HeaderMap) -> Option<User> {
    let t = session_token(headers)?;
    app.core.store().session_user(&sha256_hex(&t)).await
}

fn secure_request(headers: &HeaderMap) -> bool {
    headers
        .get("x-forwarded-proto")
        .and_then(|v| v.to_str().ok())
        .is_some_and(|v| v.eq_ignore_ascii_case("https"))
}

fn cookie_header(value: &str, max_age: i64, secure: bool) -> String {
    let mut c = format!("{COOKIE}={value}; HttpOnly; SameSite=Strict; Path=/; Max-Age={max_age}");
    if secure {
        c.push_str("; Secure");
    }
    c
}

async fn start_session(app: &AppState, headers: &HeaderMap, user_id: i64) -> Response {
    let token = random_hex(32);
    app.core
        .store()
        .create_session(&sha256_hex(&token), user_id)
        .await;
    (
        [(
            header::SET_COOKIE,
            cookie_header(&token, SESSION_TTL_SECS, secure_request(headers)),
        )],
        Json(json!({"ok": true})),
    )
        .into_response()
}

async fn require_session(State(app): State<AppState>, mut req: Request, next: Next) -> Response {
    match current_user(&app, req.headers()).await {
        Some(user) => {
            req.extensions_mut().insert(user);
            next.run(req).await
        }
        None => message(StatusCode::UNAUTHORIZED, "Login required."),
    }
}

/// Runs after `require_session`, which provides the `User`.
async fn require_admin(req: Request, next: Next) -> Response {
    if req.extensions().get::<User>().is_some_and(|u| u.is_admin) {
        next.run(req).await
    } else {
        message(StatusCode::FORBIDDEN, "Admin only.")
    }
}

// --------------------------------------------------------------- handlers

async fn status(State(app): State<AppState>, headers: HeaderMap) -> Json<Value> {
    let user = current_user(&app, &headers).await;
    Json(json!({
        "setup_done": app.core.store().user_count().await > 0,
        "logged_in": user.is_some(),
        "user": user,
        "hue_paired": app.core.store().hue_get().await.is_some(),
    }))
}

#[derive(Deserialize)]
struct Credentials {
    username: String,
    password: String,
}

fn valid_username(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '_' | '-' | '.' | '@'))
}

const USERNAME_HINT: &str = "Username: letters, digits, _ - . and @ only (max 64).";
const PASSWORD_HINT: &str = "Password must be 12 to 128 characters and must not be the username.";

fn valid_password(password: &str, username: &str) -> bool {
    (MIN_PASSWORD_LEN..=MAX_PASSWORD_LEN).contains(&password.chars().count())
        && !password.eq_ignore_ascii_case(username)
}

fn too_many(wait: i64) -> Response {
    let mut resp = message(
        StatusCode::TOO_MANY_REQUESTS,
        &format!("Too many failed attempts. Try again in {wait} seconds."),
    );
    resp.headers_mut()
        .insert(header::RETRY_AFTER, wait.to_string().parse().unwrap());
    resp
}

/// Rejects cross-site state-changing requests (defence in depth next to SameSite=Strict).
/// `Sec-Fetch-Site` decides when the browser sends it; otherwise `Origin` must match the host.
async fn same_origin(req: Request, next: Next) -> Response {
    if matches!(*req.method(), Method::GET | Method::HEAD | Method::OPTIONS) {
        return next.run(req).await;
    }
    let h = req.headers();
    let text = |name: &str| h.get(name).and_then(|v| v.to_str().ok());
    let allowed = match (text("sec-fetch-site"), text("origin")) {
        (Some(site), _) => site == "same-origin" || site == "none",
        (None, None) => true,
        (None, Some(origin)) => origin.split_once("://").is_some_and(|(_, host)| {
            [text("x-forwarded-host"), text("host")]
                .into_iter()
                .flatten()
                .filter_map(|v| v.split(',').next())
                .any(|v| v.trim().eq_ignore_ascii_case(host))
        }),
    };
    if allowed {
        next.run(req).await
    } else {
        message(StatusCode::FORBIDDEN, "Cross-site request refused.")
    }
}

async fn setup(
    State(app): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<Credentials>,
) -> Response {
    let username = body.username.trim();
    if !valid_username(username) {
        return message(StatusCode::BAD_REQUEST, USERNAME_HINT);
    }
    if !valid_password(&body.password, username) {
        return message(StatusCode::BAD_REQUEST, PASSWORD_HINT);
    }
    match app
        .core
        .store()
        .create_first_admin(username, &hash_password(&body.password))
        .await
    {
        Some(id) => {
            tracing::info!(target: "audit", user = username, "first admin created");
            start_session(&app, &headers, id).await
        }
        None => message(StatusCode::CONFLICT, "Already set up."),
    }
}

async fn login(
    State(app): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<Credentials>,
) -> Response {
    let name = body.username.trim();
    let key = throttle::key("login", name);
    if let Some(wait) = app.guard.check(&key, now_secs()) {
        tracing::warn!(target: "audit", user = name, wait, "login refused: locked");
        return too_many(wait);
    }
    let user = match app.core.store().user_login(name).await {
        Some((user, hash)) => verify_password(&body.password, &hash).then_some(user),
        None => {
            verify_dummy(&body.password);
            None
        }
    };
    match user {
        Some(user) => {
            app.guard.succeed(&key);
            tracing::info!(target: "audit", user = %user.username, "login ok");
            start_session(&app, &headers, user.id).await
        }
        None => {
            app.guard.fail(&key, now_secs());
            tracing::warn!(target: "audit", user = name, "login failed");
            message(StatusCode::UNAUTHORIZED, "Wrong username or password.")
        }
    }
}

async fn logout(State(app): State<AppState>, headers: HeaderMap) -> Response {
    if let Some(t) = session_token(&headers) {
        app.core.store().delete_session(&sha256_hex(&t)).await;
    }
    (
        [(
            header::SET_COOKIE,
            cookie_header("", 0, secure_request(&headers)),
        )],
        Json(json!({"ok": true})),
    )
        .into_response()
}

#[derive(Deserialize)]
struct PasswordChange {
    current: String,
    new: String,
}

/// Changes the caller's own password; every session is ended and a fresh one is started.
async fn password_change(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    headers: HeaderMap,
    Json(body): Json<PasswordChange>,
) -> Response {
    let store = app.core.store();
    let key = throttle::key("password", &me.username);
    if let Some(wait) = app.guard.check(&key, now_secs()) {
        return too_many(wait);
    }
    match store.user_hash(me.id).await {
        Some(h) if verify_password(&body.current, &h) => app.guard.succeed(&key),
        _ => {
            app.guard.fail(&key, now_secs());
            tracing::warn!(target: "audit", user = %me.username, "password change refused: wrong current password");
            return message(StatusCode::UNAUTHORIZED, "Current password is wrong.");
        }
    }
    if !valid_password(&body.new, &me.username) {
        return message(StatusCode::BAD_REQUEST, PASSWORD_HINT);
    }
    store.set_password(me.id, &hash_password(&body.new)).await;
    store.delete_user_sessions(me.id).await;
    tracing::info!(target: "audit", user = %me.username, "password changed");
    start_session(&app, &headers, me.id).await
}

async fn users(State(app): State<AppState>) -> Json<Value> {
    Json(json!(app.core.store().list_users().await))
}

#[derive(Deserialize)]
struct NewUser {
    username: String,
    password: String,
    #[serde(default)]
    is_admin: bool,
}

async fn user_create(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Json(body): Json<NewUser>,
) -> Response {
    let username = body.username.trim();
    if !valid_username(username) {
        return message(StatusCode::BAD_REQUEST, USERNAME_HINT);
    }
    if !valid_password(&body.password, username) {
        return message(StatusCode::BAD_REQUEST, PASSWORD_HINT);
    }
    match app
        .core
        .store()
        .create_user(username, &hash_password(&body.password), body.is_admin)
        .await
    {
        Some(id) => {
            tracing::info!(target: "audit", by = %me.username, user = username, admin = body.is_admin, "user created");
            Json(json!({"id": id, "username": username})).into_response()
        }
        None => message(StatusCode::CONFLICT, "That username is taken."),
    }
}

#[derive(Deserialize)]
struct UserUpdate {
    is_admin: Option<bool>,
    password: Option<String>,
}

async fn user_update(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Path(id): Path<i64>,
    Json(body): Json<UserUpdate>,
) -> Response {
    let store = app.core.store();
    let Some(target) = store.list_users().await.into_iter().find(|u| u.id == id) else {
        return message(StatusCode::NOT_FOUND, "No such user.");
    };
    if let Some(pw) = &body.password
        && !valid_password(pw, &target.username)
    {
        return message(StatusCode::BAD_REQUEST, PASSWORD_HINT);
    }
    if body.is_admin == Some(false) && target.is_admin && store.admin_count().await <= 1 {
        return message(StatusCode::BAD_REQUEST, "Keep at least one admin.");
    }
    if let Some(admin) = body.is_admin {
        store.set_admin(id, admin).await;
        tracing::info!(target: "audit", by = %me.username, user = %target.username, admin, "role changed");
    }
    if let Some(pw) = &body.password {
        store.set_password(id, &hash_password(pw)).await;
        store.delete_user_sessions(id).await;
        tracing::info!(target: "audit", by = %me.username, user = %target.username, "password reset");
    }
    Json(json!({"ok": true})).into_response()
}

async fn user_delete(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Path(id): Path<i64>,
) -> Response {
    if id == me.id {
        return message(StatusCode::BAD_REQUEST, "You cannot delete yourself.");
    }
    if app.core.store().delete_user(id).await {
        tracing::info!(target: "audit", by = %me.username, id, "user deleted");
        Json(json!({"ok": true})).into_response()
    } else {
        message(StatusCode::NOT_FOUND, "No such user.")
    }
}

async fn hue_status(State(app): State<AppState>) -> Json<Value> {
    let bridge = app.core.store().hue_get().await;
    Json(json!({
        "paired": bridge.is_some(),
        "ip": bridge.map(|b| b.ip),
        "running": app.hue.is_running(),
    }))
}

#[derive(Deserialize)]
struct PairRequest {
    ip: String,
}

async fn hue_pair(State(app): State<AppState>, Json(body): Json<PairRequest>) -> Response {
    let ip = body.ip.trim().to_string();
    if ip.is_empty() || ip.chars().any(char::is_whitespace) {
        return message(StatusCode::BAD_REQUEST, "Enter the bridge IP address.");
    }
    let base = bridge_base(&ip);
    match pair(&base).await {
        Ok(key) => {
            app.core
                .store()
                .hue_set(&HueBridge {
                    ip,
                    key: key.clone(),
                })
                .await;
            app.hue.start(&base, &key);
            Json(json!({"ok": true})).into_response()
        }
        Err(PairError::LinkButtonNotPressed) => message(
            StatusCode::CONFLICT,
            "Press the link button on the bridge, then try again.",
        ),
        Err(PairError::Other(e)) => {
            tracing::warn!("hue pairing failed: {e}");
            message(
                StatusCode::BAD_GATEWAY,
                &format!("Could not pair with the bridge: {e}"),
            )
        }
    }
}

async fn lights(State(app): State<AppState>) -> Json<Value> {
    let list: Vec<Value> = app
        .core
        .all_states()
        .into_iter()
        .chain(app.core.group_lights().await)
        .filter(|s| s.entity_id.starts_with("light.") || s.entity_id.starts_with("scene."))
        .map(|s| {
            json!({
                "entity_id": s.entity_id,
                "name": s.attributes.get("friendly_name").cloned().unwrap_or(Value::Null),
                "state": s.state,
                "brightness": s.attributes.get("brightness").cloned().unwrap_or(Value::Null),
            })
        })
        .collect();
    Json(Value::Array(list))
}

#[derive(Deserialize)]
struct TestBody {
    entity_id: String,
    /// Lights only: true turns on, false turns off.
    on: Option<bool>,
}

/// Runs a light or scene action from the admin page, so the setup can be tested without a watch.
async fn device_test(State(app): State<AppState>, Json(body): Json<TestBody>) -> Response {
    let id = body.entity_id;
    let result = if id.starts_with("scene.") {
        app.core.activate_scene(&id).await
    } else if id.starts_with("light.") {
        let action = if body.on.unwrap_or(true) {
            LightAction::TurnOn { brightness: None }
        } else {
            LightAction::TurnOff
        };
        app.core.call_light(&id, &action).await
    } else {
        return message(
            StatusCode::BAD_REQUEST,
            "Only lights and scenes can be tested.",
        );
    };
    match result {
        Ok(()) => {
            let state = app.core.lookup(&id).await.map(|s| s.state);
            Json(json!({ "state": state })).into_response()
        }
        Err(CallError::NoIntegration) => message(StatusCode::NOT_FOUND, "Unknown entity."),
        Err(CallError::Failed(e)) => {
            tracing::warn!("test call for {id} failed: {e}");
            message(StatusCode::BAD_GATEWAY, "The bridge rejected the request.")
        }
    }
}

async fn groups(State(app): State<AppState>) -> Json<Value> {
    let store = app.core.store();
    let mut list: Vec<Value> = Vec::new();
    for n in store.group_names().await {
        list.push(json!({
            "members": store.group_members(&n).await,
            "expose_light": store.group_exposed(&n).await,
            "light_entity_id": group_light_id(&n),
            "name": n,
        }));
    }
    Json(Value::Array(list))
}

fn valid_group_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 64
        && name
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
}

#[derive(Deserialize)]
struct GroupBody {
    members: Vec<String>,
    /// Also expose the group as one light. Omitted keeps the current setting.
    expose_light: Option<bool>,
}

async fn group_put(
    State(app): State<AppState>,
    Path(name): Path<String>,
    Json(body): Json<GroupBody>,
) -> Response {
    if !valid_group_name(&name) {
        return message(
            StatusCode::BAD_REQUEST,
            "Group name: letters, digits, _ and - only (max 64).",
        );
    }
    if body
        .members
        .iter()
        .any(|m| !m.starts_with("light.") && !m.starts_with("scene."))
    {
        return message(
            StatusCode::BAD_REQUEST,
            "Members must be light or scene entities.",
        );
    }
    if body.members.contains(&group_light_id(&name)) {
        return message(
            StatusCode::BAD_REQUEST,
            "A group cannot contain its own light.",
        );
    }
    match app.core.store().group_set(&name, &body.members).await {
        Ok(()) => {
            if let Some(expose) = body.expose_light {
                app.core.store().group_set_exposed(&name, expose).await;
            }
            Json(json!({"ok": true})).into_response()
        }
        Err(e) => {
            tracing::error!("group_set failed: {e}");
            message(
                StatusCode::INTERNAL_SERVER_ERROR,
                "Could not save the group.",
            )
        }
    }
}

async fn group_delete(State(app): State<AppState>, Path(name): Path<String>) -> Response {
    if app.core.store().group_delete(&name).await {
        Json(json!({"ok": true})).into_response()
    } else {
        message(StatusCode::NOT_FOUND, "No such group.")
    }
}

/// Admins see and manage every token; everyone else only their own.
fn token_owner(me: &User) -> Option<i64> {
    (!me.is_admin).then_some(me.id)
}

async fn tokens(State(app): State<AppState>, Extension(me): Extension<User>) -> Json<Value> {
    Json(json!(app.core.store().list_tokens(token_owner(&me)).await))
}

#[derive(Deserialize)]
struct TokenBody {
    name: String,
    /// Groups the token may use; omitted or null means unrestricted.
    scope: Option<Vec<String>>,
}

async fn token_create(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Json(body): Json<TokenBody>,
) -> Response {
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 {
        return message(
            StatusCode::BAD_REQUEST,
            "Give the token a name (max 64 characters).",
        );
    }
    let scope = match body.scope {
        None => None,
        Some(mut groups) => {
            groups.sort();
            groups.dedup();
            if groups.is_empty() {
                return message(
                    StatusCode::BAD_REQUEST,
                    "Pick at least one group, or leave the token unrestricted.",
                );
            }
            for g in &groups {
                if !app.core.store().group_exists(g).await {
                    return message(StatusCode::BAD_REQUEST, &format!("No such group: {g}."));
                }
            }
            Some(groups)
        }
    };
    // 128 bits of entropy; the prefix makes the token recognisable to people and secret scanners.
    let token = format!("{TOKEN_PREFIX}{}", random_hex(16));
    match app
        .core
        .store()
        .create_token(me.id, name, &sha256_hex(&token), scope.as_deref())
        .await
    {
        // The plaintext is returned exactly once; only its hash is stored.
        Some(id) => {
            tracing::info!(target: "audit", user = %me.username, name, id, "token created");
            Json(json!({"id": id, "name": name, "token": token})).into_response()
        }
        None => message(
            StatusCode::INTERNAL_SERVER_ERROR,
            "Could not create the token.",
        ),
    }
}

async fn token_revoke(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Path(id): Path<i64>,
) -> Response {
    if app.core.store().revoke_token(id, token_owner(&me)).await {
        tracing::info!(target: "audit", user = %me.username, id, "token revoked");
        Json(json!({"ok": true})).into_response()
    } else {
        message(StatusCode::NOT_FOUND, "No such token.")
    }
}

// ----------------------------------------------------------------- static

pub async fn static_files(uri: Uri) -> Response {
    let path = uri.path().trim_start_matches('/');
    let path = if path.is_empty() { "index.html" } else { path };
    match Assets::get(path) {
        Some(file) => {
            let mime = mime_for(path);
            ([(header::CONTENT_TYPE, mime)], file.data.into_owned()).into_response()
        }
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

fn mime_for(path: &str) -> &'static str {
    match path.rsplit('.').next() {
        Some("html") => "text/html; charset=utf-8",
        Some("js") => "text/javascript; charset=utf-8",
        Some("css") => "text/css; charset=utf-8",
        _ => "application/octet-stream",
    }
}

/// Mounted at `/api/domus`.
pub fn router(app: AppState) -> Router {
    let admin = Router::new()
        .route("/hue", get(hue_status))
        .route("/hue/pair", post(hue_pair))
        .route("/groups/{name}", put(group_put))
        .route("/groups/{name}", delete(group_delete))
        .route("/users", get(users).post(user_create))
        .route("/users/{id}", put(user_update).delete(user_delete))
        .layer(middleware::from_fn(require_admin));
    let protected = Router::new()
        .route("/logout", post(logout))
        .route("/password", post(password_change))
        .route("/lights", get(lights))
        .route("/devices/test", post(device_test))
        .route("/groups", get(groups))
        .route("/tokens", get(tokens).post(token_create))
        .route("/tokens/{id}", delete(token_revoke))
        .merge(admin)
        .layer(middleware::from_fn_with_state(app.clone(), require_session));
    Router::new()
        .route("/status", get(status))
        .route("/setup", post(setup))
        .route("/login", post(login))
        .merge(protected)
        .layer(middleware::from_fn(same_origin))
        .with_state(app)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::Core;
    use crate::hue::HueManager;
    use crate::store::Store;
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use http_body_util::BodyExt;
    use serde_json::Map;
    use std::sync::Arc;
    use tower::ServiceExt;

    async fn setup_app() -> (Router, AppState) {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let app = AppState::new(core.clone(), HueManager::new(core));
        (Router::new().nest("/api/domus", router(app.clone())), app)
    }

    async fn call(
        r: &Router,
        method: &str,
        uri: &str,
        cookie: Option<&str>,
        body: &str,
    ) -> (StatusCode, HeaderMap, Value) {
        let mut b = HttpRequest::builder()
            .method(method)
            .uri(uri)
            .header("content-type", "application/json");
        if let Some(c) = cookie {
            b = b.header("cookie", c);
        }
        let resp = r
            .clone()
            .oneshot(b.body(Body::from(body.to_string())).unwrap())
            .await
            .unwrap();
        let (parts, body) = resp.into_parts();
        let bytes = body.collect().await.unwrap().to_bytes();
        (
            parts.status,
            parts.headers,
            serde_json::from_slice(&bytes).unwrap_or(Value::Null),
        )
    }

    fn cookie_of(h: &HeaderMap) -> String {
        h.get("set-cookie")
            .unwrap()
            .to_str()
            .unwrap()
            .split(';')
            .next()
            .unwrap()
            .to_string()
    }

    #[tokio::test]
    async fn setup_login_logout_flow() {
        let (r, _) = setup_app().await;
        let (_, _, v) = call(&r, "GET", "/api/domus/status", None, "").await;
        assert_eq!(
            v,
            json!({"setup_done": false, "logged_in": false, "user": null, "hue_paired": false})
        );

        // bad username
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/setup",
                None,
                r#"{"username":"a b","password":"correct horse"}"#
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );

        // too short
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/setup",
                None,
                r#"{"username":"admin","password":"short"}"#
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );

        let (s, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"username":"admin","password":"correct horse"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::OK);
        let set_cookie = h.get("set-cookie").unwrap().to_str().unwrap();
        assert!(set_cookie.contains("HttpOnly") && set_cookie.contains("SameSite=Strict"));
        assert!(!set_cookie.contains("Secure"), "no Secure over plain http");
        let cookie = cookie_of(&h);

        // second setup refused
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/setup",
                None,
                r#"{"username":"second","password":"another one pw12"}"#
            )
            .await
            .0,
            StatusCode::CONFLICT
        );

        let (_, _, v) = call(&r, "GET", "/api/domus/status", Some(&cookie), "").await;
        assert_eq!(v["logged_in"], true);
        assert_eq!(v["user"]["username"], "admin");
        assert_eq!(v["user"]["is_admin"], true);

        // wrong / right password
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/login",
                None,
                r#"{"username":"admin","password":"nope nope"}"#
            )
            .await
            .0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/login",
                None,
                r#"{"username":"admin","password":"correct horse"}"#
            )
            .await
            .0,
            StatusCode::OK
        );

        // logout invalidates the session
        assert_eq!(
            call(&r, "POST", "/api/domus/logout", Some(&cookie), "")
                .await
                .0,
            StatusCode::OK
        );
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&cookie), "")
                .await
                .0,
            StatusCode::UNAUTHORIZED
        );
    }

    /// Sets up the admin, adds a regular user and logs both in; returns their cookies.
    async fn admin_and_member(r: &Router) -> (String, String) {
        let (_, h, _) = call(
            r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"username":"admin","password":"correct horse"}"#,
        )
        .await;
        let admin = cookie_of(&h);
        let (s, ..) = call(
            r,
            "POST",
            "/api/domus/users",
            Some(&admin),
            r#"{"username":"bob","password":"bob's password"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::OK);
        let (_, h, _) = call(
            r,
            "POST",
            "/api/domus/login",
            None,
            r#"{"username":"BOB","password":"bob's password"}"#,
        )
        .await;
        (admin, cookie_of(&h))
    }

    #[tokio::test]
    async fn members_cannot_use_admin_routes() {
        let (r, _) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        for (m, u) in [
            ("GET", "/api/domus/hue"),
            ("POST", "/api/domus/hue/pair"),
            ("PUT", "/api/domus/groups/G"),
            ("DELETE", "/api/domus/groups/G"),
            ("GET", "/api/domus/users"),
            ("POST", "/api/domus/users"),
            ("PUT", "/api/domus/users/1"),
            ("DELETE", "/api/domus/users/1"),
        ] {
            assert_eq!(
                call(&r, m, u, Some(&bob), "{}").await.0,
                StatusCode::FORBIDDEN,
                "{m} {u}"
            );
        }
        // what a member may do
        for u in [
            "/api/domus/lights",
            "/api/domus/groups",
            "/api/domus/tokens",
        ] {
            assert_eq!(
                call(&r, "GET", u, Some(&bob), "").await.0,
                StatusCode::OK,
                "{u}"
            );
        }
        assert_eq!(
            call(&r, "GET", "/api/domus/users", Some(&admin), "")
                .await
                .0,
            StatusCode::OK
        );
        let (_, _, v) = call(&r, "GET", "/api/domus/status", Some(&bob), "").await;
        assert_eq!(
            (
                v["user"]["username"].as_str(),
                v["user"]["is_admin"].as_bool()
            ),
            (Some("bob"), Some(false))
        );
    }

    #[tokio::test]
    async fn tokens_belong_to_their_owner() {
        let (r, app) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        let post = |c: &str, name: &str| {
            let (r, c, body) = (r.clone(), c.to_string(), format!(r#"{{"name":"{name}"}}"#));
            async move { call(&r, "POST", "/api/domus/tokens", Some(&c), &body).await }
        };
        let (_, _, a) = post(&admin, "admin-watch").await;
        let (_, _, b) = post(&bob, "bob-watch").await;

        let names = |v: &Value| -> Vec<String> {
            v.as_array()
                .unwrap()
                .iter()
                .map(|t| t["name"].as_str().unwrap().into())
                .collect()
        };
        let (_, _, mine) = call(&r, "GET", "/api/domus/tokens", Some(&bob), "").await;
        assert_eq!(names(&mine), ["bob-watch"]);
        let (_, _, all) = call(&r, "GET", "/api/domus/tokens", Some(&admin), "").await;
        assert_eq!(names(&all), ["admin-watch", "bob-watch"]);
        assert_eq!(all[1]["username"], "bob");

        // bob cannot revoke the admin's token; the admin can revoke bob's
        let revoke = |c: &str, v: &Value| {
            let (r, c, uri) = (
                r.clone(),
                c.to_string(),
                format!("/api/domus/tokens/{}", v["id"]),
            );
            async move { call(&r, "DELETE", &uri, Some(&c), "").await.0 }
        };
        assert_eq!(revoke(&bob, &a).await, StatusCode::NOT_FOUND);
        assert_eq!(revoke(&admin, &b).await, StatusCode::OK);
        let hash = sha256_hex(a["token"].as_str().unwrap());
        assert!(app.core.store().token_scope(&hash).await.is_some());
    }

    #[tokio::test]
    async fn user_management_rules() {
        let (r, app) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        let put = |c: &str, uri: &str, body: &str| {
            let (r, c, uri, body) = (r.clone(), c.to_string(), uri.to_string(), body.to_string());
            async move { call(&r, "PUT", &uri, Some(&c), &body).await.0 }
        };
        let bob_id = app.core.store().user_login("bob").await.unwrap().0.id;
        let bob_uri = format!("/api/domus/users/{bob_id}");

        // duplicates (case-insensitive) and weak passwords are refused
        let dup = r#"{"username":"Bob","password":"long enough pw12"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/users", Some(&admin), dup)
                .await
                .0,
            StatusCode::CONFLICT
        );
        let weak = r#"{"username":"eve","password":"short"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/users", Some(&admin), weak)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );

        // the last admin can be neither demoted nor deleted; nobody deletes themselves
        assert_eq!(
            put(&admin, "/api/domus/users/1", r#"{"is_admin":false}"#).await,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            call(&r, "DELETE", "/api/domus/users/1", Some(&admin), "")
                .await
                .0,
            StatusCode::BAD_REQUEST
        );

        // an admin reset signs the user out and the new password works
        assert_eq!(
            put(&admin, &bob_uri, r#"{"password":"reset reset reset"}"#).await,
            StatusCode::OK
        );
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&bob), "").await.0,
            StatusCode::UNAUTHORIZED
        );
        let login = r#"{"username":"bob","password":"reset reset reset"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/login", None, login).await.0,
            StatusCode::OK
        );

        // promote bob, then the original admin may be demoted
        assert_eq!(
            put(&admin, &bob_uri, r#"{"is_admin":true}"#).await,
            StatusCode::OK
        );
        assert_eq!(
            put(&admin, "/api/domus/users/1", r#"{"is_admin":false}"#).await,
            StatusCode::OK
        );
        assert_eq!(
            put(&admin, "/api/domus/users/99", "{}").await,
            StatusCode::FORBIDDEN
        );

        // deleting a user ends their session
        let (_, h, _) = call(&r, "POST", "/api/domus/login", None, login).await;
        let bob = cookie_of(&h);
        assert_eq!(
            call(&r, "DELETE", "/api/domus/users/1", Some(&bob), "")
                .await
                .0,
            StatusCode::OK
        );
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&admin), "")
                .await
                .0,
            StatusCode::UNAUTHORIZED
        );
    }

    #[tokio::test]
    async fn changing_own_password() {
        let (r, _) = setup_app().await;
        let (_, bob) = admin_and_member(&r).await;
        let change = |body: &str| {
            let (r, bob, body) = (r.clone(), bob.clone(), body.to_string());
            async move { call(&r, "POST", "/api/domus/password", Some(&bob), &body).await }
        };
        assert_eq!(
            change(r#"{"current":"wrong","new":"brand new pw"}"#)
                .await
                .0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            change(r#"{"current":"bob's password","new":"short"}"#)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        let (s, h, _) = change(r#"{"current":"bob's password","new":"brand new pw"}"#).await;
        assert_eq!(s, StatusCode::OK);
        // the old session is gone, the fresh one works
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&bob), "").await.0,
            StatusCode::UNAUTHORIZED
        );
        let fresh = cookie_of(&h);
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&fresh), "")
                .await
                .0,
            StatusCode::OK
        );
        let login = r#"{"username":"bob","password":"brand new pw"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/login", None, login).await.0,
            StatusCode::OK
        );
    }

    #[tokio::test]
    async fn secure_cookie_behind_https_proxy() {
        let (r, _) = setup_app().await;
        let req = HttpRequest::builder()
            .method("POST")
            .uri("/api/domus/setup")
            .header("content-type", "application/json")
            .header("x-forwarded-proto", "https")
            .body(Body::from(
                r#"{"username":"admin","password":"correct horse"}"#,
            ))
            .unwrap();
        let resp = r.clone().oneshot(req).await.unwrap();
        assert!(
            resp.headers()["set-cookie"]
                .to_str()
                .unwrap()
                .contains("Secure")
        );
    }

    #[tokio::test]
    async fn protected_routes_need_session() {
        let (r, _) = setup_app().await;
        for (m, u) in [
            ("GET", "/api/domus/tokens"),
            ("GET", "/api/domus/lights"),
            ("GET", "/api/domus/groups"),
            ("POST", "/api/domus/hue/pair"),
        ] {
            assert_eq!(
                call(&r, m, u, None, "{}").await.0,
                StatusCode::UNAUTHORIZED,
                "{u}"
            );
        }
        assert_eq!(
            call(
                &r,
                "GET",
                "/api/domus/tokens",
                Some("domus_session=forged"),
                ""
            )
            .await
            .0,
            StatusCode::UNAUTHORIZED
        );
    }

    #[tokio::test]
    async fn token_scope_is_validated_and_listed() {
        let (r, app) = setup_app().await;
        let (_, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"username":"admin","password":"correct horse"}"#,
        )
        .await;
        let c = cookie_of(&h);
        app.core
            .store()
            .group_set("Garmin", &["light.hue_a".into()])
            .await
            .unwrap();

        let post = |body: &'static str| {
            let (r, c) = (r.clone(), c.clone());
            async move { call(&r, "POST", "/api/domus/tokens", Some(&c), body).await }
        };
        assert_eq!(
            post(r#"{"name":"a","scope":[]}"#).await.0,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            post(r#"{"name":"a","scope":["Nope"]}"#).await.0,
            StatusCode::BAD_REQUEST
        );
        let (s, _, v) = post(r#"{"name":"a","scope":["Garmin","Garmin"]}"#).await;
        assert_eq!(s, StatusCode::OK);
        let hash = sha256_hex(v["token"].as_str().unwrap());
        assert_eq!(
            app.core.store().token_scope(&hash).await,
            Some(Some(vec!["Garmin".to_string()]))
        );
        assert_eq!(post(r#"{"name":"open"}"#).await.0, StatusCode::OK);

        let (_, _, list) = call(&r, "GET", "/api/domus/tokens", Some(&c), "").await;
        assert_eq!(list[0]["scope"], json!(["Garmin"]));
        assert!(list[1]["scope"].is_null());
    }

    #[tokio::test]
    async fn tokens_groups_and_lights() {
        let (r, app) = setup_app().await;
        let (_, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"username":"admin","password":"correct horse"}"#,
        )
        .await;
        let c = cookie_of(&h);

        // token: plaintext once, hash stored, usable, revocable
        let (s, _, v) = call(
            &r,
            "POST",
            "/api/domus/tokens",
            Some(&c),
            r#"{"name":"watch"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::OK);
        let token = v["token"].as_str().unwrap().to_string();
        assert!(
            token.starts_with("domus_") && token.len() == 6 + 32,
            "{token}"
        );
        assert!(
            app.core
                .store()
                .token_scope(&sha256_hex(&token))
                .await
                .is_some()
        );
        let (_, _, list) = call(&r, "GET", "/api/domus/tokens", Some(&c), "").await;
        assert_eq!(list[0]["name"], "watch");
        assert!(list[0].get("token").is_none());
        assert_eq!(
            call(&r, "POST", "/api/domus/tokens", Some(&c), r#"{"name":" "}"#)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        let id = v["id"].as_i64().unwrap();
        assert_eq!(
            call(
                &r,
                "DELETE",
                &format!("/api/domus/tokens/{id}"),
                Some(&c),
                ""
            )
            .await
            .0,
            StatusCode::OK
        );
        assert!(
            !app.core
                .store()
                .token_scope(&sha256_hex(&token))
                .await
                .is_some()
        );
        assert_eq!(
            call(
                &r,
                "DELETE",
                &format!("/api/domus/tokens/{id}"),
                Some(&c),
                ""
            )
            .await
            .0,
            StatusCode::NOT_FOUND
        );

        // lights
        app.core.set_state(
            "light.hue_a",
            "on",
            serde_json::from_value(json!({"friendly_name": "Desk"})).unwrap(),
        );
        let (_, _, l) = call(&r, "GET", "/api/domus/lights", Some(&c), "").await;
        assert_eq!(
            l,
            json!([{"entity_id": "light.hue_a", "name": "Desk", "state": "on", "brightness": null}])
        );

        // groups
        let put_ok = call(
            &r,
            "PUT",
            "/api/domus/groups/Garmin",
            Some(&c),
            r#"{"members":["light.hue_a"]}"#,
        )
        .await;
        assert_eq!(put_ok.0, StatusCode::OK);
        assert_eq!(
            call(
                &r,
                "PUT",
                "/api/domus/groups/bad%20name",
                Some(&c),
                r#"{"members":[]}"#
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            call(
                &r,
                "PUT",
                "/api/domus/groups/G",
                Some(&c),
                r#"{"members":["switch.x"]}"#
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );
        let (_, _, g) = call(&r, "GET", "/api/domus/groups", Some(&c), "").await;
        assert_eq!(
            g,
            json!([{"name": "Garmin", "members": ["light.hue_a"], "expose_light": false, "light_entity_id": "light.domus_group_Garmin"}])
        );

        // exposing: flag shown, light listed, self-membership rejected, omitted flag keeps it
        let body = r#"{"members":["light.hue_a"],"expose_light":true}"#;
        assert_eq!(
            call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), body)
                .await
                .0,
            StatusCode::OK
        );
        let (_, _, g) = call(&r, "GET", "/api/domus/groups", Some(&c), "").await;
        assert_eq!(g[0]["expose_light"], true);
        let (_, _, l) = call(&r, "GET", "/api/domus/lights", Some(&c), "").await;
        assert!(
            l.as_array()
                .unwrap()
                .iter()
                .any(|x| x["entity_id"] == "light.domus_group_Garmin" && x["state"] == "on")
        );
        // scenes are accepted as members and offered in the picker; other domains are not
        app.core.set_state("scene.hue_s", "unknown", Map::new());
        let sc = r#"{"members":["light.hue_a","scene.hue_s"]}"#;
        assert_eq!(
            call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), sc)
                .await
                .0,
            StatusCode::OK
        );
        let (_, _, l) = call(&r, "GET", "/api/domus/lights", Some(&c), "").await;
        assert!(
            l.as_array()
                .unwrap()
                .iter()
                .any(|x| x["entity_id"] == "scene.hue_s")
        );
        let sw = r#"{"members":["switch.x"]}"#;
        assert_eq!(
            call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), sw)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        let own = r#"{"members":["light.domus_group_Garmin"]}"#;
        assert_eq!(
            call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), own)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        let keep = r#"{"members":["light.hue_a"]}"#;
        call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), keep).await;
        assert!(app.core.store().group_exposed("Garmin").await);
        assert_eq!(
            call(&r, "DELETE", "/api/domus/groups/Garmin", Some(&c), "")
                .await
                .0,
            StatusCode::OK
        );
        assert_eq!(
            call(&r, "DELETE", "/api/domus/groups/Garmin", Some(&c), "")
                .await
                .0,
            StatusCode::NOT_FOUND
        );
    }

    #[tokio::test]
    async fn pairing_failure_reports_bad_gateway() {
        let (r, app) = setup_app().await;
        let (_, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"username":"admin","password":"correct horse"}"#,
        )
        .await;
        let c = cookie_of(&h);
        let (s, _, _) = call(
            &r,
            "POST",
            "/api/domus/hue/pair",
            Some(&c),
            r#"{"ip":"http://127.0.0.1:1"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::BAD_GATEWAY);
        assert!(
            app.core.store().hue_get().await.is_none(),
            "nothing stored on failure"
        );
        assert_eq!(
            call(&r, "POST", "/api/domus/hue/pair", Some(&c), r#"{"ip":""}"#)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn login_is_throttled_per_username() {
        let (r, _) = setup_app().await;
        admin_and_member(&r).await;
        let login = |user: &str, pw: &str| {
            let (r, body) = (
                r.clone(),
                format!(r#"{{"username":"{user}","password":"{pw}"}}"#),
            );
            async move { call(&r, "POST", "/api/domus/login", None, &body).await }
        };
        // unknown names are throttled the same way, so a 429 reveals nothing
        for name in ["admin", "ghost"] {
            for _ in 0..5 {
                assert_eq!(login(name, "wrong").await.0, StatusCode::UNAUTHORIZED);
            }
            let (s, h, _) = login(name, "correct horse").await;
            assert_eq!(s, StatusCode::TOO_MANY_REQUESTS, "{name}");
            let wait: i64 = h["retry-after"].to_str().unwrap().parse().unwrap();
            assert!((1..=60).contains(&wait));
        }
        // the lock is case-insensitive and does not spill onto other users
        assert_eq!(
            login("ADMIN", "correct horse").await.0,
            StatusCode::TOO_MANY_REQUESTS
        );
        assert_eq!(login("bob", "bob's password").await.0, StatusCode::OK);
    }

    #[tokio::test]
    async fn successful_login_resets_the_failure_count() {
        let (r, _) = setup_app().await;
        admin_and_member(&r).await;
        let login = |pw: &'static str| {
            let (r, body) = (
                r.clone(),
                format!(r#"{{"username":"bob","password":"{pw}"}}"#),
            );
            async move { call(&r, "POST", "/api/domus/login", None, &body).await.0 }
        };
        for _ in 0..4 {
            assert_eq!(login("wrong").await, StatusCode::UNAUTHORIZED);
        }
        assert_eq!(login("bob's password").await, StatusCode::OK);
        for _ in 0..4 {
            assert_eq!(login("wrong").await, StatusCode::UNAUTHORIZED);
        }
        assert_eq!(login("bob's password").await, StatusCode::OK);
    }

    #[tokio::test]
    async fn password_change_is_throttled() {
        let (r, _) = setup_app().await;
        let (_, bob) = admin_and_member(&r).await;
        let change = |current: &str| {
            let (r, bob, body) = (
                r.clone(),
                bob.clone(),
                format!(r#"{{"current":"{current}","new":"brand new pw 12"}}"#),
            );
            async move {
                call(&r, "POST", "/api/domus/password", Some(&bob), &body)
                    .await
                    .0
            }
        };
        for _ in 0..5 {
            assert_eq!(change("wrong").await, StatusCode::UNAUTHORIZED);
        }
        assert_eq!(
            change("bob's password").await,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    #[tokio::test]
    async fn password_rules() {
        let (r, _) = setup_app().await;
        let setup = |user: &str, pw: &str| {
            let (r, body) = (
                r.clone(),
                format!(r#"{{"username":"{user}","password":"{pw}"}}"#),
            );
            async move { call(&r, "POST", "/api/domus/setup", None, &body).await.0 }
        };
        assert_eq!(setup("admin", "elevenchars").await, StatusCode::BAD_REQUEST);
        assert_eq!(
            setup("administrator", "Administrator").await,
            StatusCode::BAD_REQUEST,
            "same as the username, any case"
        );
        let long = "x".repeat(129);
        assert_eq!(setup("admin", &long).await, StatusCode::BAD_REQUEST);
        assert_eq!(
            setup("admin", &"x".repeat(128)).await,
            StatusCode::OK,
            "128 is the limit"
        );
        // exactly 12 characters, counted as characters rather than bytes
        let (r, _) = setup_app().await;
        let body = r#"{"username":"admin","password":"密碼密碼密碼密碼密碼密碼"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/setup", None, body).await.0,
            StatusCode::OK
        );
    }

    #[tokio::test]
    async fn cross_site_requests_are_refused() {
        let (r, _) = setup_app().await;
        let send = |method: &str, headers: &[(&str, &str)]| {
            let mut b = HttpRequest::builder()
                .method(method)
                .uri("/api/domus/logout")
                .header("host", "domus.example");
            for (k, v) in headers {
                b = b.header(*k, *v);
            }
            let r = r.clone();
            async move {
                r.oneshot(b.body(Body::empty()).unwrap())
                    .await
                    .unwrap()
                    .status()
            }
        };
        // no Origin (curl, hasscontrol) passes the check; the route then needs a session
        let unauth = StatusCode::UNAUTHORIZED;
        assert_eq!(send("POST", &[]).await, unauth);
        assert_eq!(
            send("POST", &[("origin", "https://domus.example")]).await,
            unauth
        );
        assert_eq!(
            send("POST", &[("origin", "https://DOMUS.example")]).await,
            unauth
        );
        assert_eq!(
            send(
                "POST",
                &[
                    ("origin", "https://domus.example"),
                    ("host", "127.0.0.1:8123"),
                    ("x-forwarded-host", "domus.example"),
                ]
            )
            .await,
            unauth,
            "behind a proxy"
        );
        assert_eq!(
            send("POST", &[("sec-fetch-site", "same-origin")]).await,
            unauth
        );

        let forbidden = StatusCode::FORBIDDEN;
        assert_eq!(
            send("POST", &[("origin", "https://evil.example")]).await,
            forbidden
        );
        assert_eq!(send("POST", &[("origin", "null")]).await, forbidden);
        assert_eq!(
            send("DELETE", &[("origin", "https://evil.example")]).await,
            forbidden
        );
        assert_eq!(
            send("POST", &[("sec-fetch-site", "cross-site")]).await,
            forbidden
        );
        assert_eq!(
            send("POST", &[("sec-fetch-site", "same-site")]).await,
            forbidden
        );
        // reads are never blocked
        assert_ne!(
            send("GET", &[("origin", "https://evil.example")]).await,
            forbidden
        );
    }

    #[tokio::test]
    async fn static_index_is_served() {
        let resp = static_files("/".parse().unwrap()).await;
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(
            static_files("/nope.txt".parse().unwrap()).await.status(),
            StatusCode::NOT_FOUND
        );
    }

    struct Bridge;
    impl crate::core::Integration for Bridge {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.hue_") || id.starts_with("scene.hue_")
        }
        fn call_light<'a>(
            &'a self,
            id: &'a str,
            _a: &'a LightAction,
        ) -> crate::core::BoxFut<'a, Result<(), String>> {
            Box::pin(async move {
                if id.ends_with("bad") {
                    Err("boom".into())
                } else {
                    Ok(())
                }
            })
        }
        fn activate_scene<'a>(
            &'a self,
            _id: &'a str,
        ) -> crate::core::BoxFut<'a, Result<(), String>> {
            Box::pin(async { Ok(()) })
        }
    }

    #[tokio::test]
    async fn device_test_endpoint_controls_lights_and_scenes() {
        let (r, app) = setup_app().await;
        app.core.set_integration("bridge", Arc::new(Bridge));
        app.core.set_state("light.hue_a", "off", Map::new());
        app.core.set_state("light.hue_bad", "off", Map::new());
        app.core.set_state("scene.hue_s", "unknown", Map::new());
        let body = r#"{"username":"admin","password":"correct horse"}"#;
        let (_, h, _) = call(&r, "POST", "/api/domus/setup", None, body).await;
        let c = cookie_of(&h);
        let post = |b: &'static str| {
            let (r, c) = (r.clone(), c.clone());
            async move { call(&r, "POST", "/api/domus/devices/test", Some(&c), b).await }
        };

        let (s, _, v) = post(r#"{"entity_id":"light.hue_a","on":true}"#).await;
        assert_eq!((s, v["state"].as_str()), (StatusCode::OK, Some("on")));
        let (_, _, v) = post(r#"{"entity_id":"light.hue_a","on":false}"#).await;
        assert_eq!(v["state"], "off");
        let (s, _, v) = post(r#"{"entity_id":"scene.hue_s"}"#).await;
        assert_eq!(s, StatusCode::OK);
        assert!(v["state"].as_str().unwrap().contains('T'));
        assert_eq!(
            post(r#"{"entity_id":"light.hue_bad"}"#).await.0,
            StatusCode::BAD_GATEWAY
        );
        assert_eq!(
            post(r#"{"entity_id":"light.ghost"}"#).await.0,
            StatusCode::NOT_FOUND
        );
        assert_eq!(
            post(r#"{"entity_id":"switch.x"}"#).await.0,
            StatusCode::BAD_REQUEST
        );

        // requires a session
        let (s, ..) = call(
            &r,
            "POST",
            "/api/domus/devices/test",
            None,
            r#"{"entity_id":"light.hue_a"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::UNAUTHORIZED);
    }
}
