//! Home Assistant OAuth login (`/auth/*`), as used by the official Companion apps.
//!
//! The app opens `/auth/authorize` in a web view; the page posts the credentials to
//! `/auth/login_flow`, gets a one-time code and navigates to the app's custom scheme. The app
//! then trades the code at `/auth/token` for a short-lived access token (a regular row of the
//! `tokens` table, so the normal bearer check accepts it) and a refresh token.

use axum::extract::{Query, State};
use axum::http::{StatusCode, header};
use axum::response::{Html, IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Form, Json, Router};
use serde::Deserialize;
use serde_json::json;
use std::collections::HashMap;
use std::sync::Mutex;

use crate::api::AppState;
use crate::store::NewAppToken;
use crate::throttle;
use crate::util::{now_secs, random_hex, sha256_hex, verify_dummy, verify_password};

/// Lifetime of an authorization code.
const CODE_TTL_SECS: i64 = 60;
/// Lifetime of an access token; the app refreshes shortly before it ends.
pub const ACCESS_TTL_SECS: i64 = 30 * 60;
const TOKEN_PREFIX: &str = "domus_";
/// Past this many pending codes, expired ones are dropped on the next insert.
const MAX_CODES: usize = 1_000;

const RELEASE_REDIRECT: &str = "homeassistant://auth-callback";
const DEBUG_REDIRECT: &str = "homeassistant-dev://auth-callback";

/// An official app and the one custom-scheme redirect it uses.
struct Client {
    client_id: &'static str,
    redirect_uri: &'static str,
}

/// The official apps (release builds, then debug builds). Anything else would hand out a code
/// to a stranger.
const CLIENTS: [Client; 4] = [
    Client {
        client_id: "https://home-assistant.io/iOS",
        redirect_uri: RELEASE_REDIRECT,
    },
    Client {
        client_id: "https://home-assistant.io/android",
        redirect_uri: RELEASE_REDIRECT,
    },
    Client {
        client_id: "https://home-assistant.io/iOS/dev-auth",
        redirect_uri: DEBUG_REDIRECT,
    },
    Client {
        client_id: "https://home-assistant.io/android/dev-auth",
        redirect_uri: DEBUG_REDIRECT,
    },
];

struct PendingCode {
    user_id: i64,
    client_id: String,
    expires: i64,
}

/// One-time authorization codes, kept in memory only: they live for a minute.
#[derive(Default)]
pub struct AuthCodes(Mutex<HashMap<String, PendingCode>>);

impl AuthCodes {
    fn issue(&self, user_id: i64, client_id: &str) -> String {
        let code = random_hex(16);
        let now = now_secs();
        let mut codes = self.0.lock().unwrap();
        if codes.len() >= MAX_CODES {
            codes.retain(|_, c| c.expires > now);
        }
        codes.insert(
            sha256_hex(&code),
            PendingCode {
                user_id,
                client_id: client_id.to_string(),
                expires: now + CODE_TTL_SECS,
            },
        );
        code
    }

    /// Consumes `code`: it works once, and only for the client it was issued to.
    fn redeem(&self, code: &str, client_id: &str) -> Option<i64> {
        let pending = self.0.lock().unwrap().remove(&sha256_hex(code))?;
        (pending.expires > now_secs() && pending.client_id == client_id).then_some(pending.user_id)
    }
}

fn valid_client(client_id: &str, redirect_uri: &str) -> bool {
    CLIENTS
        .iter()
        .any(|c| c.client_id == client_id && c.redirect_uri == redirect_uri)
}

fn no_store(resp: impl IntoResponse) -> Response {
    let mut resp = resp.into_response();
    resp.headers_mut()
        .insert(header::CACHE_CONTROL, "no-store".parse().unwrap());
    resp
}

fn oauth_error(error: &str) -> Response {
    no_store((StatusCode::BAD_REQUEST, Json(json!({ "error": error }))))
}

#[derive(Deserialize)]
struct AuthorizeQuery {
    #[serde(default)]
    client_id: String,
    #[serde(default)]
    redirect_uri: String,
}

const LOGIN_PAGE: &str = include_str!("oauth/login.html");

async fn authorize(Query(q): Query<AuthorizeQuery>) -> Response {
    if !valid_client(&q.client_id, &q.redirect_uri) {
        return no_store((StatusCode::BAD_REQUEST, "Unknown client or redirect URI."));
    }
    no_store(Html(LOGIN_PAGE))
}

#[derive(Deserialize)]
struct LoginFlow {
    username: String,
    password: String,
    client_id: String,
    redirect_uri: String,
}

/// Checks the credentials with the same throttle as the admin login and returns a one-time code.
async fn login_flow(State(app): State<AppState>, Json(body): Json<LoginFlow>) -> Response {
    if !valid_client(&body.client_id, &body.redirect_uri) {
        return oauth_error("invalid_request");
    }
    let name = body.username.trim();
    let key = throttle::key("login", name);
    if let Some(wait) = app.guard.check(&key, now_secs()) {
        tracing::warn!(target: "audit", user = name, wait, "app login refused: locked");
        let mut resp = no_store((
            StatusCode::TOO_MANY_REQUESTS,
            Json(json!({
                "message": format!("Too many failed attempts. Try again in {wait} seconds."),
            })),
        ));
        resp.headers_mut()
            .insert(header::RETRY_AFTER, wait.to_string().parse().unwrap());
        return resp;
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
            tracing::info!(target: "audit", user = %user.username, client = %body.client_id, "app login ok");
            let code = app.codes.issue(user.id, &body.client_id);
            no_store(Json(json!({ "code": code })))
        }
        None => {
            app.guard.fail(&key, now_secs());
            tracing::warn!(target: "audit", user = name, "app login failed");
            no_store((
                StatusCode::UNAUTHORIZED,
                Json(json!({ "message": "Wrong username or password." })),
            ))
        }
    }
}

#[derive(Deserialize, Default)]
struct TokenForm {
    grant_type: Option<String>,
    code: Option<String>,
    client_id: Option<String>,
    refresh_token: Option<String>,
    action: Option<String>,
    token: Option<String>,
}

/// Mints an access token for the refresh token `refresh_id`, returning the plain token.
async fn mint_access(
    app: &AppState,
    user_id: i64,
    client_id: &str,
    refresh_id: i64,
) -> Option<String> {
    let token = format!("{TOKEN_PREFIX}{}", random_hex(16));
    let name = if client_id.contains("/iOS") {
        "Home Assistant app (iOS)"
    } else {
        "Home Assistant app (Android)"
    };
    app.core
        .store()
        .create_app_token(NewAppToken {
            user_id,
            name,
            token_hash: &sha256_hex(&token),
            expires: now_secs() + ACCESS_TTL_SECS,
            refresh_id,
        })
        .await
        .then_some(token)
}

async fn token(State(app): State<AppState>, Form(form): Form<TokenForm>) -> Response {
    if form.action.as_deref() == Some("revoke") {
        return revoke_value(&app, form.token.as_deref()).await;
    }
    let client_id = form.client_id.as_deref().unwrap_or_default();
    match form.grant_type.as_deref() {
        Some("authorization_code") => {
            let Some(code) = form.code.as_deref() else {
                return oauth_error("invalid_request");
            };
            let Some(user_id) = app.codes.redeem(code, client_id) else {
                return oauth_error("invalid_grant");
            };
            let refresh = format!("{TOKEN_PREFIX}r_{}", random_hex(32));
            let store = app.core.store();
            let Some(refresh_id) = store
                .refresh_create(user_id, client_id, &sha256_hex(&refresh))
                .await
            else {
                return no_store(StatusCode::INTERNAL_SERVER_ERROR);
            };
            let Some(access) = mint_access(&app, user_id, client_id, refresh_id).await else {
                store.refresh_revoke(&sha256_hex(&refresh)).await;
                return no_store(StatusCode::INTERNAL_SERVER_ERROR);
            };
            no_store(Json(json!({
                "access_token": access,
                "token_type": "Bearer",
                "refresh_token": refresh,
                "expires_in": ACCESS_TTL_SECS,
            })))
        }
        Some("refresh_token") => {
            let Some(refresh) = form.refresh_token.as_deref() else {
                return oauth_error("invalid_request");
            };
            let Some((refresh_id, user_id)) = app
                .core
                .store()
                .refresh_lookup(&sha256_hex(refresh), client_id)
                .await
            else {
                return oauth_error("invalid_grant");
            };
            let Some(access) = mint_access(&app, user_id, client_id, refresh_id).await else {
                return no_store(StatusCode::INTERNAL_SERVER_ERROR);
            };
            no_store(Json(json!({
                "access_token": access,
                "token_type": "Bearer",
                "expires_in": ACCESS_TTL_SECS,
            })))
        }
        Some(_) => oauth_error("unsupported_grant_type"),
        None => oauth_error("invalid_request"),
    }
}

/// Revokes an access or refresh token. Like Home Assistant, it answers 200 whether or not the
/// token existed, so it cannot be used to probe for valid tokens.
async fn revoke_value(app: &AppState, token: Option<&str>) -> Response {
    if let Some(token) = token {
        let hash = sha256_hex(token);
        let store = app.core.store();
        if !store.refresh_revoke(&hash).await {
            store.access_revoke(&hash).await;
        }
    }
    no_store(StatusCode::OK)
}

#[derive(Deserialize)]
struct RevokeForm {
    token: Option<String>,
}

async fn revoke(State(app): State<AppState>, Form(form): Form<RevokeForm>) -> Response {
    revoke_value(&app, form.token.as_deref()).await
}

pub fn router(app: AppState) -> Router {
    Router::new()
        .route("/auth/authorize", get(authorize))
        .route("/auth/login_flow", post(login_flow))
        .route("/auth/token", post(token))
        .route("/auth/revoke", post(revoke))
        .with_state(app)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::Core;
    use crate::hue::HueManager;
    use crate::store::{NewToken, Store};
    use crate::util::hash_password;
    use axum::body::Body;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use serde_json::Value;
    use std::sync::Arc;
    use tower::ServiceExt;

    const IOS: &str = "https://home-assistant.io/iOS";
    const REDIRECT: &str = "homeassistant://auth-callback";
    const PASSWORD: &str = "correct-horse-battery";

    async fn setup() -> (Router, AppState) {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        core.store()
            .create_first_admin("admin", &hash_password(PASSWORD))
            .await
            .unwrap();
        let app = AppState::new(core.clone(), HueManager::new(core));
        (crate::build_app(app.clone()), app)
    }

    async fn send(r: &Router, req: Request<Body>) -> (StatusCode, Value) {
        let resp = r.clone().oneshot(req).await.unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        (
            status,
            serde_json::from_slice(&bytes).unwrap_or(Value::Null),
        )
    }

    async fn login_code(r: &Router, password: &str) -> (StatusCode, Value) {
        let body = json!({
            "username": "admin", "password": password,
            "client_id": IOS, "redirect_uri": REDIRECT,
        });
        send(
            r,
            Request::post("/auth/login_flow")
                .header("content-type", "application/json")
                .body(Body::from(body.to_string()))
                .unwrap(),
        )
        .await
    }

    async fn token_form(r: &Router, form: &str) -> (StatusCode, Value) {
        send(
            r,
            Request::post("/auth/token")
                .header("content-type", "application/x-www-form-urlencoded")
                .body(Body::from(form.to_string()))
                .unwrap(),
        )
        .await
    }

    async fn state_status(r: &Router, token: &str) -> StatusCode {
        r.clone()
            .oneshot(
                Request::get("/api/states/light.none")
                    .header("authorization", format!("Bearer {token}"))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap()
            .status()
    }

    #[tokio::test]
    async fn authorize_page_checks_client_and_redirect() {
        let (r, _) = setup().await;
        let ok =
            format!("/auth/authorize?response_type=code&client_id={IOS}&redirect_uri={REDIRECT}");
        let resp = r
            .clone()
            .oneshot(Request::get(&ok).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::OK);
        assert_eq!(resp.headers()["cache-control"], "no-store");
        let evil = format!("/auth/authorize?client_id={IOS}&redirect_uri=https://evil.example/cb");
        let resp = r
            .clone()
            .oneshot(Request::get(&evil).body(Body::empty()).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn full_flow_login_token_refresh_revoke() {
        let (r, _) = setup().await;
        let (status, body) = login_code(&r, PASSWORD).await;
        assert_eq!(status, StatusCode::OK);
        let code = body["code"].as_str().unwrap().to_string();

        let (status, tokens) = token_form(
            &r,
            &format!("grant_type=authorization_code&code={code}&client_id={IOS}"),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(tokens["token_type"], "Bearer");
        assert_eq!(tokens["expires_in"], ACCESS_TTL_SECS);
        let access = tokens["access_token"].as_str().unwrap().to_string();
        let refresh = tokens["refresh_token"].as_str().unwrap().to_string();
        assert_eq!(state_status(&r, &access).await, StatusCode::NOT_FOUND);

        // A code works once.
        let (status, body) = token_form(
            &r,
            &format!("grant_type=authorization_code&code={code}&client_id={IOS}"),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "invalid_grant");

        // Refreshing mints a new access token and retires the old one.
        let (status, fresh) = token_form(
            &r,
            &format!("grant_type=refresh_token&refresh_token={refresh}&client_id={IOS}"),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert!(fresh.get("refresh_token").is_none());
        let access2 = fresh["access_token"].as_str().unwrap().to_string();
        assert_eq!(state_status(&r, &access).await, StatusCode::UNAUTHORIZED);
        assert_eq!(state_status(&r, &access2).await, StatusCode::NOT_FOUND);

        // Revoking the access token (what iOS does) also ends the refresh token.
        let (status, _) = token_form(&r, &format!("action=revoke&token={access2}")).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(state_status(&r, &access2).await, StatusCode::UNAUTHORIZED);
        let (status, body) = token_form(
            &r,
            &format!("grant_type=refresh_token&refresh_token={refresh}&client_id={IOS}"),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "invalid_grant");
    }

    #[tokio::test]
    async fn revoke_endpoint_ends_refresh_token() {
        let (r, _) = setup().await;
        let code = login_code(&r, PASSWORD).await.1["code"]
            .as_str()
            .unwrap()
            .to_string();
        let (_, t) = token_form(
            &r,
            &format!("grant_type=authorization_code&code={code}&client_id={IOS}"),
        )
        .await;
        let refresh = t["refresh_token"].as_str().unwrap();
        let (status, _) = send(
            &r,
            Request::post("/auth/revoke")
                .header("content-type", "application/x-www-form-urlencoded")
                .body(Body::from(format!("token={refresh}")))
                .unwrap(),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(
            state_status(&r, t["access_token"].as_str().unwrap()).await,
            StatusCode::UNAUTHORIZED
        );
    }

    #[tokio::test]
    async fn wrong_password_and_wrong_client_are_refused() {
        let (r, _) = setup().await;
        assert_eq!(login_code(&r, "nope").await.0, StatusCode::UNAUTHORIZED);
        let code = login_code(&r, PASSWORD).await.1["code"]
            .as_str()
            .unwrap()
            .to_string();
        let other = "https://home-assistant.io/android";
        let (status, body) = token_form(
            &r,
            &format!("grant_type=authorization_code&code={code}&client_id={other}"),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "invalid_grant");
    }

    #[tokio::test]
    async fn login_flow_shares_the_login_throttle() {
        let (r, _) = setup().await;
        for _ in 0..5 {
            assert_eq!(login_code(&r, "nope").await.0, StatusCode::UNAUTHORIZED);
        }
        assert_eq!(
            login_code(&r, PASSWORD).await.0,
            StatusCode::TOO_MANY_REQUESTS
        );
    }

    /// Logs in and returns `(access token, refresh token)`.
    async fn app_login(r: &Router) -> (String, String) {
        let code = login_code(r, PASSWORD).await.1["code"]
            .as_str()
            .unwrap()
            .to_string();
        let (_, t) = token_form(
            r,
            &format!("grant_type=authorization_code&code={code}&client_id={IOS}"),
        )
        .await;
        (
            t["access_token"].as_str().unwrap().to_string(),
            t["refresh_token"].as_str().unwrap().to_string(),
        )
    }

    async fn refresh_status(r: &Router, refresh: &str) -> (StatusCode, Value) {
        token_form(
            r,
            &format!("grant_type=refresh_token&refresh_token={refresh}&client_id={IOS}"),
        )
        .await
    }

    #[tokio::test]
    async fn revoking_the_app_token_in_the_admin_list_logs_the_app_out() {
        let (r, app) = setup().await;
        let (access, refresh) = app_login(&r).await;
        let id = app.core.store().list_tokens(None).await[0].id;
        assert!(app.core.store().revoke_token(id, None).await);
        assert_eq!(state_status(&r, &access).await, StatusCode::UNAUTHORIZED);
        assert_eq!(
            refresh_status(&r, &refresh).await.0,
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn password_change_and_user_delete_end_app_logins() {
        let (r, app) = setup().await;
        let (access, refresh) = app_login(&r).await;
        app.core.store().delete_user_refresh_tokens(1).await;
        assert_eq!(state_status(&r, &access).await, StatusCode::UNAUTHORIZED);
        assert_eq!(
            refresh_status(&r, &refresh).await.0,
            StatusCode::BAD_REQUEST
        );

        let (_, refresh) = app_login(&r).await;
        assert!(app.core.store().delete_user(1).await);
        assert_eq!(
            refresh_status(&r, &refresh).await.0,
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn only_the_latest_access_token_stays_live() {
        let (r, _) = setup().await;
        let (first, refresh) = app_login(&r).await;
        let a = refresh_status(&r, &refresh).await.1["access_token"]
            .as_str()
            .unwrap()
            .to_string();
        let b = refresh_status(&r, &refresh).await.1["access_token"]
            .as_str()
            .unwrap()
            .to_string();
        assert_eq!(state_status(&r, &first).await, StatusCode::UNAUTHORIZED);
        assert_eq!(state_status(&r, &a).await, StatusCode::UNAUTHORIZED);
        assert_eq!(state_status(&r, &b).await, StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn revoking_the_refresh_token_logs_the_app_out() {
        let (r, _) = setup().await;
        let (access, refresh) = app_login(&r).await;
        let (status, _) = token_form(&r, &format!("action=revoke&token={refresh}")).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(state_status(&r, &access).await, StatusCode::UNAUTHORIZED);
        assert_eq!(
            refresh_status(&r, &refresh).await.0,
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn login_flow_refuses_unknown_or_mismatched_redirects() {
        let (r, _) = setup().await;
        for (client, redirect) in [
            (IOS, "https://evil.example/cb"),
            (IOS, "homeassistant-dev://auth-callback"),
            ("https://evil.example", REDIRECT),
        ] {
            let body = json!({
                "username": "admin", "password": PASSWORD,
                "client_id": client, "redirect_uri": redirect,
            });
            let (status, _) = send(
                &r,
                Request::post("/auth/login_flow")
                    .header("content-type", "application/json")
                    .body(Body::from(body.to_string()))
                    .unwrap(),
            )
            .await;
            assert_eq!(status, StatusCode::BAD_REQUEST, "{client} {redirect}");
        }
    }

    #[tokio::test]
    async fn expired_codes_are_refused() {
        let (r, app) = setup().await;
        let code = login_code(&r, PASSWORD).await.1["code"]
            .as_str()
            .unwrap()
            .to_string();
        for pending in app.codes.0.lock().unwrap().values_mut() {
            pending.expires = now_secs() - 1;
        }
        let (status, body) = token_form(
            &r,
            &format!("grant_type=authorization_code&code={code}&client_id={IOS}"),
        )
        .await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "invalid_grant");
    }

    /// Trades `bearer` for an admin-page session; returns the status and the cookie, if any.
    async fn from_token(r: &Router, bearer: Option<&str>) -> (StatusCode, Option<String>) {
        let mut req = Request::post("/api/domus/session/from-token");
        if let Some(t) = bearer {
            req = req.header("authorization", format!("Bearer {t}"));
        }
        let resp = r
            .clone()
            .oneshot(req.body(Body::empty()).unwrap())
            .await
            .unwrap();
        let cookie = resp
            .headers()
            .get("set-cookie")
            .map(|c| c.to_str().unwrap().split(';').next().unwrap().to_string());
        (resp.status(), cookie)
    }

    async fn admin(
        r: &Router,
        method: &str,
        uri: &str,
        cookie: &str,
        body: Value,
    ) -> (StatusCode, Value) {
        send(
            r,
            Request::builder()
                .method(method)
                .uri(uri)
                .header("content-type", "application/json")
                .header("cookie", cookie)
                .body(Body::from(body.to_string()))
                .unwrap(),
        )
        .await
    }

    #[tokio::test]
    async fn app_token_starts_a_session_that_ends_with_the_app_login() {
        let (r, _) = setup().await;
        let (access, _) = app_login(&r).await;
        let (status, cookie) = from_token(&r, Some(&access)).await;
        assert_eq!(status, StatusCode::OK);
        let cookie = cookie.unwrap();
        let (_, body) = admin(&r, "GET", "/api/domus/status", &cookie, Value::Null).await;
        assert_eq!(body["logged_in"], true);
        let (_, sessions) = admin(&r, "GET", "/api/domus/sessions", &cookie, Value::Null).await;
        assert!(
            sessions[0]["user_agent"]
                .as_str()
                .unwrap()
                .starts_with("HA app")
        );
        // Logging the app out (what iOS does) also ends the page session.
        token_form(&r, &format!("action=revoke&token={access}")).await;
        let (_, body) = admin(&r, "GET", "/api/domus/status", &cookie, Value::Null).await;
        assert_eq!(body["logged_in"], false);
    }

    #[tokio::test]
    async fn removing_the_app_in_the_admin_list_ends_its_page_session() {
        let (r, app) = setup().await;
        let (access, _) = app_login(&r).await;
        let cookie = from_token(&r, Some(&access)).await.1.unwrap();
        let id = app.core.store().list_tokens(None).await[0].id;
        assert!(app.core.store().revoke_token(id, None).await);
        let (_, body) = admin(&r, "GET", "/api/domus/status", &cookie, Value::Null).await;
        assert_eq!(body["logged_in"], false);
    }

    #[tokio::test]
    async fn only_app_login_tokens_may_start_a_session() {
        let (r, app) = setup().await;
        assert_eq!(from_token(&r, None).await.0, StatusCode::UNAUTHORIZED);
        assert_eq!(
            from_token(&r, Some("nope")).await.0,
            StatusCode::UNAUTHORIZED
        );
        // An ordinary API token (here an unrestricted one) is not accepted either.
        app.core
            .store()
            .create_token(NewToken {
                user_id: 1,
                name: "plain",
                token_hash: &sha256_hex("domus_plain"),
                scope: None,
                read_only: false,
                expires: None,
            })
            .await
            .unwrap();
        let (status, cookie) = from_token(&r, Some("domus_plain")).await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        assert!(cookie.is_none());
    }

    #[tokio::test]
    async fn app_sessions_need_the_password_for_sensitive_actions() {
        let (r, _) = setup().await;
        let (access, _) = app_login(&r).await;
        let cookie = from_token(&r, Some(&access)).await.1.unwrap();
        let new_token = json!({"name": "ci"});
        let (status, body) =
            admin(&r, "POST", "/api/domus/tokens", &cookie, new_token.clone()).await;
        assert_eq!(status, StatusCode::FORBIDDEN);
        assert_eq!(body["reauth"], true);
        let (status, _) = admin(
            &r,
            "POST",
            "/api/domus/session/reauth",
            &cookie,
            json!({"password": "nope"}),
        )
        .await;
        assert_eq!(status, StatusCode::UNAUTHORIZED);
        let (status, _) = admin(
            &r,
            "POST",
            "/api/domus/session/reauth",
            &cookie,
            json!({"password": PASSWORD}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let (status, _) = admin(&r, "POST", "/api/domus/tokens", &cookie, new_token).await;
        assert!(status.is_success(), "{status}");
    }

    #[tokio::test]
    async fn every_sensitive_admin_action_needs_the_password() {
        let (r, _) = setup().await;
        let (access, _) = app_login(&r).await;
        let cookie = from_token(&r, Some(&access)).await.1.unwrap();
        let attempts = [
            ("POST", "/api/domus/tokens", json!({"name": "ci"})),
            (
                "POST",
                "/api/domus/users",
                json!({"username": "bob", "password": "long-enough-pass"}),
            ),
            ("PUT", "/api/domus/users/1", json!({"is_admin": true})),
            ("DELETE", "/api/domus/users/1", Value::Null),
            ("POST", "/api/domus/hue/pair", json!({})),
            ("PUT", "/api/domus/groups/den", json!({"members": []})),
            ("DELETE", "/api/domus/groups/den", Value::Null),
        ];
        for (method, uri, body) in attempts {
            let (status, body) = admin(&r, method, uri, &cookie, body).await;
            assert_eq!(status, StatusCode::FORBIDDEN, "{method} {uri}");
            assert_eq!(body["reauth"], true, "{method} {uri}");
        }
    }

    #[tokio::test]
    async fn password_change_ends_the_app_session() {
        let (r, _) = setup().await;
        let (access, _) = app_login(&r).await;
        let cookie = from_token(&r, Some(&access)).await.1.unwrap();
        // the app session cannot even reach it without the old password; use a password session
        let resp = r
            .clone()
            .oneshot(
                Request::post("/api/domus/login")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        json!({"username": "admin", "password": PASSWORD}).to_string(),
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        let own = resp.headers()["set-cookie"]
            .to_str()
            .unwrap()
            .split(';')
            .next()
            .unwrap()
            .to_string();
        let (status, _) = admin(
            &r,
            "POST",
            "/api/domus/password",
            &own,
            json!({"current": PASSWORD, "new": "another-long-passphrase"}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        let (_, body) = admin(&r, "GET", "/api/domus/status", &cookie, Value::Null).await;
        assert_eq!(body["logged_in"], false);
        assert_eq!(state_status(&r, &access).await, StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn deleting_the_user_ends_the_app_session() {
        let (r, app) = setup().await;
        let (access, _) = app_login(&r).await;
        let cookie = from_token(&r, Some(&access)).await.1.unwrap();
        assert!(app.core.store().delete_user(1).await);
        let (_, body) = admin(&r, "GET", "/api/domus/status", &cookie, Value::Null).await;
        assert_eq!(body["logged_in"], false);
    }

    #[tokio::test]
    async fn a_second_exchange_replaces_the_first_session() {
        let (r, _) = setup().await;
        let (access, _) = app_login(&r).await;
        let first = from_token(&r, Some(&access)).await.1.unwrap();
        let second = from_token(&r, Some(&access)).await.1.unwrap();
        assert_ne!(first, second);
        let (_, body) = admin(&r, "GET", "/api/domus/status", &first, Value::Null).await;
        assert_eq!(body["logged_in"], false);
        let (_, body) = admin(&r, "GET", "/api/domus/status", &second, Value::Null).await;
        assert_eq!(body["logged_in"], true);
    }

    #[tokio::test]
    async fn password_sessions_are_not_asked_again() {
        let (r, _) = setup().await;
        let resp = r
            .clone()
            .oneshot(
                Request::post("/api/domus/login")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        json!({"username": "admin", "password": PASSWORD}).to_string(),
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();
        let cookie = resp.headers()["set-cookie"]
            .to_str()
            .unwrap()
            .split(';')
            .next()
            .unwrap()
            .to_string();
        let (status, _) = admin(
            &r,
            "POST",
            "/api/domus/tokens",
            &cookie,
            json!({"name": "ci"}),
        )
        .await;
        assert!(status.is_success(), "{status}");
    }

    #[tokio::test]
    async fn unknown_grant_types_are_rejected() {
        let (r, _) = setup().await;
        let (status, body) = token_form(&r, "grant_type=password&client_id=x").await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "unsupported_grant_type");
        let (status, body) = token_form(&r, "client_id=x").await;
        assert_eq!(status, StatusCode::BAD_REQUEST);
        assert_eq!(body["error"], "invalid_request");
    }
}
