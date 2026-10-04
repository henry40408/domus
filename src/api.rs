//! Home Assistant compatible REST subset (what hasscontrol needs).

use axum::Extension;
use axum::body::Bytes;
use axum::extract::{Path, Request, State};
use axum::http::{StatusCode, header};
use axum::middleware::{self, Next};
use axum::response::{IntoResponse, Response};
use axum::routing::{get, post};
use axum::{Json, Router};
use serde_json::{Value, json};
use std::sync::{Arc, Mutex};

use crate::core::{CallError, Core, LightAction, TurnOn};
use crate::hue::HueManager;
use crate::store::TokenAuth;
use crate::throttle::LoginGuard;
use crate::util::sha256_hex;

#[derive(Clone)]
pub struct AppState {
    pub core: Arc<Core>,
    pub hue: Arc<HueManager>,
    pub guard: Arc<LoginGuard>,
    /// Required by `/setup` while set; cleared once the first admin exists.
    pub setup_code: Arc<Mutex<Option<String>>>,
    /// Flips to `true` on shutdown so long-lived responses (SSE) end instead of stalling the drain.
    pub shutdown: Arc<tokio::sync::watch::Sender<bool>>,
}

impl AppState {
    pub fn new(core: Arc<Core>, hue: Arc<HueManager>) -> Self {
        crate::util::warm_dummy();
        Self {
            core,
            hue,
            guard: Arc::default(),
            setup_code: Arc::default(),
            shutdown: Arc::new(tokio::sync::watch::channel(false).0),
        }
    }

    pub fn with_setup_code(self, code: Option<String>) -> Self {
        *self.setup_code.lock().unwrap() = code;
        self
    }
}

fn message(status: StatusCode, msg: &str) -> Response {
    (status, Json(json!({ "message": msg }))).into_response()
}

fn bearer(req: &Request) -> Option<&str> {
    let v = req.headers().get(header::AUTHORIZATION)?.to_str().ok()?;
    let (scheme, token) = v.split_once(' ')?;
    scheme.eq_ignore_ascii_case("bearer").then(|| token.trim())
}

async fn require_token(State(app): State<AppState>, mut req: Request, next: Next) -> Response {
    let hash = bearer(&req).map(sha256_hex);
    let auth = match hash {
        Some(h) => app.core.store().token_auth(&h).await,
        None => None,
    };
    match auth {
        Some(auth) => {
            req.extensions_mut().insert(auth);
            next.run(req).await
        }
        None => (StatusCode::UNAUTHORIZED, "401: Unauthorized").into_response(),
    }
}

async fn get_state(
    State(app): State<AppState>,
    Extension(auth): Extension<TokenAuth>,
    Path(entity_id): Path<String>,
) -> Response {
    if !app.core.allowed(&auth.scope, &entity_id).await {
        return message(StatusCode::NOT_FOUND, "Entity not found.");
    }
    match app.core.lookup(&entity_id).await {
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

/// Hue's supported color temperature range, in mireds (6535 K to 2000 K).
const MIREK_RANGE: std::ops::RangeInclusive<f64> = 153.0..=500.0;
/// Longest fade accepted, so a typo cannot park a light for hours.
const MAX_TRANSITION_MS: f64 = 3_600_000.0;

fn number_pair(v: &Value) -> Option<(f64, f64)> {
    match v.as_array()?.as_slice() {
        [a, b] => Some((a.as_f64()?, b.as_f64()?)),
        _ => None,
    }
}

/// sRGB (0-255 each) to CIE xy, using the wide-gamut matrix Hue documents.
fn rgb_to_xy(r: f64, g: f64, b: f64) -> (f64, f64) {
    let lin = |c: f64| {
        let c = (c / 255.0).clamp(0.0, 1.0);
        if c > 0.04045 {
            ((c + 0.055) / 1.055).powf(2.4)
        } else {
            c / 12.92
        }
    };
    let (r, g, b) = (lin(r), lin(g), lin(b));
    let x = r * 0.664511 + g * 0.154324 + b * 0.162028;
    let y = r * 0.283881 + g * 0.668433 + b * 0.047685;
    let z = r * 0.000088 + g * 0.072310 + b * 0.986039;
    let sum = x + y + z;
    if sum == 0.0 {
        return (0.3127, 0.3290); // black has no chromaticity: fall back to the white point
    }
    let round = |v: f64| (v / sum * 10_000.0).round() / 10_000.0;
    (round(x), round(y))
}

/// HSV with full value to sRGB; Home Assistant's `hs_color` is hue 0-360 and saturation 0-100.
fn hs_to_rgb(h: f64, s: f64) -> (f64, f64, f64) {
    let (s, h) = ((s / 100.0).clamp(0.0, 1.0), h.rem_euclid(360.0) / 60.0);
    let c = s;
    let x = c * (1.0 - (h % 2.0 - 1.0).abs());
    let m = 1.0 - c;
    let (r, g, b) = match h as u8 {
        0 => (c, x, 0.0),
        1 => (x, c, 0.0),
        2 => (0.0, c, x),
        3 => (0.0, x, c),
        4 => (x, 0.0, c),
        _ => (c, 0.0, x),
    };
    ((r + m) * 255.0, (g + m) * 255.0, (b + m) * 255.0)
}

/// Fade time in ms from `transition` (seconds, like Home Assistant).
fn requested_transition(body: &Value) -> Result<Option<u32>, &'static str> {
    match body.get("transition") {
        None | Some(Value::Null) => Ok(None),
        Some(v) => match v.as_f64() {
            Some(t) if t >= 0.0 => Ok(Some((t * 1000.0).min(MAX_TRANSITION_MS).round() as u32)),
            _ => Err("transition must be a non-negative number of seconds."),
        },
    }
}

/// Mireds or xy, whichever the request names.
type Color = (Option<u16>, Option<(f64, f64)>);

/// The color a `turn_on` asks for, as mireds or xy. Home Assistant accepts several spellings;
/// the first one present wins.
fn requested_color(body: &Value) -> Result<Color, &'static str> {
    let mirek = |m: f64| Some(m.clamp(*MIREK_RANGE.start(), *MIREK_RANGE.end()).round() as u16);
    if let Some(v) = body.get("color_temp") {
        let m = v
            .as_f64()
            .filter(|m| *m > 0.0)
            .ok_or("color_temp must be mireds.")?;
        return Ok((mirek(m), None));
    }
    for key in ["color_temp_kelvin", "kelvin"] {
        if let Some(v) = body.get(key) {
            let k = v
                .as_f64()
                .filter(|k| *k > 0.0)
                .ok_or("Kelvin must be a positive number.")?;
            return Ok((mirek(1_000_000.0 / k), None));
        }
    }
    if let Some(v) = body.get("xy_color") {
        let (x, y) = number_pair(v)
            .filter(|(x, y)| (0.0..=1.0).contains(x) && (0.0..=1.0).contains(y))
            .ok_or("xy_color must be [x, y] between 0 and 1.")?;
        return Ok((None, Some((x, y))));
    }
    if let Some(v) = body.get("hs_color") {
        let (h, s) = number_pair(v).ok_or("hs_color must be [hue, saturation].")?;
        let (r, g, b) = hs_to_rgb(h, s);
        return Ok((None, Some(rgb_to_xy(r, g, b))));
    }
    if let Some(v) = body.get("rgb_color") {
        let ok = v.as_array().filter(|a| {
            a.len() == 3
                && a.iter()
                    .all(|c| c.as_f64().is_some_and(|c| (0.0..=255.0).contains(&c)))
        });
        let a = ok.ok_or("rgb_color must be [r, g, b] between 0 and 255.")?;
        let c: Vec<f64> = a.iter().filter_map(Value::as_f64).collect();
        return Ok((None, Some(rgb_to_xy(c[0], c[1], c[2]))));
    }
    Ok((None, None))
}

/// Empty bodies count as `null`; anything else must be valid JSON.
fn parse_body(body: &Bytes) -> Result<Value, &'static str> {
    if body.is_empty() {
        return Ok(Value::Null);
    }
    serde_json::from_slice(body).map_err(|_| "Invalid JSON.")
}

async fn scene_service(
    State(app): State<AppState>,
    Extension(auth): Extension<TokenAuth>,
    Path(service): Path<String>,
    body: Bytes,
) -> Response {
    if auth.read_only {
        return message(StatusCode::FORBIDDEN, "This token is read-only.");
    }
    let body = match parse_body(&body) {
        Ok(b) => b,
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };
    if service != "turn_on" {
        return message(StatusCode::NOT_FOUND, "Service not found.");
    }
    let ids = match requested_entities(&body) {
        Ok(ids) => ids,
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };
    let mut changed = Vec::new();
    for id in ids.iter().filter(|i| i.starts_with("scene.")) {
        // Out-of-scope entities are ignored, like unknown ones.
        if !app.core.allowed(&auth.scope, id).await {
            continue;
        }
        match app.core.activate_scene(id).await {
            Ok(()) => changed.extend(app.core.lookup(id).await),
            Err(CallError::NoIntegration) => {}
            Err(CallError::Failed(e)) => {
                tracing::warn!("scene call for {id} failed: {e}");
                return message(StatusCode::BAD_GATEWAY, &format!("Call to {id} failed."));
            }
        }
    }
    Json(changed).into_response()
}

/// `Ok(None)` for a service that does not exist.
pub(crate) fn light_action(
    service: &str,
    body: &Value,
) -> Result<Option<LightAction>, &'static str> {
    let transition_ms = requested_transition(body)?;
    Ok(match service {
        "turn_on" => {
            let (color_temp, xy) = requested_color(body)?;
            Some(LightAction::TurnOn(TurnOn {
                brightness: requested_brightness(body),
                color_temp,
                xy,
                transition_ms,
            }))
        }
        "turn_off" => Some(LightAction::TurnOff { transition_ms }),
        _ => None,
    })
}

async fn light_service(
    State(app): State<AppState>,
    Extension(auth): Extension<TokenAuth>,
    Path(service): Path<String>,
    body: Bytes,
) -> Response {
    if auth.read_only {
        return message(StatusCode::FORBIDDEN, "This token is read-only.");
    }
    let body = match parse_body(&body) {
        Ok(b) => b,
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };
    let action = match light_action(&service, &body) {
        Ok(Some(a)) => a,
        Ok(None) => return message(StatusCode::NOT_FOUND, "Service not found."),
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };
    let ids = match requested_entities(&body) {
        Ok(ids) => app.core.expand_entities(&ids).await,
        Err(e) => return message(StatusCode::BAD_REQUEST, e),
    };

    let mut changed = Vec::new();
    // A group may also hold scenes; those are not lights.
    for id in ids.iter().filter(|i| !i.starts_with("scene.")) {
        if !app.core.allowed(&auth.scope, id).await {
            continue;
        }
        match app.core.call_light(id, &action).await {
            Ok(()) => {
                if let Some(s) = app.core.lookup(id).await {
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
        .route("/api/services/scene/{service}", post(scene_service))
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
            id.starts_with("light.hue_") || id.starts_with("scene.hue_")
        }
        fn activate_scene<'a>(&'a self, id: &'a str) -> BoxFut<'a, Result<(), String>> {
            Box::pin(async move {
                if id.ends_with("bad") {
                    Err("boom".into())
                } else {
                    Ok(())
                }
            })
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

    async fn setup() -> (Router, AppState, Arc<Fake>) {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        core.store()
            .create_token(1, "t", &sha256_hex(TOKEN), None, false, None)
            .await;
        let fake = Arc::new(Fake(Mutex::new(Vec::new())));
        core.set_integration("fake", fake.clone());
        core.set_state("light.hue_a", "off", Map::new());
        core.set_state("light.hue_b", "off", Map::new());
        core.store()
            .group_set("Garmin", &["light.hue_a".into(), "light.hue_b".into()])
            .await
            .unwrap();
        let app = AppState::new(core.clone(), HueManager::new(core));
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
        let (r, ..) = setup().await;
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
        let (r, ..) = setup().await;
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
        let (r, ..) = setup().await;
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
    async fn read_only_tokens_read_but_never_control() {
        let (r, app, fake) = setup().await;
        let store = app.core.store();
        store
            .create_token(1, "ro", &sha256_hex("ro"), None, true, None)
            .await;
        let t = Some("ro");
        assert_eq!(
            call(&r, "GET", "/api/states/light.hue_a", t, "").await.0,
            StatusCode::OK
        );
        let body = r#"{"entity_id":"light.hue_a"}"#;
        for uri in [
            "/api/services/light/turn_on",
            "/api/services/light/turn_off",
            "/api/services/scene/turn_on",
        ] {
            assert_eq!(
                call(&r, "POST", uri, t, body).await.0,
                StatusCode::FORBIDDEN,
                "{uri}"
            );
        }
        assert!(fake.0.lock().unwrap().is_empty());
    }

    #[tokio::test]
    async fn owner_group_limit_narrows_existing_tokens() {
        let (r, app, _) = setup().await;
        let store = app.core.store();
        let owner = store.create_user("owner", "h", false).await.unwrap();
        let token = "owned-token";
        store
            .create_token(owner, "o", &sha256_hex(token), None, false, None)
            .await;
        store
            .group_set("Other", &["light.hue_c".into()])
            .await
            .unwrap();
        app.core.set_state("light.hue_c", "off", Map::new());
        let get = |id: &str| {
            let (r, uri) = (r.clone(), format!("/api/states/{id}"));
            async move { call(&r, "GET", &uri, Some(token), "").await.0 }
        };
        assert_eq!(get("light.hue_c").await, StatusCode::OK);
        // the token itself is unrestricted, but its owner is limited to Garmin
        store
            .set_user_scope(owner, Some(&["Garmin".to_string()]))
            .await;
        assert_eq!(get("light.hue_a").await, StatusCode::OK);
        assert_eq!(get("light.hue_c").await, StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn turn_on_string_entity_returns_state_array() {
        let (r, app, fake) = setup().await;
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
            ("light.hue_a".to_string(), LightAction::on())
        );
    }

    #[tokio::test]
    async fn target_array_group_and_brightness() {
        let (r, _, fake) = setup().await;
        let body = r#"{"target":{"entity_id":["group.Garmin"]},"brightness_pct":50}"#;
        let (s, v) = call(&r, "POST", "/api/services/light/turn_on", Some(TOKEN), body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v.as_array().unwrap().len(), 2);
        assert_eq!(
            fake.0.lock().unwrap()[1].1,
            LightAction::TurnOn(TurnOn {
                brightness: Some(128),
                ..TurnOn::default()
            })
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

    #[test]
    fn color_and_transition_fields_follow_home_assistant() {
        let action = |service: &str, body: Value| light_action(service, &body);
        let on = |p: TurnOn| Ok(Some(LightAction::TurnOn(p)));

        assert_eq!(
            action("turn_on", json!({"color_temp": 300, "transition": 1.5})),
            on(TurnOn {
                color_temp: Some(300),
                transition_ms: Some(1500),
                ..TurnOn::default()
            })
        );
        // kelvin converts to mireds and is clamped to what Hue can do
        assert_eq!(
            action("turn_on", json!({"color_temp_kelvin": 2700})),
            on(TurnOn {
                color_temp: Some(370),
                ..TurnOn::default()
            })
        );
        assert_eq!(
            action("turn_on", json!({"color_temp": 10})),
            on(TurnOn {
                color_temp: Some(153),
                ..TurnOn::default()
            })
        );
        assert_eq!(
            action("turn_off", json!({"transition": 3})),
            Ok(Some(LightAction::TurnOff {
                transition_ms: Some(3000)
            }))
        );
        // red in sRGB lands near the red corner of the gamut
        let Ok(Some(LightAction::TurnOn(TurnOn {
            xy: Some((x, y)), ..
        }))) = action("turn_on", json!({"rgb_color": [255, 0, 0]}))
        else {
            panic!("rgb_color should give xy");
        };
        assert!((x - 0.7).abs() < 0.03 && (y - 0.3).abs() < 0.03, "{x} {y}");
        let Ok(Some(LightAction::TurnOn(TurnOn { xy: Some(hs), .. }))) =
            action("turn_on", json!({"hs_color": [0, 100]}))
        else {
            panic!("hs_color should give xy");
        };
        assert_eq!(hs, (x, y), "hs red and rgb red are the same color");

        for bad in [
            json!({"transition": -1}),
            json!({"transition": "x"}),
            json!({"color_temp": 0}),
            json!({"xy_color": [0.5]}),
            json!({"xy_color": [2, 0]}),
            json!({"rgb_color": [1, 2]}),
            json!({"hs_color": "red"}),
        ] {
            assert!(action("turn_on", bad.clone()).is_err(), "{bad}");
        }
        assert_eq!(action("toggle", json!({})), Ok(None));
    }

    #[tokio::test]
    async fn service_edge_cases() {
        let (r, ..) = setup().await;
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
    async fn scene_turn_on_stamps_state_and_light_calls_skip_scenes() {
        let (r, app, fake) = setup().await;
        app.core.set_state("scene.hue_a", "unknown", Map::new());
        app.core.set_state("scene.hue_bad", "unknown", Map::new());

        let (_, v) = call(&r, "GET", "/api/states/scene.hue_a", Some(TOKEN), "").await;
        assert_eq!(v["state"], "unknown");

        let body = r#"{"entity_id":"scene.hue_a"}"#;
        let (s, v) = call(&r, "POST", "/api/services/scene/turn_on", Some(TOKEN), body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v[0]["entity_id"], "scene.hue_a");
        assert!(v[0]["state"].as_str().unwrap().contains('T'));
        let (_, v) = call(&r, "GET", "/api/states/scene.hue_a", Some(TOKEN), "").await;
        assert!(v["state"].as_str().unwrap().contains('T'));

        // unknown scenes are ignored, failures are 502, other services 404
        let ghost = r#"{"target":{"entity_id":["scene.ghost"]}}"#;
        let (s, v) = call(
            &r,
            "POST",
            "/api/services/scene/turn_on",
            Some(TOKEN),
            ghost,
        )
        .await;
        assert_eq!((s, v), (StatusCode::OK, json!([])));
        let bad = r#"{"entity_id":"scene.hue_bad"}"#;
        assert_eq!(
            call(&r, "POST", "/api/services/scene/turn_on", Some(TOKEN), bad)
                .await
                .0,
            StatusCode::BAD_GATEWAY
        );
        assert_eq!(
            call(&r, "POST", "/api/services/scene/create", Some(TOKEN), "{}")
                .await
                .0,
            StatusCode::NOT_FOUND
        );

        // a group holding a scene lists it, and light calls on the group leave it alone
        app.core
            .store()
            .group_set("Mixed", &["light.hue_a".into(), "scene.hue_a".into()])
            .await
            .unwrap();
        let (_, v) = call(&r, "GET", "/api/states/group.Mixed", Some(TOKEN), "").await;
        assert_eq!(
            v["attributes"]["entity_id"],
            json!(["light.hue_a", "scene.hue_a"])
        );
        let body = r#"{"entity_id":"group.Mixed"}"#;
        let (_, v) = call(&r, "POST", "/api/services/light/turn_on", Some(TOKEN), body).await;
        assert_eq!(v.as_array().unwrap().len(), 1);
        assert_eq!(fake.0.lock().unwrap().len(), 1);
    }

    #[tokio::test]
    async fn group_light_service_call_returns_its_state() {
        let (r, app, fake) = setup().await;
        app.core.store().group_set_exposed("Garmin", true).await;
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

    #[tokio::test]
    async fn scoped_token_only_reaches_its_groups() {
        let (r, app, fake) = setup().await;
        app.core.set_state("light.hue_c", "off", Map::new());
        app.core.set_state("scene.hue_a", "unknown", Map::new());
        let store = app.core.store();
        store
            .group_set("Other", &["light.hue_c".into(), "scene.hue_a".into()])
            .await
            .unwrap();
        store.group_set_exposed("Garmin", true).await;
        let scope = ["Garmin".to_string()];
        store
            .create_token(
                1,
                "scoped",
                &sha256_hex("scoped"),
                Some(&scope),
                false,
                None,
            )
            .await;
        let t = Some("scoped");

        for ok in [
            "group.Garmin",
            "light.hue_a",
            "light.hue_b",
            "light.domus_group_Garmin",
        ] {
            let uri = format!("/api/states/{ok}");
            assert_eq!(call(&r, "GET", &uri, t, "").await.0, StatusCode::OK, "{ok}");
        }
        for denied in ["group.Other", "light.hue_c", "scene.hue_a", "group.None"] {
            let uri = format!("/api/states/{denied}");
            assert_eq!(
                call(&r, "GET", &uri, t, "").await.0,
                StatusCode::NOT_FOUND,
                "{denied}"
            );
        }

        // out-of-scope entities are skipped, in-scope ones still run
        let body = r#"{"entity_id":["light.hue_a","light.hue_c","group.Other"]}"#;
        let (s, v) = call(&r, "POST", "/api/services/light/turn_on", t, body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v.as_array().unwrap().len(), 1);
        assert_eq!(v[0]["entity_id"], "light.hue_a");
        assert_eq!(fake.0.lock().unwrap().len(), 1);

        let body = r#"{"entity_id":"scene.hue_a"}"#;
        let (s, v) = call(&r, "POST", "/api/services/scene/turn_on", t, body).await;
        assert_eq!((s, v), (StatusCode::OK, json!([])));
        let (_, v) = call(&r, "GET", "/api/states/scene.hue_a", Some(TOKEN), "").await;
        assert_eq!(v["state"], "unknown");

        // the group's own light switch fans out to its members
        let body = r#"{"entity_id":"light.domus_group_Garmin"}"#;
        let (s, v) = call(&r, "POST", "/api/services/light/turn_off", t, body).await;
        assert_eq!(s, StatusCode::OK);
        assert_eq!(v[0]["entity_id"], "light.domus_group_Garmin");
        assert_eq!(fake.0.lock().unwrap().len(), 3);

        // an unrestricted token still reaches everything
        assert_eq!(
            call(&r, "GET", "/api/states/light.hue_c", Some(TOKEN), "")
                .await
                .0,
            StatusCode::OK
        );
    }
}
