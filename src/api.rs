//! Home Assistant compatible REST subset (what hasscontrol needs).

use axum::body::Bytes;
use axum::extract::{Path, Request, State};
use axum::http::{StatusCode, header};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde_json::{Value, json};
use std::sync::Arc;

use crate::core::{CallError, Core, LightAction};
use crate::hue::HueManager;
use crate::util::sha256_hex;

#[derive(Clone)]
pub struct AppState {
    pub core: Arc<Core>,
    pub hue: Arc<HueManager>,
}

fn message(status: StatusCode, msg: &str) -> Response {
    (status, Json(json!({ "message": msg }))).into_response()
}

fn bearer(req: &Request) -> Option<&str> {
    let v = req.headers().get(header::AUTHORIZATION)?.to_str().ok()?;
    let (scheme, token) = v.split_once(' ')?;
    scheme.eq_ignore_ascii_case("bearer").then(|| token.trim())
}

async fn require_token(State(app): State<AppState>, req: Request, next: Next) -> Response {
    let ok = bearer(&req).is_some_and(|t| app.core.store().token_valid(&sha256_hex(t)));
    if ok {
        next.run(req).await
    } else {
        (StatusCode::UNAUTHORIZED, "401: Unauthorized").into_response()
    }
}

async fn get_state(State(app): State<AppState>, Path(entity_id): Path<String>) -> Response {
    match app.core.lookup(&entity_id) {
        Some(s) => Json(s).into_response(),
        None => message(StatusCode::NOT_FOUND, "Entity not found."),
    }
}

/// Collects entity ids from `entity_id` (string or array) and `target.entity_id`.
fn requested_entities(body: &Value) -> Result<Vec<String>, &'static str> {
    let mut out = Vec::new();
    let sources = [body.get("entity_id"), body.pointer("/target/entity_id")];
    for v in sources.into_iter().flatten() {
        match v {
            Value::String(s) => out.extend(s.split(',').map(|p| p.trim().to_string())),
            Value::Array(a) => {
                for item in a {
                    out.push(
                        item.as_str()
                            .ok_or("entity_id must be a string")?
                            .to_string(),
                    );
                }
            }
            Value::Null => {}
            _ => return Err("entity_id must be a string or list of strings"),
        }
    }
    out.retain(|s| !s.is_empty());
    Ok(out)
}

fn requested_brightness(body: &Value) -> Option<u8> {
    if let Some(b) = body.get("brightness").and_then(Value::as_f64) {
        return Some(b.round().clamp(0.0, 255.0) as u8);
    }
    body.get("brightness_pct")
        .and_then(Value::as_f64)
        .map(|p| (p / 100.0 * 255.0).round().clamp(0.0, 255.0) as u8)
}

async fn light_service(
    State(app): State<AppState>,
    Path(service): Path<String>,
    body: Bytes,
) -> Response {
    let body: Value = if body.is_empty() {
        Value::Null
    } else {
        match serde_json::from_slice(&body) {
            Ok(v) => v,
            Err(_) => return message(StatusCode::BAD_REQUEST, "Invalid JSON."),
        }
    };
    let action = match service.as_str() {
        "turn_on" => LightAction::TurnOn {
            brightness: requested_brightness(&body),
        },
        "turn_off" => LightAction::TurnOff,
        _ => return message(StatusCode::NOT_FOUND, "Service not found."),
    };
    let ids = match requested_entities(&body) {
        Ok(ids) => app.core.expand_entities(&ids),
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };

    let mut changed = Vec::new();
    for id in &ids {
        match app.core.call_light(id, &action).await {
            Ok(()) => {
                if let Some(s) = app.core.lookup(id) {
                    changed.push(s);
                }
            }
            // HA ignores unknown entities in service calls.
            Err(CallError::NoIntegration) => {}
            Err(CallError::Failed(e)) => {
                tracing::warn!("service call for {id} failed: {e}");
                return message(StatusCode::BAD_GATEWAY, &format!("Call to {id} failed."));
            }
        }
    }
    Json(changed).into_response()
}

pub fn router(app: AppState) -> Router {
    Router::new()
        .route("/api/states/{entity_id}", get(get_state))
        .route("/api/services/light/{service}", post(light_service))
        .layer(middleware::from_fn_with_state(app.clone(), require_token))
        .with_state(app)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::{BoxFut, Integration};
    use crate::store::Store;
    use axum::body::Body;
    use axum::http::Request as HttpRequest;
    use http_body_util::BodyExt;
    use serde_json::Map;
    use std::sync::Mutex;
    use tower::ServiceExt;

    struct Fake(Mutex<Vec<(String, LightAction)>>);
    impl Integration for Fake {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.hue_")
        }
        fn call_light<'a>(
            &'a self,
            id: &'a str,
            a: &'a LightAction,
        ) -> BoxFut<'a, Result<(), String>> {
            Box::pin(async move {
                self.0.lock().unwrap().push((id.to_string(), a.clone()));
                Ok(())
            })
        }
    }

    const TOKEN: &str = "secret-token";

    fn setup() -> (Router, AppState, Arc<Fake>) {
        let core = Core::new(Arc::new(Store::open_memory().unwrap()));
        core.store().create_token("t", &sha256_hex(TOKEN));
        let fake = Arc::new(Fake(Mutex::new(Vec::new())));
        core.set_integration("fake", fake.clone());
        core.set_state("light.hue_a", "off", Map::new());
        core.set_state("light.hue_b", "off", Map::new());
        core.store()
            .group_set("Garmin", &["light.hue_a".into(), "light.hue_b".into()])
            .unwrap();
        let app = AppState {
            core: core.clone(),
            hue: HueManager::new(core),
        };
        (router(app.clone()), app, fake)
    }

    async fn call(
        r: &Router,
        method: &str,
        uri: &str,
        token: Option<&str>,
        body: &str,
    ) -> (StatusCode, Value) {
        let mut b = HttpRequest::builder()
            .method(method)
            .uri(uri)
            .header("content-type", "application/json");
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

    #[tokio::test]
    async fn auth_required() {
        let (r, ..) = setup();
        assert_eq!(
            call(&r, "GET", "/api/states/light.hue_a", None, "").await.0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            call(&r, "GET", "/api/states/light.hue_a", Some("wrong"), "")
                .await
                .0,
            StatusCode::UNAUTHORIZED
        );
        assert_eq!(
            call(&r, "GET", "/api/states/light.hue_a", Some(TOKEN), "")
                .await
                .0,
            StatusCode::OK
        );
    }

    #[tokio::test]
    async fn state_shape_and_404() {
        let (r, ..) = setup();
        let (_, v) = call(&r, "GET", "/api/states/light.hue_a", Some(TOKEN), "").await;
        assert_eq!(v["entity_id"], "light.hue_a");
        assert_eq!(v["state"], "off");
        assert!(v["context"]["id"].is_string());
        let (s, v) = call(&r, "GET", "/api/states/light.nope", Some(TOKEN), "").await;
        assert_eq!(s, StatusCode::NOT_FOUND);
        assert_eq!(v["message"], "Entity not found.");
    }

    #[tokio::test]
    async fn group_lists_members() {
        let (r, ..) = setup();
        let (s, v) = call(&r, "GET", "/api/states/group.Garmin", Some(TOKEN), "").await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v["entity_id"], "group.Garmin");
        assert_eq!(
            v["attributes"]["entity_id"],
            json!(["light.hue_a", "light.hue_b"])
        );
        assert_eq!(
            call(&r, "GET", "/api/states/group.None", Some(TOKEN), "")
                .await
                .0,
            StatusCode::NOT_FOUND
        );
    }

    #[tokio::test]
    async fn turn_on_string_entity_returns_state_array() {
        let (r, app, fake) = setup();
        let (s, v) = call(
            &r,
            "POST",
            "/api/services/light/turn_on",
            Some(TOKEN),
            r#"{"entity_id":"light.hue_a"}"#,
        )
        .await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v[0]["entity_id"], "light.hue_a");
        assert_eq!(v[0]["state"], "on");
        assert_eq!(app.core.get_state("light.hue_a").unwrap().state, "on");
        assert_eq!(
            fake.0.lock().unwrap()[0],
            (
                "light.hue_a".to_string(),
                LightAction::TurnOn { brightness: None }
            )
        );
    }

    #[tokio::test]
    async fn target_array_group_and_brightness() {
        let (r, _, fake) = setup();
        let body = r#"{"target":{"entity_id":["group.Garmin"]},"brightness_pct":50}"#;
        let (s, v) = call(&r, "POST", "/api/services/light/turn_on", Some(TOKEN), body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v.as_array().unwrap().len(), 2);
        assert_eq!(
            fake.0.lock().unwrap()[1].1,
            LightAction::TurnOn {
                brightness: Some(128)
            }
        );

        let (_, v) = call(
            &r,
            "POST",
            "/api/services/light/turn_off",
            Some(TOKEN),
            r#"{"entity_id":"group.Garmin"}"#,
        )
        .await;
        assert!(v.as_array().unwrap().iter().all(|s| s["state"] == "off"));
    }

    #[tokio::test]
    async fn service_edge_cases() {
        let (r, ..) = setup();
        // unknown entity: empty array, still JSON
        let (s, v) = call(
            &r,
            "POST",
            "/api/services/light/turn_on",
            Some(TOKEN),
            r#"{"entity_id":"light.ghost"}"#,
        )
        .await;
        assert_eq!((s, v), (StatusCode::OK, json!([])));
        // empty body
        let (s, v) = call(&r, "POST", "/api/services/light/turn_off", Some(TOKEN), "").await;
        assert_eq!((s, v), (StatusCode::OK, json!([])));
        // bad entity type
        assert_eq!(
            call(
                &r,
                "POST",
                "/api/services/light/turn_on",
                Some(TOKEN),
                r#"{"entity_id":5}"#
            )
            .await
            .0,
            StatusCode::BAD_REQUEST
        );
        // unknown service
        assert_eq!(
            call(&r, "POST", "/api/services/light/toggle", Some(TOKEN), "{}")
                .await
                .0,
            StatusCode::NOT_FOUND
        );
        // unauthenticated service call
        assert_eq!(
            call(&r, "POST", "/api/services/light/turn_on", None, "{}")
                .await
                .0,
            StatusCode::UNAUTHORIZED
        );
    }

    #[tokio::test]
    async fn group_light_service_call_returns_its_state() {
        let (r, app, fake) = setup();
        app.core.store().group_set_exposed("Garmin", true);
        let body = r#"{"entity_id":"light.domus_group_Garmin"}"#;
        let (s, v) = call(&r, "POST", "/api/services/light/turn_on", Some(TOKEN), body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v[0]["entity_id"], "light.domus_group_Garmin");
        assert_eq!(v[0]["state"], "on");
        assert_eq!(fake.0.lock().unwrap().len(), 2);
        let (s, v) = call(
            &r,
            "GET",
            "/api/states/light.domus_group_Garmin",
            Some(TOKEN),
            "",
        )
        .await;
        assert_eq!((s, v["state"].as_str()), (StatusCode::OK, Some("on")));
    }
}
