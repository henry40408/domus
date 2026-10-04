//! Admin API (`/api/domus/*`, session cookie) and the embedded admin page.

use axum::Extension;
use axum::extract::{Path, Request, State};
use axum::http::{HeaderMap, Method, StatusCode, Uri, header};
use axum::middleware::{self, Next};
use axum::response::sse::{Event, KeepAlive, Sse};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get, post, put};
use axum::{Json, Router};
use futures_util::{Stream, StreamExt};
use rust_embed::RustEmbed;
use serde::Deserialize;
use serde_json::{Value, json};
use std::convert::Infallible;
use std::time::Duration;

use crate::api::{AppState, light_action};
use crate::core::{CallError, group_light_id};
use crate::hue::{PairError, bridge_base, pair};
use crate::store::{HueBridge, SESSION_TTL_SECS, User};
use crate::throttle;
use crate::util::{
    constant_time_eq, hash_password, now_secs, random_hex, sha256_hex, verify_dummy,
    verify_password,
};

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

fn user_agent(headers: &HeaderMap) -> &str {
    headers
        .get(header::USER_AGENT)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
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
        .create_session(&sha256_hex(&token), user_id, user_agent(headers))
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
        "setup_code_required": app.setup_code.lock().unwrap().is_some(),
        "logged_in": user.is_some(),
        "user": user,
        "hue_paired": app.core.store().hue_get().await.is_some(),
        "version": crate::GIT_VERSION,
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

#[derive(Deserialize)]
struct SetupBody {
    username: String,
    password: String,
    #[serde(default)]
    setup_code: String,
}

async fn setup(
    State(app): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<SetupBody>,
) -> Response {
    let expected = app.setup_code.lock().unwrap().clone();
    if let Some(expected) = expected {
        let key = throttle::key("setup", "");
        if let Some(wait) = app.guard.check(&key, now_secs()) {
            return too_many(wait);
        }
        // Dashes are only for readability (`a1b2-c3d4`), so they are ignored on both sides.
        let plain = |c: &str| c.trim().replace('-', "");
        if !constant_time_eq(
            plain(&body.setup_code).as_bytes(),
            plain(&expected).as_bytes(),
        ) {
            app.guard.fail(&key, now_secs());
            tracing::warn!(target: "audit", "setup refused: wrong setup code");
            return message(
                StatusCode::FORBIDDEN,
                "Wrong setup code. Find it in the domus log.",
            );
        }
    }
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
            *app.setup_code.lock().unwrap() = None;
            app.guard.succeed(&throttle::key("setup", ""));
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

/// The caller's own live sessions; the one making this request is flagged `current`.
async fn sessions(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    headers: HeaderMap,
) -> Json<Value> {
    let current = session_token(&headers)
        .map(|t| sha256_hex(&t))
        .unwrap_or_default();
    Json(json!(app.core.store().list_sessions(me.id, &current).await))
}

async fn session_revoke(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Path(id): Path<i64>,
) -> Response {
    if app.core.store().delete_session_by_id(me.id, id).await {
        tracing::info!(target: "audit", user = %me.username, "session revoked");
        Json(json!({"ok": true})).into_response()
    } else {
        message(StatusCode::NOT_FOUND, "No such session.")
    }
}

async fn sessions_revoke_others(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    headers: HeaderMap,
) -> Response {
    // `require_session` already proved there is a cookie.
    let keep = session_token(&headers)
        .map(|t| sha256_hex(&t))
        .unwrap_or_default();
    let n = app.core.store().delete_other_sessions(me.id, &keep).await;
    tracing::info!(target: "audit", user = %me.username, ended = n, "other sessions revoked");
    Json(json!({"ok": true, "ended": n})).into_response()
}

/// How often an open event stream re-checks that its session is still alive.
#[cfg(not(test))]
const EVENTS_RECHECK: Duration = Duration::from_secs(30);
#[cfg(test)]
const EVENTS_RECHECK: Duration = Duration::from_millis(100);

/// Server-sent events for the admin page: one `change` event per entity the caller may see,
/// carrying only the entity id (the page reloads the list). `*` means "events were missed,
/// reload everything". The stream ends when the session does.
async fn events(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    headers: HeaderMap,
) -> Sse<impl Stream<Item = Result<Event, Infallible>>> {
    let hash = session_token(&headers)
        .map(|t| sha256_hex(&t))
        .unwrap_or_default();
    let rx = app.core.subscribe();
    let mut down = app.shutdown.subscribe();
    let ticker =
        tokio::time::interval_at(tokio::time::Instant::now() + EVENTS_RECHECK, EVENTS_RECHECK);
    let stream = futures_util::stream::unfold((rx, ticker), move |(mut rx, mut ticker)| {
        let (app, me, hash) = (app.clone(), me.clone(), hash.clone());
        async move {
            loop {
                let id = tokio::select! {
                    got = rx.recv() => match got {
                        Ok(id) => {
                            if !app.core.allowed(&me.scope, &id).await {
                                continue;
                            }
                            id
                        }
                        // Lagged: events were missed. (The sender lives in `app.core`, which this
                        // stream holds, so the channel never closes.)
                        Err(_) => "*".to_string(),
                    },
                    _ = ticker.tick() => {
                        if app.core.store().session_valid(&hash).await {
                            continue;
                        }
                        return None;
                    }
                };
                return Some((Ok(Event::default().event("change").data(id)), (rx, ticker)));
            }
        }
    });
    // Graceful shutdown waits for open responses, so end the stream when it starts.
    let stream = stream.take_until(async move {
        let _ = down.wait_for(|stopping| *stopping).await;
    });
    Sse::new(stream).keep_alive(KeepAlive::default())
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

/// Tells an absent field (`None`) from an explicit `null` (`Some(None)`).
fn present<'de, D, T>(d: D) -> Result<Option<Option<T>>, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Deserialize<'de>,
{
    Option::<T>::deserialize(d).map(Some)
}

#[derive(Deserialize)]
struct UserUpdate {
    is_admin: Option<bool>,
    password: Option<String>,
    /// Groups the user may use; `null` removes the limit, omitted leaves it alone.
    #[serde(default, deserialize_with = "present")]
    scope: Option<Option<Vec<String>>>,
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
    let scope = match body.scope {
        None => None,
        Some(None) => Some(None),
        Some(Some(groups)) => {
            if body.is_admin.unwrap_or(target.is_admin) {
                return message(StatusCode::BAD_REQUEST, "Admins are not limited to groups.");
            }
            match validate_scope(&app, groups).await {
                Ok(groups) => Some(Some(groups)),
                Err(e) => return message(StatusCode::BAD_REQUEST, &e),
            }
        }
    };
    if let Some(scope) = &scope {
        store.set_user_scope(id, scope.as_deref()).await;
        tracing::info!(target: "audit", by = %me.username, user = %target.username, scope = ?scope, "group limit changed");
    }
    if let Some(admin) = body.is_admin {
        store.set_admin(id, admin).await;
        if admin {
            // Admins are never limited.
            store.set_user_scope(id, None).await;
        }
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
            app.hue.start_synced(&base, &key).await;
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

async fn lights(State(app): State<AppState>, Extension(me): Extension<User>) -> Json<Value> {
    let states = app
        .core
        .all_states()
        .into_iter()
        .chain(app.core.group_lights().await)
        .filter(|s| s.entity_id.starts_with("light.") || s.entity_id.starts_with("scene."));
    let mut list: Vec<Value> = Vec::new();
    for s in states {
        if !app.core.allowed(&me.scope, &s.entity_id).await {
            continue;
        }
        let mut item = json!({
            "entity_id": s.entity_id,
            "name": s.attributes.get("friendly_name").cloned().unwrap_or(Value::Null),
            "state": s.state,
            "brightness": s.attributes.get("brightness").cloned().unwrap_or(Value::Null),
        });
        // Color details exist only for lights that have them.
        for key in [
            "color_mode",
            "color_temp",
            "xy_color",
            "supported_color_modes",
        ] {
            if let Some(v) = s.attributes.get(key) {
                item[key] = v.clone();
            }
        }
        list.push(item);
    }
    Json(Value::Array(list))
}

/// Rooms and zones whose lights can be controlled together, limited to what the user may see.
async fn rooms(State(app): State<AppState>, Extension(me): Extension<User>) -> Json<Value> {
    let mut list: Vec<Value> = Vec::new();
    for set in app.core.light_sets() {
        let mut visible = !set.entity_ids.is_empty();
        for id in &set.entity_ids {
            visible &= app.core.allowed(&me.scope, id).await;
        }
        if visible {
            list.push(json!(set));
        }
    }
    Json(Value::Array(list))
}

/// Runs a light or scene action from the admin page, so the setup can be tested without a watch.
/// Takes the same color, brightness and `transition` fields as the Home Assistant services, plus
/// `on` (default true) and either `entity_id` or `entity_ids` (lights to drive together).
async fn device_test(
    State(app): State<AppState>,
    Extension(me): Extension<User>,
    Json(body): Json<Value>,
) -> Response {
    let ids: Vec<String> = match (body.get("entity_id"), body.get("entity_ids")) {
        (Some(Value::String(id)), _) => vec![id.clone()],
        (_, Some(Value::Array(ids))) => ids
            .iter()
            .filter_map(|i| i.as_str().map(str::to_string))
            .collect(),
        _ => Vec::new(),
    };
    let Some(id) = ids.first().cloned() else {
        return message(StatusCode::BAD_REQUEST, "Missing entity_id.");
    };
    for i in &ids {
        if !app.core.allowed(&me.scope, i).await {
            return message(StatusCode::NOT_FOUND, "Unknown entity.");
        }
    }
    let result = if id.starts_with("scene.") {
        app.core.activate_scene(&id).await
    } else if ids.iter().all(|i| i.starts_with("light.")) {
        let service = if body.get("on").and_then(Value::as_bool).unwrap_or(true) {
            "turn_on"
        } else {
            "turn_off"
        };
        let action = match light_action(service, &body) {
            Ok(Some(a)) => a,
            Ok(None) => return message(StatusCode::BAD_REQUEST, "Unknown service."),
            Err(e) => return message(StatusCode::BAD_REQUEST, e),
        };
        if ids.len() == 1 {
            app.core.call_light(&id, &action).await
        } else {
            app.core.call_lights(&ids, &action).await
        }
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

async fn groups(State(app): State<AppState>, Extension(me): Extension<User>) -> Json<Value> {
    let store = app.core.store();
    let mut list: Vec<Value> = Vec::new();
    for n in store.group_names().await {
        if me.scope.as_ref().is_some_and(|g| !g.contains(&n)) {
            continue;
        }
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

/// Sorts and dedups a requested group list and checks it is non-empty and every group exists.
async fn validate_scope(app: &AppState, mut groups: Vec<String>) -> Result<Vec<String>, String> {
    groups.sort();
    groups.dedup();
    if groups.is_empty() {
        return Err("Pick at least one group, or leave it unrestricted.".into());
    }
    for g in &groups {
        if !app.core.store().group_exists(g).await {
            return Err(format!("No such group: {g}."));
        }
    }
    Ok(groups)
}

#[derive(Deserialize)]
struct TokenBody {
    name: String,
    /// Groups the token may use; omitted or null means everything its owner may use.
    scope: Option<Vec<String>>,
    /// May read states but not control anything.
    #[serde(default)]
    read_only: bool,
    /// Days until the token stops working; omitted means never.
    expires_days: Option<i64>,
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
    let requested = match body.scope {
        None => None,
        Some(groups) => match validate_scope(&app, groups).await {
            Ok(groups) => Some(groups),
            Err(e) => return message(StatusCode::BAD_REQUEST, &e),
        },
    };
    // A limited user can only hand out what they have: no scope means "all of mine".
    let scope = match (&me.scope, requested) {
        (None, requested) => requested,
        (Some(own), None) => Some(own.clone()),
        (Some(own), Some(groups)) => {
            if let Some(bad) = groups.iter().find(|g| !own.contains(g)) {
                return message(
                    StatusCode::FORBIDDEN,
                    &format!("Your account may not use the group {bad}."),
                );
            }
            Some(groups)
        }
    };
    let expires = match body.expires_days {
        None => None,
        Some(days) if (1..=3650).contains(&days) => Some(now_secs() + days * 86_400),
        Some(_) => {
            return message(StatusCode::BAD_REQUEST, "Expiry must be 1 to 3650 days.");
        }
    };
    // 128 bits of entropy; the prefix makes the token recognisable to people and secret scanners.
    let token = format!("{TOKEN_PREFIX}{}", random_hex(16));
    match app
        .core
        .store()
        .create_token(
            me.id,
            name,
            &sha256_hex(&token),
            scope.as_deref(),
            body.read_only,
            expires,
        )
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

/// `index.html` stamps every asset URL with the build version, so a new release means new URLs
/// and no cache (browser or CDN) can serve a stale file. Only a URL carrying the current
/// version is cached for good; anything else, and the page itself, is revalidated every time.
/// A `dev` or `-dirty` version does not change between edits, so it is never cached.
const ASSET_VERSION_PLACEHOLDER: &str = "__ASSET_VERSION__";

fn cache_control_for(uri: &Uri) -> &'static str {
    let stamped = uri.query().is_some_and(|q| {
        q.split('&')
            .any(|kv| kv.strip_prefix("v=") == Some(crate::GIT_VERSION))
    });
    let stable = crate::GIT_VERSION != "dev" && !crate::GIT_VERSION.ends_with("-dirty");
    if stamped && stable && uri.path() != "/" && uri.path() != "/index.html" {
        "public, max-age=31536000, immutable"
    } else {
        "no-cache"
    }
}

pub async fn static_files(uri: Uri) -> Response {
    let path = uri.path().trim_start_matches('/');
    let path = if path.is_empty() { "index.html" } else { path };
    match Assets::get(path) {
        Some(file) => {
            let mut body = file.data.into_owned();
            if path == "index.html" {
                body = String::from_utf8_lossy(&body)
                    .replace(ASSET_VERSION_PLACEHOLDER, crate::GIT_VERSION)
                    .into_bytes();
            }
            (
                [
                    (header::CONTENT_TYPE, mime_for(path)),
                    (header::CACHE_CONTROL, cache_control_for(&uri)),
                ],
                body,
            )
                .into_response()
        }
        None => StatusCode::NOT_FOUND.into_response(),
    }
}

fn mime_for(path: &str) -> &'static str {
    match path.rsplit('.').next() {
        Some("html") => "text/html; charset=utf-8",
        Some("js") => "text/javascript; charset=utf-8",
        Some("css") => "text/css; charset=utf-8",
        Some("svg") => "image/svg+xml",
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
        .route("/events", get(events))
        .route("/sessions", get(sessions))
        .route("/sessions/revoke-others", post(sessions_revoke_others))
        .route("/sessions/{id}", delete(session_revoke))
        .route("/lights", get(lights))
        .route("/rooms", get(rooms))
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
    use crate::core::{Core, LightAction, LightSet};
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
            json!({"setup_done": false, "setup_code_required": false, "logged_in": false, "user": null, "hue_paired": false, "version": crate::GIT_VERSION})
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
        assert!(app.core.store().token_auth(&hash).await.is_some());
    }

    /// Opens `/events` and returns the response; `next_event` then reads it frame by frame.
    async fn open_events(r: &Router, cookie: Option<&str>) -> Response {
        let mut b = HttpRequest::builder().uri("/api/domus/events");
        if let Some(c) = cookie {
            b = b.header("cookie", c);
        }
        r.clone()
            .oneshot(b.body(Body::empty()).unwrap())
            .await
            .unwrap()
    }

    async fn next_event(body: &mut Body) -> String {
        loop {
            let frame = tokio::time::timeout(Duration::from_secs(2), body.frame())
                .await
                .expect("an event within 2s")
                .expect("stream still open")
                .unwrap();
            if let Ok(data) = frame.into_data() {
                let text = String::from_utf8(data.to_vec()).unwrap();
                if !text.starts_with(':') {
                    return text;
                }
            }
        }
    }

    #[tokio::test]
    async fn events_stream_only_what_the_user_may_see() {
        let (r, app) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        let store = app.core.store();
        store
            .group_set("G", &["light.a".to_string()])
            .await
            .unwrap();
        let bob_id = store.user_login("bob").await.unwrap().0.id;
        assert!(store.set_user_scope(bob_id, Some(&["G".to_string()])).await);

        assert_eq!(
            open_events(&r, None).await.status(),
            StatusCode::UNAUTHORIZED
        );

        let resp = open_events(&r, Some(&admin)).await;
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(resp.headers()[header::CONTENT_TYPE], "text/event-stream");
        let mut admin_stream = resp.into_body();
        let mut bob_stream = open_events(&r, Some(&bob)).await.into_body();

        app.core.set_state("light.b", "on", Map::new());
        app.core.set_state("light.a", "on", Map::new());

        // the admin sees both, in order
        assert_eq!(
            next_event(&mut admin_stream).await,
            "event: change\ndata: light.b\n\n"
        );
        assert_eq!(
            next_event(&mut admin_stream).await,
            "event: change\ndata: light.a\n\n"
        );
        // bob is limited to group G, so light.b never reaches him
        assert_eq!(
            next_event(&mut bob_stream).await,
            "event: change\ndata: light.a\n\n"
        );
    }

    #[tokio::test]
    async fn events_report_missed_changes_and_end_with_the_session() {
        let (r, app) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;

        // a client that falls behind is told to reload everything
        let mut slow = open_events(&r, Some(&admin)).await.into_body();
        for i in 0..300 {
            app.core.set_state(&format!("light.l{i}"), "on", Map::new());
        }
        assert_eq!(next_event(&mut slow).await, "event: change\ndata: *\n\n");

        // the stream outlives rechecks while the session is alive, and ends once it is revoked
        let mut stream = open_events(&r, Some(&bob)).await.into_body();
        tokio::time::sleep(EVENTS_RECHECK * 3).await;
        let (_, _, list) = call(&r, "GET", "/api/domus/sessions", Some(&bob), "").await;
        let id = list[0]["id"].as_i64().unwrap();
        let uri = format!("/api/domus/sessions/{id}");
        assert_eq!(
            call(&r, "DELETE", &uri, Some(&bob), "").await.0,
            StatusCode::OK
        );
        let end = tokio::time::timeout(Duration::from_secs(2), async {
            while let Some(frame) = stream.frame().await {
                frame.unwrap();
            }
        })
        .await;
        assert!(
            end.is_ok(),
            "the stream closes after the session is revoked"
        );
    }

    #[tokio::test]
    async fn events_end_on_shutdown() {
        let (r, app) = setup_app().await;
        let (admin, _) = admin_and_member(&r).await;
        let mut stream = open_events(&r, Some(&admin)).await.into_body();
        app.shutdown.send_replace(true);
        let end = tokio::time::timeout(Duration::from_secs(2), stream.frame()).await;
        assert!(
            matches!(end, Ok(None)),
            "the stream closes when the server shuts down"
        );
    }

    #[tokio::test]
    async fn sessions_are_listed_and_revoked_per_user() {
        let (r, _) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        let login = r#"{"username":"bob","password":"bob's password"}"#;
        let (_, h, _) = call(&r, "POST", "/api/domus/login", None, login).await;
        let bob2 = cookie_of(&h);

        let (st, _, list) = call(&r, "GET", "/api/domus/sessions", Some(&bob), "").await;
        assert_eq!(st, StatusCode::OK);
        let list = list.as_array().unwrap();
        assert_eq!(list.len(), 2);
        assert_eq!(list.iter().filter(|s| s["current"] == true).count(), 1);
        assert!(list.iter().all(|s| s.get("token_hash").is_none()));
        // the admin's own list holds only the admin's session
        let (_, _, theirs) = call(&r, "GET", "/api/domus/sessions", Some(&admin), "").await;
        assert_eq!(theirs.as_array().unwrap().len(), 1);
        let admin_sid = theirs[0]["id"].as_i64().unwrap();

        // someone else's session cannot be ended, and a bogus id is just as unknown
        for id in [admin_sid, 9999] {
            let uri = format!("/api/domus/sessions/{id}");
            let (st, ..) = call(&r, "DELETE", &uri, Some(&bob), "").await;
            assert_eq!(st, StatusCode::NOT_FOUND);
        }
        assert_eq!(
            call(&r, "GET", "/api/domus/tokens", Some(&admin), "")
                .await
                .0,
            StatusCode::OK
        );

        // ending the other bob session logs that browser out
        let other = list.iter().find(|s| s["current"] == false).unwrap()["id"]
            .as_i64()
            .unwrap();
        let uri = format!("/api/domus/sessions/{other}");
        assert_eq!(
            call(&r, "DELETE", &uri, Some(&bob), "").await.0,
            StatusCode::OK
        );
        let alive = |c: &str| {
            let (r, c) = (r.clone(), c.to_string());
            async move { call(&r, "GET", "/api/domus/tokens", Some(&c), "").await.0 }
        };
        let (a, b) = (alive(&bob).await, alive(&bob2).await);
        assert!(
            (a == StatusCode::OK) != (b == StatusCode::OK),
            "exactly one of the two survives"
        );

        // revoke-others keeps the caller's session
        let (_, h, _) = call(&r, "POST", "/api/domus/login", None, login).await;
        let bob3 = cookie_of(&h);
        let (st, _, v) = call(
            &r,
            "POST",
            "/api/domus/sessions/revoke-others",
            Some(&bob3),
            "",
        )
        .await;
        assert_eq!(st, StatusCode::OK);
        assert_eq!(v["ended"], 1);
        assert_eq!(alive(&bob3).await, StatusCode::OK);
        assert_eq!(alive(&admin).await, StatusCode::OK);
        let (_, _, list) = call(&r, "GET", "/api/domus/sessions", Some(&bob3), "").await;
        assert_eq!(list.as_array().unwrap().len(), 1);
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
            app.core.store().token_auth(&hash).await.map(|a| a.scope),
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
                .token_auth(&sha256_hex(&token))
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
                .token_auth(&sha256_hex(&token))
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
    async fn setup_needs_the_one_time_code() {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let app = AppState::new(core.clone(), HueManager::new(core))
            .with_setup_code(Some("abc123".into()));
        let r = Router::new().nest("/api/domus", router(app));
        let setup = |code: &str| {
            let (r, body) = (
                r.clone(),
                format!(
                    r#"{{"username":"admin","password":"correct horse","setup_code":"{code}"}}"#
                ),
            );
            async move { call(&r, "POST", "/api/domus/setup", None, &body).await }
        };
        let (_, _, v) = call(&r, "GET", "/api/domus/status", None, "").await;
        assert_eq!(v["setup_code_required"], true);

        assert_eq!(setup("").await.0, StatusCode::FORBIDDEN);
        assert_eq!(setup("nope").await.0, StatusCode::FORBIDDEN);
        let missing = r#"{"username":"admin","password":"correct horse"}"#;
        assert_eq!(
            call(&r, "POST", "/api/domus/setup", None, missing).await.0,
            StatusCode::FORBIDDEN
        );
        let (s, h, _) = setup(" abc-123 ").await;
        assert_eq!(s, StatusCode::OK);
        assert!(h.contains_key("set-cookie"));

        // the code is spent: it cannot be reused, and the page stops asking for it
        assert_eq!(setup("abc123").await.0, StatusCode::CONFLICT);
        let (_, _, v) = call(&r, "GET", "/api/domus/status", None, "").await;
        assert_eq!(v["setup_code_required"], false);
    }

    #[tokio::test]
    async fn wrong_setup_codes_are_throttled() {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let app = AppState::new(core.clone(), HueManager::new(core))
            .with_setup_code(Some("abc123".into()));
        let r = Router::new().nest("/api/domus", router(app));
        let body = |code: &str| {
            format!(r#"{{"username":"admin","password":"correct horse","setup_code":"{code}"}}"#)
        };
        for _ in 0..5 {
            let s = call(&r, "POST", "/api/domus/setup", None, &body("bad"))
                .await
                .0;
            assert_eq!(s, StatusCode::FORBIDDEN);
        }
        // even the right code waits out the lock
        let (s, h, _) = call(&r, "POST", "/api/domus/setup", None, &body("abc123")).await;
        assert_eq!(s, StatusCode::TOO_MANY_REQUESTS);
        assert!(h.contains_key("retry-after"));
    }

    #[tokio::test]
    async fn limited_users_only_see_and_hand_out_their_groups() {
        let (r, app) = setup_app().await;
        let (admin, bob) = admin_and_member(&r).await;
        app.core.set_integration("bridge", Arc::new(Bridge));
        for id in ["light.hue_a", "light.hue_b"] {
            app.core.set_state(id, "off", Map::new());
        }
        let store = app.core.store();
        store
            .group_set("G1", &["light.hue_a".into()])
            .await
            .unwrap();
        store
            .group_set("G2", &["light.hue_b".into()])
            .await
            .unwrap();
        let bob_id = store.user_login("bob").await.unwrap().0.id;
        let uri = format!("/api/domus/users/{bob_id}");
        let put = |c: &str, uri: &str, body: &str| {
            let (r, c, uri, body) = (r.clone(), c.to_string(), uri.to_string(), body.to_string());
            async move { call(&r, "PUT", &uri, Some(&c), &body).await.0 }
        };

        // validation: unknown group, empty list, and admins cannot be limited
        assert_eq!(
            put(&admin, &uri, r#"{"scope":["Nope"]}"#).await,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            put(&admin, &uri, r#"{"scope":[]}"#).await,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            put(&admin, "/api/domus/users/1", r#"{"scope":["G1"]}"#).await,
            StatusCode::BAD_REQUEST
        );
        assert_eq!(
            put(&admin, &uri, r#"{"scope":["G1"]}"#).await,
            StatusCode::OK
        );
        let (_, _, users) = call(&r, "GET", "/api/domus/users", Some(&admin), "").await;
        assert_eq!(users[1]["scope"], json!(["G1"]));
        assert!(users[0]["scope"].is_null());

        // what bob can see and test is limited to G1
        let (_, _, g) = call(&r, "GET", "/api/domus/groups", Some(&bob), "").await;
        assert_eq!(g.as_array().unwrap().len(), 1);
        assert_eq!(g[0]["name"], "G1");
        let (_, _, l) = call(&r, "GET", "/api/domus/lights", Some(&bob), "").await;
        let ids: Vec<_> = l
            .as_array()
            .unwrap()
            .iter()
            .map(|x| x["entity_id"].as_str().unwrap())
            .collect();
        assert_eq!(ids, ["light.hue_a"]);
        let test = |c: &str, id: &str| {
            let (r, c, body) = (
                r.clone(),
                c.to_string(),
                format!(r#"{{"entity_id":"{id}"}}"#),
            );
            async move {
                call(&r, "POST", "/api/domus/devices/test", Some(&c), &body)
                    .await
                    .0
            }
        };
        assert_eq!(test(&bob, "light.hue_a").await, StatusCode::OK);
        assert_eq!(test(&bob, "light.hue_b").await, StatusCode::NOT_FOUND);
        assert_eq!(test(&admin, "light.hue_b").await, StatusCode::OK);

        // tokens: default to bob's own groups, never beyond them
        let mint = |body: &str| {
            let (r, bob, body) = (r.clone(), bob.clone(), body.to_string());
            async move { call(&r, "POST", "/api/domus/tokens", Some(&bob), &body).await }
        };
        let (s, _, _) = mint(r#"{"name":"x","scope":["G2"]}"#).await;
        assert_eq!(s, StatusCode::FORBIDDEN);
        let (s, _, _) = mint(r#"{"name":"x","scope":["G1","G2"]}"#).await;
        assert_eq!(s, StatusCode::FORBIDDEN);
        let (s, _, v) = mint(r#"{"name":"open"}"#).await;
        assert_eq!(s, StatusCode::OK);
        let auth = store
            .token_auth(&sha256_hex(v["token"].as_str().unwrap()))
            .await
            .unwrap();
        assert_eq!(auth.scope, Some(vec!["G1".to_string()]));
        assert_eq!(
            mint(r#"{"name":"ok","scope":["G1"]}"#).await.0,
            StatusCode::OK
        );
        // the admin is not limited
        let (_, _, v) = call(
            &r,
            "POST",
            "/api/domus/tokens",
            Some(&admin),
            r#"{"name":"a"}"#,
        )
        .await;
        assert_eq!(
            store
                .token_auth(&sha256_hex(v["token"].as_str().unwrap()))
                .await
                .unwrap()
                .scope,
            None
        );

        // null lifts the limit; making someone admin clears it
        assert_eq!(put(&admin, &uri, r#"{"scope":null}"#).await, StatusCode::OK);
        assert_eq!(store.list_users().await[1].scope, None);
        assert_eq!(test(&bob, "light.hue_b").await, StatusCode::OK);
        assert_eq!(
            put(&admin, &uri, r#"{"scope":["G1"]}"#).await,
            StatusCode::OK
        );
        assert_eq!(
            put(&admin, &uri, r#"{"is_admin":true}"#).await,
            StatusCode::OK
        );
        assert_eq!(store.list_users().await[1].scope, None);
    }

    #[tokio::test]
    async fn token_read_only_and_expiry_are_validated() {
        let (r, app) = setup_app().await;
        let (admin, _) = admin_and_member(&r).await;
        let mint = |body: &str| {
            let (r, c, body) = (r.clone(), admin.clone(), body.to_string());
            async move { call(&r, "POST", "/api/domus/tokens", Some(&c), &body).await }
        };
        for bad in ["0", "-5", "3651"] {
            let body = format!(r#"{{"name":"x","expires_days":{bad}}}"#);
            assert_eq!(mint(&body).await.0, StatusCode::BAD_REQUEST, "{bad}");
        }
        let (s, _, v) = mint(r#"{"name":"x","read_only":true,"expires_days":30}"#).await;
        assert_eq!(s, StatusCode::OK);
        let auth = app
            .core
            .store()
            .token_auth(&sha256_hex(v["token"].as_str().unwrap()))
            .await;
        assert!(auth.unwrap().read_only);
        let (_, _, list) = call(&r, "GET", "/api/domus/tokens", Some(&admin), "").await;
        assert_eq!(list[0]["read_only"], true);
        let left = list[0]["expires"].as_i64().unwrap() - now_secs();
        assert!((30 * 86_400 - 5..=30 * 86_400).contains(&left));
    }

    #[tokio::test]
    async fn static_index_is_served() {
        let resp = static_files("/".parse().unwrap()).await;
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(
            static_files("/nope.txt".parse().unwrap()).await.status(),
            StatusCode::NOT_FOUND
        );
        let icon = static_files("/favicon.svg".parse().unwrap()).await;
        assert_eq!(icon.status(), StatusCode::OK);
        assert_eq!(icon.headers()[header::CONTENT_TYPE], "image/svg+xml");
    }

    #[tokio::test]
    async fn assets_are_stamped_and_cached_by_version() {
        let body = |resp: Response| async {
            let bytes = axum::body::to_bytes(resp.into_body(), usize::MAX)
                .await
                .unwrap();
            String::from_utf8(bytes.to_vec()).unwrap()
        };
        let index = static_files("/".parse().unwrap()).await;
        assert_eq!(index.headers()[header::CACHE_CONTROL], "no-cache");
        let html = body(index).await;
        assert!(!html.contains(ASSET_VERSION_PLACEHOLDER));
        let stamped = format!("/app.js?v={}", crate::GIT_VERSION);
        assert!(html.contains(&stamped));

        // bare URLs are never pinned, so a stale reference cannot stick
        let bare = static_files("/app.js".parse().unwrap()).await;
        assert_eq!(bare.headers()[header::CACHE_CONTROL], "no-cache");

        // stamped ones are pinned only for a build whose version identifies its content
        let pinned = static_files(stamped.parse().unwrap()).await;
        let stable = crate::GIT_VERSION != "dev" && !crate::GIT_VERSION.ends_with("-dirty");
        assert_eq!(
            pinned.headers()[header::CACHE_CONTROL],
            if stable {
                "public, max-age=31536000, immutable"
            } else {
                "no-cache"
            }
        );
        let wrong = static_files("/app.js?v=other".parse().unwrap()).await;
        assert_eq!(wrong.headers()[header::CACHE_CONTROL], "no-cache");
        let index_stamped =
            static_files(format!("/?v={}", crate::GIT_VERSION).parse().unwrap()).await;
        assert_eq!(index_stamped.headers()[header::CACHE_CONTROL], "no-cache");
    }

    struct Bridge;
    impl crate::core::Integration for Bridge {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.hue_") || id.starts_with("scene.hue_")
        }
        fn light_sets(&self) -> Vec<LightSet> {
            vec![LightSet {
                name: "Study".into(),
                kind: "room".into(),
                entity_ids: vec!["light.hue_a".into(), "light.hue_b".into()],
            }]
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

        // color and transition use the Home Assistant field names and are validated
        let (s, _, v) =
            post(r#"{"entity_id":"light.hue_a","color_temp":300,"transition":2}"#).await;
        assert_eq!((s, v["state"].as_str()), (StatusCode::OK, Some("on")));
        assert_eq!(
            post(r#"{"entity_id":"light.hue_a","xy_color":[2,0]}"#)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        // several lights at once, and the sets that can be driven together
        let (s, _, v) = post(r#"{"entity_ids":["light.hue_a","light.hue_bad"],"on":true}"#).await;
        assert_eq!((s, v["state"].as_str()), (StatusCode::OK, Some("on")));
        assert_eq!(post(r#"{}"#).await.0, StatusCode::BAD_REQUEST);
        let (s, _, v) = call(&r, "GET", "/api/domus/rooms", Some(&c), "").await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v[0]["name"], "Study");
        assert_eq!(v[0]["entity_ids"], json!(["light.hue_a", "light.hue_b"]));

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
