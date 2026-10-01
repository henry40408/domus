//! Admin API (`/api/domus/*`, session cookie) and the embedded admin page.

use axum::extract::{Path, Request, State};
use axum::http::{HeaderMap, StatusCode, Uri, header};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{delete, get, post, put};
use axum::{Json, Router};
use rust_embed::RustEmbed;
use serde::Deserialize;
use serde_json::{Value, json};

use crate::api::AppState;
use crate::core::group_light_id;
use crate::hue::{PairError, bridge_base, pair};
use crate::store::{HueBridge, SESSION_TTL_SECS};
use crate::util::{hash_password, random_hex, sha256_hex, verify_password};

const COOKIE: &str = "domus_session";
const MIN_PASSWORD_LEN: usize = 8;

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

fn logged_in(app: &AppState, headers: &HeaderMap) -> bool {
    session_token(headers).is_some_and(|t| app.core.store().session_valid(&sha256_hex(&t)))
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

fn start_session(app: &AppState, headers: &HeaderMap) -> Response {
    let token = random_hex(32);
    app.core.store().create_session(&sha256_hex(&token));
    (
        [(
            header::SET_COOKIE,
            cookie_header(&token, SESSION_TTL_SECS, secure_request(headers)),
        )],
        Json(json!({"ok": true})),
    )
        .into_response()
}

async fn require_session(State(app): State<AppState>, req: Request, next: Next) -> Response {
    if logged_in(&app, req.headers()) {
        next.run(req).await
    } else {
        message(StatusCode::UNAUTHORIZED, "Login required.")
    }
}

// --------------------------------------------------------------- handlers

async fn status(State(app): State<AppState>, headers: HeaderMap) -> Json<Value> {
    Json(json!({
        "setup_done": app.core.store().owner_hash().is_some(),
        "logged_in": logged_in(&app, &headers),
        "hue_paired": app.core.store().hue_get().is_some(),
    }))
}

#[derive(Deserialize)]
struct Password {
    password: String,
}

async fn setup(
    State(app): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<Password>,
) -> Response {
    if body.password.chars().count() < MIN_PASSWORD_LEN {
        return message(
            StatusCode::BAD_REQUEST,
            "Password must be at least 8 characters.",
        );
    }
    if !app
        .core
        .store()
        .set_owner_if_absent(&hash_password(&body.password))
    {
        return message(StatusCode::CONFLICT, "Already set up.");
    }
    start_session(&app, &headers)
}

async fn login(
    State(app): State<AppState>,
    headers: HeaderMap,
    Json(body): Json<Password>,
) -> Response {
    match app.core.store().owner_hash() {
        Some(h) if verify_password(&body.password, &h) => start_session(&app, &headers),
        _ => message(StatusCode::UNAUTHORIZED, "Wrong password."),
    }
}

async fn logout(State(app): State<AppState>, headers: HeaderMap) -> Response {
    if let Some(t) = session_token(&headers) {
        app.core.store().delete_session(&sha256_hex(&t));
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

async fn hue_status(State(app): State<AppState>) -> Json<Value> {
    let bridge = app.core.store().hue_get();
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
            app.core.store().hue_set(&HueBridge {
                ip,
                key: key.clone(),
            });
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
        .chain(app.core.group_lights())
        .filter(|s| s.entity_id.starts_with("light."))
        .map(|s| {
            json!({
                "entity_id": s.entity_id,
                "name": s.attributes.get("friendly_name").cloned().unwrap_or(Value::Null),
                "state": s.state,
            })
        })
        .collect();
    Json(Value::Array(list))
}

async fn groups(State(app): State<AppState>) -> Json<Value> {
    let store = app.core.store();
    let list: Vec<Value> = store
        .group_names()
        .into_iter()
        .map(|n| {
            json!({
                "name": n,
                "members": store.group_members(&n),
                "expose_light": store.group_exposed(&n),
                "light_entity_id": group_light_id(&n),
            })
        })
        .collect();
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
    if body.members.iter().any(|m| !m.starts_with("light.")) {
        return message(StatusCode::BAD_REQUEST, "Members must be light entities.");
    }
    if body.members.contains(&group_light_id(&name)) {
        return message(
            StatusCode::BAD_REQUEST,
            "A group cannot contain its own light.",
        );
    }
    match app.core.store().group_set(&name, &body.members) {
        Ok(()) => {
            if let Some(expose) = body.expose_light {
                app.core.store().group_set_exposed(&name, expose);
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
    if app.core.store().group_delete(&name) {
        Json(json!({"ok": true})).into_response()
    } else {
        message(StatusCode::NOT_FOUND, "No such group.")
    }
}

async fn tokens(State(app): State<AppState>) -> Json<Value> {
    Json(json!(app.core.store().list_tokens()))
}

#[derive(Deserialize)]
struct TokenBody {
    name: String,
}

async fn token_create(State(app): State<AppState>, Json(body): Json<TokenBody>) -> Response {
    let name = body.name.trim();
    if name.is_empty() || name.chars().count() > 64 {
        return message(
            StatusCode::BAD_REQUEST,
            "Give the token a name (max 64 characters).",
        );
    }
    let token = random_hex(32);
    match app.core.store().create_token(name, &sha256_hex(&token)) {
        // The plaintext is returned exactly once; only its hash is stored.
        Some(id) => Json(json!({"id": id, "name": name, "token": token})).into_response(),
        None => message(
            StatusCode::INTERNAL_SERVER_ERROR,
            "Could not create the token.",
        ),
    }
}

async fn token_revoke(State(app): State<AppState>, Path(id): Path<i64>) -> Response {
    if app.core.store().revoke_token(id) {
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
    let protected = Router::new()
        .route("/logout", post(logout))
        .route("/hue", get(hue_status))
        .route("/hue/pair", post(hue_pair))
        .route("/lights", get(lights))
        .route("/groups", get(groups))
        .route("/groups/{name}", put(group_put))
        .route("/groups/{name}", delete(group_delete))
        .route("/tokens", get(tokens).post(token_create))
        .route("/tokens/{id}", delete(token_revoke))
        .layer(middleware::from_fn_with_state(app.clone(), require_session));
    Router::new()
        .route("/status", get(status))
        .route("/setup", post(setup))
        .route("/login", post(login))
        .merge(protected)
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
    use std::sync::Arc;
    use tower::ServiceExt;

    fn setup_app() -> (Router, AppState) {
        let core = Core::new(Arc::new(Store::open_memory().unwrap()));
        let app = AppState {
            core: core.clone(),
            hue: HueManager::new(core),
        };
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
        let (r, _) = setup_app();
        let (_, _, v) = call(&r, "GET", "/api/domus/status", None, "").await;
        assert_eq!(
            v,
            json!({"setup_done": false, "logged_in": false, "hue_paired": false})
        );

        // too short
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/setup",
                None,
                r#"{"password":"short"}"#
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
            r#"{"password":"correct horse"}"#,
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
                r#"{"password":"another one"}"#
            )
            .await
            .0,
            StatusCode::CONFLICT
        );

        let (_, _, v) = call(&r, "GET", "/api/domus/status", Some(&cookie), "").await;
        assert_eq!(v["logged_in"], true);

        // wrong / right password
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/domus/login",
                None,
                r#"{"password":"nope nope"}"#
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
                r#"{"password":"correct horse"}"#
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

    #[tokio::test]
    async fn secure_cookie_behind_https_proxy() {
        let (r, _) = setup_app();
        let req = HttpRequest::builder()
            .method("POST")
            .uri("/api/domus/setup")
            .header("content-type", "application/json")
            .header("x-forwarded-proto", "https")
            .body(Body::from(r#"{"password":"correct horse"}"#))
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
        let (r, _) = setup_app();
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
    async fn tokens_groups_and_lights() {
        let (r, app) = setup_app();
        let (_, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"password":"correct horse"}"#,
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
        assert!(app.core.store().token_valid(&sha256_hex(&token)));
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
        assert!(!app.core.store().token_valid(&sha256_hex(&token)));
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
            json!([{"entity_id": "light.hue_a", "name": "Desk", "state": "on"}])
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
        let own = r#"{"members":["light.domus_group_Garmin"]}"#;
        assert_eq!(
            call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), own)
                .await
                .0,
            StatusCode::BAD_REQUEST
        );
        let keep = r#"{"members":["light.hue_a"]}"#;
        call(&r, "PUT", "/api/domus/groups/Garmin", Some(&c), keep).await;
        assert!(app.core.store().group_exposed("Garmin"));
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
        let (r, app) = setup_app();
        let (_, h, _) = call(
            &r,
            "POST",
            "/api/domus/setup",
            None,
            r#"{"password":"correct horse"}"#,
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
            app.core.store().hue_get().is_none(),
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
    async fn static_index_is_served() {
        let resp = static_files("/".parse().unwrap()).await;
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(
            static_files("/nope.txt".parse().unwrap()).await.status(),
            StatusCode::NOT_FOUND
        );
    }
}
