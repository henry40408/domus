//! The Home Assistant `mobile_app` integration: device registration and the app's webhook.
//!
//! domus does not do anything with sensors, locations or events yet; the webhook accepts them
//! so the Companion apps see a healthy server. The webhook id is the only credential of the
//! webhook, exactly as in Home Assistant, and is never stored in plain.

use axum::body::Bytes;
use axum::extract::{Path, State};
use axum::http::StatusCode;
use axum::middleware;
use axum::response::{IntoResponse, Response};
use axum::routing::post;
use axum::{Extension, Json, Router};
use serde_json::{Map, Value, json};

use crate::api::{AppState, require_token};
use crate::store::{RegistrationError, RegistrationFields, TokenAuth};
use crate::util::{random_hex, sha256_hex};

/// The Home Assistant version domus claims to be: the apps gate features on it, and this is a
/// release old enough to need no handshake beyond what domus implements.
pub const HA_VERSION: &str = "2025.12.0";
/// Longest accepted value of any text field the app reports.
const FIELD_MAX: usize = 256;

fn message(status: StatusCode, msg: &str) -> Response {
    (status, Json(json!({ "message": msg }))).into_response()
}

/// A scalar field as text; apps are not consistent about strings versus numbers.
fn scalar(body: &Map<String, Value>, key: &str) -> Result<Option<String>, String> {
    let text = match body.get(key) {
        None | Some(Value::Null) => return Ok(None),
        Some(Value::String(s)) => s.clone(),
        Some(v @ (Value::Number(_) | Value::Bool(_))) => v.to_string(),
        Some(_) => return Err(format!("Invalid field: {key}.")),
    };
    if text.chars().count() > FIELD_MAX {
        return Err(format!("Invalid field: {key}."));
    }
    Ok(Some(text))
}

/// A required, bounded field of the registration request; empty is allowed, absent is not.
fn text(body: &Map<String, Value>, key: &str) -> Result<String, String> {
    scalar(body, key)?.ok_or_else(|| format!("Missing required field: {key}."))
}

/// An optional field; empty counts as absent.
fn optional_text(body: &Map<String, Value>, key: &str) -> Result<Option<String>, String> {
    Ok(scalar(body, key)?.filter(|s| !s.is_empty()))
}

fn registration_fields(body: &Map<String, Value>) -> Result<RegistrationFields, String> {
    Ok(RegistrationFields {
        app_id: text(body, "app_id")?,
        app_name: text(body, "app_name")?,
        app_version: text(body, "app_version")?,
        device_id: optional_text(body, "device_id")?,
        device_name: text(body, "device_name")?,
        manufacturer: text(body, "manufacturer")?,
        model: text(body, "model")?,
        os_name: text(body, "os_name")?,
        os_version: optional_text(body, "os_version")?,
    })
}

/// `POST /api/mobile_app/registrations`: registers the calling app and hands out its webhook id.
/// No `secret` is returned, so the app sends its webhooks unencrypted (the apps support that).
async fn register(
    State(app): State<AppState>,
    Extension(auth): Extension<TokenAuth>,
    Json(body): Json<Value>,
) -> Response {
    // A token limited to some groups must not mint a credential that is not.
    let Some(user_id) = auth
        .user_id
        .filter(|_| !auth.read_only && auth.scope.is_none())
    else {
        return message(
            StatusCode::FORBIDDEN,
            "This token may not register devices.",
        );
    };
    let Some(body) = body.as_object() else {
        return message(StatusCode::BAD_REQUEST, "Expected a JSON object.");
    };
    let fields = match registration_fields(body) {
        Ok(f) => f,
        Err(e) => return message(StatusCode::BAD_REQUEST, &e),
    };
    let webhook_id = random_hex(16);
    match app
        .core
        .store()
        .registration_create(user_id, auth.refresh_id, &sha256_hex(&webhook_id), &fields)
        .await
    {
        Ok(_) => {}
        Err(RegistrationError::TooMany) => {
            return message(
                StatusCode::TOO_MANY_REQUESTS,
                "Too many registered devices; remove some first.",
            );
        }
        Err(RegistrationError::Failed) => {
            return message(StatusCode::INTERNAL_SERVER_ERROR, "Could not register.");
        }
    }
    tracing::info!(
        target: "audit", user_id, device = %fields.device_name, app = %fields.app_id,
        "app device registered"
    );
    (
        StatusCode::CREATED,
        Json(json!({
            "webhook_id": webhook_id,
            "secret": null,
            "cloudhook_url": null,
            "remote_ui_url": null,
        })),
    )
        .into_response()
}

fn ok(value: Value) -> Response {
    Json(value).into_response()
}

/// What `get_config` tells the app. domus has no location of its own, so those are zeros.
pub(crate) fn config() -> Value {
    json!({
        "latitude": 0.0,
        "longitude": 0.0,
        "elevation": 0,
        "unit_system": {
            "length": "km",
            "mass": "g",
            "pressure": "Pa",
            "temperature": "°C",
            "volume": "L",
            "wind_speed": "m/s",
            "accumulated_precipitation": "mm",
        },
        "location_name": "domus",
        "time_zone": "UTC",
        "components": ["api", "mobile_app", "light", "scene"],
        "version": HA_VERSION,
    })
}

fn registration_json(f: &RegistrationFields) -> Value {
    json!({
        "app_id": f.app_id,
        "app_name": f.app_name,
        "app_version": f.app_version,
        "device_id": f.device_id,
        "device_name": f.device_name,
        "manufacturer": f.manufacturer,
        "model": f.model,
        "os_name": f.os_name,
        "os_version": f.os_version,
        "supports_encryption": false,
    })
}

/// `POST /api/webhook/{id}`: `{"type": ..., "data": ...}` from the app. Unknown ids get 410, which
/// tells the app its registration is gone; the id has 128 bits so this is no oracle.
async fn webhook(
    State(app): State<AppState>,
    Path(webhook_id): Path<String>,
    body: Bytes,
) -> Response {
    let store = app.core.store();
    let Some(reg) = store
        .registration_by_webhook(&sha256_hex(&webhook_id))
        .await
    else {
        return StatusCode::GONE.into_response();
    };
    let Ok(Value::Object(body)) = serde_json::from_slice::<Value>(&body) else {
        return message(StatusCode::BAD_REQUEST, "Expected a JSON object.");
    };
    let Some(kind) = body.get("type").and_then(Value::as_str) else {
        return message(StatusCode::BAD_REQUEST, "Missing type.");
    };
    let data = body.get("data").cloned().unwrap_or(Value::Null);
    match kind {
        "get_config" => ok(config()),
        "update_registration" => {
            let Some(data) = data.as_object().filter(|d| !d.is_empty()) else {
                return message(StatusCode::BAD_REQUEST, "Nothing to update.");
            };
            let mut fields = reg.fields.clone();
            let set = |key: &str, slot: &mut String| {
                if let Ok(Some(v)) = optional_text(data, key) {
                    *slot = v;
                }
            };
            set("app_version", &mut fields.app_version);
            set("device_name", &mut fields.device_name);
            set("manufacturer", &mut fields.manufacturer);
            set("model", &mut fields.model);
            if let Ok(Some(v)) = optional_text(data, "os_version") {
                fields.os_version = Some(v);
            }
            store.registration_update(reg.id, &fields).await;
            ok(registration_json(&fields))
        }
        "register_sensor" => (StatusCode::CREATED, Json(json!({"success": true}))).into_response(),
        "update_sensor_states" => {
            let mut results = Map::new();
            for sensor in data.as_array().into_iter().flatten() {
                if let Some(id) = sensor.get("unique_id").and_then(Value::as_str) {
                    results.insert(id.to_string(), json!({"success": true}));
                }
            }
            ok(Value::Object(results))
        }
        "update_location" => StatusCode::NO_CONTENT.into_response(),
        "get_zones" => ok(json!([])),
        // Accepted and dropped: nothing in domus consumes them yet.
        _ => ok(json!({})),
    }
}

pub fn router(app: AppState) -> Router {
    let registrations = Router::new()
        .route("/api/mobile_app/registrations", post(register))
        .layer(middleware::from_fn_with_state(app.clone(), require_token));
    Router::new()
        .merge(registrations)
        .route("/api/webhook/{id}", post(webhook))
        .with_state(app)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::Core;
    use crate::hue::HueManager;
    use crate::store::Store;
    use axum::body::Body;
    use axum::http::Request;
    use http_body_util::BodyExt;
    use std::sync::Arc;
    use tower::ServiceExt;

    const TOKEN: &str = "app-token";

    async fn setup() -> (Router, AppState) {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let store = core.store();
        let user = store.create_first_admin("admin", "x").await.unwrap();
        store
            .create_token(user, "t", &sha256_hex(TOKEN), None, false, None)
            .await
            .unwrap();
        store
            .create_token(user, "ro", &sha256_hex("ro-token"), None, true, None)
            .await
            .unwrap();
        let app = AppState::new(core.clone(), HueManager::new(core));
        (crate::build_app(app.clone()), app)
    }

    async fn post_json(
        r: &Router,
        uri: &str,
        token: Option<&str>,
        body: Value,
    ) -> (StatusCode, Value) {
        let mut b = Request::post(uri).header("content-type", "application/json");
        if let Some(t) = token {
            b = b.header("authorization", format!("Bearer {t}"));
        }
        let resp = r
            .clone()
            .oneshot(b.body(Body::from(body.to_string())).unwrap())
            .await
            .unwrap();
        let status = resp.status();
        let bytes = resp.into_body().collect().await.unwrap().to_bytes();
        (
            status,
            serde_json::from_slice(&bytes).unwrap_or(Value::Null),
        )
    }

    fn registration() -> Value {
        json!({
            "app_id": "io.homeassistant.companion.android",
            "app_name": "Home Assistant",
            "app_version": "2025.10.0",
            "device_name": "Pixel",
            "manufacturer": "Google",
            "model": "Pixel 8",
            "os_name": "Android",
            "os_version": "15",
            "supports_encryption": true,
            "app_data": {"push_token": "abc"},
        })
    }

    async fn registered(r: &Router) -> String {
        let (status, body) = post_json(
            r,
            "/api/mobile_app/registrations",
            Some(TOKEN),
            registration(),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
        body["webhook_id"].as_str().unwrap().to_string()
    }

    #[tokio::test]
    async fn registration_needs_a_writable_token_and_required_fields() {
        let (r, app) = setup().await;
        let uri = "/api/mobile_app/registrations";
        assert_eq!(
            post_json(&r, uri, None, registration()).await.0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            post_json(&r, uri, Some("ro-token"), registration()).await.0,
            StatusCode::FORBIDDEN
        );
        let mut missing = registration();
        missing.as_object_mut().unwrap().remove("model");
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), missing).await.0,
            StatusCode::BAD_REQUEST
        );
        assert!(app.core.store().list_registrations(None).await.is_empty());

        let (status, body) = post_json(&r, uri, Some(TOKEN), registration()).await;
        assert_eq!(status, StatusCode::CREATED);
        assert!(body["webhook_id"].as_str().unwrap().len() >= 32);
        assert_eq!(
            body.get("secret"),
            Some(&Value::Null),
            "key present but null: webhooks stay plain"
        );
        let list = app.core.store().list_registrations(None).await;
        assert_eq!(list.len(), 1);
        assert_eq!(list[0].fields.device_name, "Pixel");
        assert_eq!(list[0].username.as_deref(), Some("admin"));
    }

    #[tokio::test]
    async fn webhook_serves_what_the_apps_ask_for() {
        let (r, _) = setup().await;
        let id = registered(&r).await;
        let uri = format!("/api/webhook/{id}");

        let (status, cfg) = post_json(&r, &uri, None, json!({"type": "get_config"})).await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(cfg["version"], HA_VERSION);
        assert!(cfg["unit_system"].is_object());
        assert!(
            cfg["components"]
                .as_array()
                .unwrap()
                .contains(&json!("mobile_app"))
        );

        let (status, body) = post_json(
            &r,
            &uri,
            None,
            json!({"type": "register_sensor", "data": {"unique_id": "battery", "state": 80}}),
        )
        .await;
        assert_eq!(
            (status, body["success"].clone()),
            (StatusCode::CREATED, json!(true))
        );

        let (status, body) = post_json(
            &r,
            &uri,
            None,
            json!({"type": "update_sensor_states", "data": [{"unique_id": "a"}, {"unique_id": "b"}]}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(
            body,
            json!({"a": {"success": true}, "b": {"success": true}})
        );

        assert_eq!(
            post_json(
                &r,
                &uri,
                None,
                json!({"type": "update_location", "data": {}})
            )
            .await
            .0,
            StatusCode::NO_CONTENT
        );
        assert_eq!(
            post_json(&r, &uri, None, json!({"type": "get_zones"}))
                .await
                .1,
            json!([])
        );
        // Unknown types are accepted so a newer app does not break.
        assert_eq!(
            post_json(&r, &uri, None, json!({"type": "something_new"})).await,
            (StatusCode::OK, json!({}))
        );
        assert_eq!(
            post_json(&r, &uri, None, json!({"data": 1})).await.0,
            StatusCode::BAD_REQUEST
        );
    }

    #[tokio::test]
    async fn update_registration_changes_the_device_and_rejects_empty_data() {
        let (r, app) = setup().await;
        let id = registered(&r).await;
        let uri = format!("/api/webhook/{id}");
        assert_eq!(
            post_json(
                &r,
                &uri,
                None,
                json!({"type": "update_registration", "data": {}})
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );
        let (status, body) = post_json(
            &r,
            &uri,
            None,
            json!({"type": "update_registration", "data": {"device_name": "Pixel Pro", "app_version": "2025.11.0"}}),
        )
        .await;
        assert_eq!(status, StatusCode::OK);
        assert_eq!(body["device_name"], "Pixel Pro");
        let reg = &app.core.store().list_registrations(None).await[0];
        assert_eq!(reg.fields.device_name, "Pixel Pro");
        assert_eq!(reg.fields.app_version, "2025.11.0");
        assert_eq!(reg.fields.model, "Pixel 8");
    }

    #[tokio::test]
    async fn unknown_and_removed_webhooks_are_gone() {
        let (r, app) = setup().await;
        assert_eq!(
            post_json(&r, "/api/webhook/nope", None, json!({"type": "get_config"}))
                .await
                .0,
            StatusCode::GONE
        );
        let id = registered(&r).await;
        let reg_id = app.core.store().list_registrations(None).await[0].id;
        assert!(app.core.store().delete_registration(reg_id, None).await);
        assert_eq!(
            post_json(
                &r,
                &format!("/api/webhook/{id}"),
                None,
                json!({"type": "get_config"})
            )
            .await
            .0,
            StatusCode::GONE
        );
    }

    #[tokio::test]
    async fn only_the_owner_may_remove_a_registration() {
        let (r, app) = setup().await;
        registered(&r).await;
        let store = app.core.store();
        let reg = store.list_registrations(None).await[0].id;
        let other = store.create_user("bob", "x", false).await.unwrap();
        assert!(store.list_registrations(Some(other)).await.is_empty());
        assert!(!store.delete_registration(reg, Some(other)).await);
        assert!(store.delete_registration(reg, Some(1)).await);
    }

    /// An app login: a refresh token plus one access token minted from it.
    async fn app_login(app: &AppState, access: &str) -> i64 {
        let store = app.core.store();
        let refresh = store
            .refresh_create(1, "https://home-assistant.io/iOS", &sha256_hex("refresh"))
            .await
            .unwrap();
        assert!(
            store
                .create_app_token(1, "app", &sha256_hex(access), i64::MAX, refresh)
                .await
        );
        refresh
    }

    #[tokio::test]
    async fn malformed_requests_are_rejected() {
        let (r, app) = setup().await;
        let uri = "/api/mobile_app/registrations";
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), json!([1])).await.0,
            StatusCode::BAD_REQUEST
        );
        let mut nested = registration();
        nested["model"] = json!({"a": 1});
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), nested).await.0,
            StatusCode::BAD_REQUEST
        );
        let mut long = registration();
        long["device_name"] = json!("x".repeat(FIELD_MAX + 1));
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), long).await.0,
            StatusCode::BAD_REQUEST
        );
        assert!(app.core.store().list_registrations(None).await.is_empty());

        let id = registered(&r).await;
        let hook = format!("/api/webhook/{id}");
        assert_eq!(
            post_json(&r, &hook, None, json!([1])).await.0,
            StatusCode::BAD_REQUEST
        );
        let resp = r
            .clone()
            .oneshot(Request::post(&hook).body(Body::from("not json")).unwrap())
            .await
            .unwrap();
        assert_eq!(resp.status(), StatusCode::BAD_REQUEST);
        let (status, body) = post_json(
            &r,
            &hook,
            None,
            json!({"type": "update_registration", "data": {"os_version": "16"}}),
        )
        .await;
        assert_eq!(
            (status, body["os_version"].clone()),
            (StatusCode::OK, json!("16"))
        );
    }

    #[tokio::test]
    async fn scoped_tokens_may_not_register() {
        let (r, app) = setup().await;
        let scope = ["Garmin".to_string()];
        app.core
            .store()
            .create_token(
                1,
                "scoped",
                &sha256_hex("scoped"),
                Some(&scope),
                false,
                None,
            )
            .await
            .unwrap();
        let (status, _) = post_json(
            &r,
            "/api/mobile_app/registrations",
            Some("scoped"),
            registration(),
        )
        .await;
        assert_eq!(status, StatusCode::FORBIDDEN);
    }

    #[tokio::test]
    async fn required_fields_accept_empty_and_numbers() {
        let (r, _) = setup().await;
        let mut body = registration();
        body["os_version"] = json!(18);
        body["manufacturer"] = json!("");
        let (status, _) = post_json(&r, "/api/mobile_app/registrations", Some(TOKEN), body).await;
        assert_eq!(status, StatusCode::CREATED);
    }

    #[tokio::test]
    async fn registering_the_same_device_again_replaces_it() {
        let (r, app) = setup().await;
        let mut body = registration();
        body["device_id"] = json!("ABC");
        let uri = "/api/mobile_app/registrations";
        let (_, first) = post_json(&r, uri, Some(TOKEN), body.clone()).await;
        let (status, second) = post_json(&r, uri, Some(TOKEN), body).await;
        assert_eq!(status, StatusCode::CREATED);
        assert_ne!(first["webhook_id"], second["webhook_id"]);
        assert_eq!(app.core.store().list_registrations(None).await.len(), 1);
        let old = format!("/api/webhook/{}", first["webhook_id"].as_str().unwrap());
        assert_eq!(
            post_json(&r, &old, None, json!({"type": "get_config"}))
                .await
                .0,
            StatusCode::GONE
        );
    }

    #[tokio::test]
    async fn registrations_per_user_are_capped() {
        let (r, _) = setup().await;
        let uri = "/api/mobile_app/registrations";
        for i in 0..20 {
            let mut body = registration();
            body["device_id"] = json!(format!("dev-{i}"));
            assert_eq!(
                post_json(&r, uri, Some(TOKEN), body).await.0,
                StatusCode::CREATED
            );
        }
        let mut body = registration();
        body["device_id"] = json!("one-too-many");
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), body).await.0,
            StatusCode::TOO_MANY_REQUESTS
        );
        // Re-registering a known device is not "one more".
        let mut body = registration();
        body["device_id"] = json!("dev-3");
        assert_eq!(
            post_json(&r, uri, Some(TOKEN), body).await.0,
            StatusCode::CREATED
        );
    }

    #[tokio::test]
    async fn removing_a_registration_logs_the_app_out() {
        let (r, app) = setup().await;
        let store = app.core.store();
        let refresh = app_login(&app, "app-access").await;
        let (status, _) = post_json(
            &r,
            "/api/mobile_app/registrations",
            Some("app-access"),
            registration(),
        )
        .await;
        assert_eq!(status, StatusCode::CREATED);
        let reg = store.list_registrations(None).await[0].id;
        assert!(store.delete_registration(reg, Some(1)).await);
        assert!(store.list_registrations(None).await.is_empty());
        assert!(
            store
                .refresh_lookup(&sha256_hex("refresh"), "https://home-assistant.io/iOS")
                .await
                .is_none()
        );
        assert_eq!(
            post_json(
                &r,
                "/api/mobile_app/registrations",
                Some("app-access"),
                registration()
            )
            .await
            .0,
            StatusCode::UNAUTHORIZED
        );
        let _ = refresh;
    }

    #[tokio::test]
    async fn logging_the_app_out_removes_its_registration() {
        let (r, app) = setup().await;
        let store = app.core.store();
        app_login(&app, "app-access").await;
        let (_, body) = post_json(
            &r,
            "/api/mobile_app/registrations",
            Some("app-access"),
            registration(),
        )
        .await;
        let id = body["webhook_id"].as_str().unwrap().to_string();
        store.delete_user_refresh_tokens(1).await;
        assert!(store.list_registrations(None).await.is_empty());
        assert_eq!(
            post_json(
                &r,
                &format!("/api/webhook/{id}"),
                None,
                json!({"type": "get_config"})
            )
            .await
            .0,
            StatusCode::GONE
        );
    }

    #[tokio::test]
    async fn deleting_a_user_removes_their_registrations() {
        let (r, app) = setup().await;
        let id = registered(&r).await;
        assert!(app.core.store().delete_user(1).await);
        assert_eq!(
            post_json(
                &r,
                &format!("/api/webhook/{id}"),
                None,
                json!({"type": "get_config"})
            )
            .await
            .0,
            StatusCode::GONE
        );
    }
}
