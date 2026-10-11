//! The Home Assistant WebSocket API (`/api/websocket`), the subset the Companion apps need:
//! authentication, the state snapshot and its updates, and enough registry commands for the
//! apps to start. Anything else answers `unknown_command`.

use std::collections::HashSet;
use std::time::Duration;

use axum::Router;
use axum::extract::State;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::response::Response;
use axum::routing::get;
use serde_json::{Map, Value, json};
use tokio::sync::broadcast::error::RecvError;

use crate::api::AppState;
use crate::core::State as EntityState;
use crate::mobile::{HA_VERSION, config};
use crate::store::{Scope, TokenAuth, User};
use crate::util::sha256_hex;

/// How long a client may take to send its `auth` message.
#[cfg(not(test))]
const AUTH_TIMEOUT: Duration = Duration::from_secs(10);
#[cfg(test)]
const AUTH_TIMEOUT: Duration = Duration::from_millis(300);

/// How often the token is checked for revocation.
#[cfg(not(test))]
const RECHECK: Duration = Duration::from_secs(30);
#[cfg(test)]
const RECHECK: Duration = Duration::from_millis(100);

/// Registries domus has nothing in; the apps ask for them and expect a list.
const EMPTY_LISTS: &[&str] = &[
    "config/device_registry/list",
    "config/entity_registry/list",
    "config/area_registry/list",
    "config/floor_registry/list",
    "config/label_registry/list",
];

/// An open `subscribe_entities`: its message id and the entities it was limited to, if any.
struct Subscription {
    id: u64,
    only: Option<HashSet<String>>,
}

impl Subscription {
    fn wants(&self, entity_id: &str) -> bool {
        self.only.as_ref().is_none_or(|s| s.contains(entity_id))
    }
}

struct Session {
    app: AppState,
    scope: Scope,
    user: Option<User>,
    subs: Vec<Subscription>,
}

async fn upgrade(State(app): State<AppState>, ws: WebSocketUpgrade) -> Response {
    ws.on_upgrade(move |socket| serve(app, socket))
}

async fn serve(app: AppState, mut ws: WebSocket) {
    let mut down = app.shutdown.subscribe();
    // Graceful shutdown waits for open connections, so end this one when it starts.
    tokio::select! {
        _ = run(&app, &mut ws) => {}
        _ = down.wait_for(|stopping| *stopping) => {}
    }
    let _ = ws.send(Message::Close(None)).await;
}

async fn send(ws: &mut WebSocket, value: Value) -> Result<(), ()> {
    ws.send(Message::text(value.to_string()))
        .await
        .map_err(|_| ())
}

fn result(id: u64, value: Value) -> Value {
    json!({"id": id, "type": "result", "success": true, "result": value})
}

fn failure(id: Option<u64>, code: &str, message: &str) -> Value {
    json!({
        "id": id, "type": "result", "success": false,
        "error": {"code": code, "message": message},
    })
}

/// Waits for the `auth` message; `None` when the client fails to authenticate in time.
async fn authenticate(app: &AppState, ws: &mut WebSocket) -> Option<(String, TokenAuth)> {
    let msg = tokio::time::timeout(AUTH_TIMEOUT, async {
        while let Some(Ok(m)) = ws.recv().await {
            if let Message::Text(t) = m {
                return Some(t);
            }
        }
        None
    })
    .await
    .ok()??;
    let body: Value = serde_json::from_str(&msg).unwrap_or(Value::Null);
    let token = (body["type"] == "auth")
        .then(|| body["access_token"].as_str())
        .flatten();
    if let Some(token) = token {
        let hash = sha256_hex(token);
        if let Some(auth) = app.core.store().token_auth(&hash).await {
            return Some((hash, auth));
        }
    }
    let _ = send(
        ws,
        json!({"type": "auth_invalid", "message": "Invalid access token or password"}),
    )
    .await;
    None
}

async fn run(app: &AppState, ws: &mut WebSocket) {
    if send(
        ws,
        json!({"type": "auth_required", "ha_version": HA_VERSION}),
    )
    .await
    .is_err()
    {
        return;
    }
    let Some((hash, auth)) = authenticate(app, ws).await else {
        return;
    };
    if send(ws, json!({"type": "auth_ok", "ha_version": HA_VERSION}))
        .await
        .is_err()
    {
        return;
    }
    let user = match auth.user_id {
        Some(id) => app.core.store().user_by_id(id).await,
        None => None,
    };
    let mut session = Session {
        app: app.clone(),
        scope: auth.scope,
        user,
        subs: Vec::new(),
    };
    let mut changes = app.core.subscribe();
    let mut ticker = tokio::time::interval_at(tokio::time::Instant::now() + RECHECK, RECHECK);
    loop {
        let sent = tokio::select! {
            incoming = ws.recv() => match incoming {
                Some(Ok(Message::Text(text))) => session.command(ws, &text).await,
                Some(Ok(Message::Close(_)) | Err(_)) | None => return,
                // Pings are answered by the library; binary frames are not part of the protocol.
                Some(Ok(_)) => Ok(()),
            },
            changed = changes.recv() => match changed {
                Ok(id) => session.changed(ws, &id).await,
                // Missed some: every subscriber gets the snapshot again, which replaces what
                // it had.
                Err(RecvError::Lagged(_)) => session.resync(ws).await,
                Err(RecvError::Closed) => return,
            },
            _ = ticker.tick() => {
                if !app.core.store().token_exists(&hash).await {
                    return;
                }
                Ok(())
            }
        };
        if sent.is_err() {
            return;
        }
    }
}

/// Seconds since the epoch of a `YYYY-MM-DDTHH:MM:SS.ffffff+00:00` timestamp, as the apps want.
fn epoch(ts: &str) -> f64 {
    let num = |range: std::ops::Range<usize>| -> i64 {
        ts.get(range).and_then(|s| s.parse().ok()).unwrap_or(0)
    };
    let (y, m, d) = (num(0..4), num(5..7), num(8..10));
    // Days since 1970-01-01; Howard Hinnant's algorithm.
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y.rem_euclid(400);
    let doy = (153 * (if m > 2 { m - 3 } else { m + 9 }) + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146_097 + doe - 719_468;
    let secs = days * 86_400 + num(11..13) * 3600 + num(14..16) * 60 + num(17..19);
    let micros: f64 = ts
        .get(19..)
        .and_then(|s| s.strip_prefix('.'))
        .and_then(|s| s.get(..6))
        .and_then(|s| s.parse().ok())
        .unwrap_or(0.0);
    secs as f64 + micros / 1_000_000.0
}

/// A state in the compressed form of `subscribe_entities`.
fn compressed(s: &EntityState) -> Value {
    json!({
        "s": s.state,
        "a": s.attributes,
        "c": s.context.id,
        "lc": epoch(&s.last_changed),
        "lu": epoch(&s.last_updated),
    })
}

impl Session {
    async fn visible_states(&self, only: Option<&HashSet<String>>) -> Vec<EntityState> {
        let mut out = Vec::new();
        for s in self.app.core.all_states() {
            if only.is_some_and(|o| !o.contains(&s.entity_id)) {
                continue;
            }
            if self.app.core.allowed(&self.scope, &s.entity_id).await {
                out.push(s);
            }
        }
        out
    }

    async fn snapshot(&self, sub: &Subscription) -> Value {
        let added: Map<String, Value> = self
            .visible_states(sub.only.as_ref())
            .await
            .iter()
            .map(|s| (s.entity_id.clone(), compressed(s)))
            .collect();
        json!({"id": sub.id, "type": "event", "event": {"a": added}})
    }

    async fn resync(&self, ws: &mut WebSocket) -> Result<(), ()> {
        for sub in &self.subs {
            send(ws, self.snapshot(sub).await).await?;
        }
        Ok(())
    }

    /// Pushes one entity's new state (or its removal) to the subscriptions that want it.
    async fn changed(&self, ws: &mut WebSocket, entity_id: &str) -> Result<(), ()> {
        if self.subs.is_empty() || !self.app.core.allowed(&self.scope, entity_id).await {
            return Ok(());
        }
        let event = match self.app.core.get_state(entity_id) {
            Some(s) => json!({"a": {entity_id: compressed(&s)}}),
            None => json!({"r": [entity_id]}),
        };
        for sub in self.subs.iter().filter(|s| s.wants(entity_id)) {
            send(ws, json!({"id": sub.id, "type": "event", "event": event})).await?;
        }
        Ok(())
    }

    async fn command(&mut self, ws: &mut WebSocket, text: &str) -> Result<(), ()> {
        let Ok(Value::Object(msg)) = serde_json::from_str::<Value>(text) else {
            return send(
                ws,
                failure(None, "invalid_format", "Message incorrectly formatted."),
            )
            .await;
        };
        let Some(id) = msg.get("id").and_then(Value::as_u64) else {
            return send(
                ws,
                failure(None, "invalid_format", "Message needs an integer id."),
            )
            .await;
        };
        let kind = msg.get("type").and_then(Value::as_str).unwrap_or_default();
        let reply = match kind {
            "ping" => json!({"id": id, "type": "pong"}),
            "supported_features" | "subscribe_events" => result(id, Value::Null),
            "unsubscribe_events" => {
                let target = msg.get("subscription").and_then(Value::as_u64);
                let before = self.subs.len();
                self.subs.retain(|s| Some(s.id) != target);
                if self.subs.len() == before {
                    failure(Some(id), "not_found", "Subscription not found.")
                } else {
                    result(id, Value::Null)
                }
            }
            "auth/current_user" => result(id, self.current_user()),
            "get_config" => result(id, config()),
            "get_services" | "get_panels" => result(id, json!({})),
            "get_states" => {
                let states = self.visible_states(None).await;
                result(id, json!(states))
            }
            "subscribe_entities" => {
                let only = msg.get("entity_ids").and_then(Value::as_array).map(|a| {
                    a.iter()
                        .filter_map(|v| v.as_str().map(str::to_string))
                        .collect()
                });
                let sub = Subscription { id, only };
                // The result comes first, then the snapshot as the first event.
                send(ws, result(id, Value::Null)).await?;
                let snapshot = self.snapshot(&sub).await;
                self.subs.retain(|s| s.id != id);
                self.subs.push(sub);
                snapshot
            }
            k if EMPTY_LISTS.contains(&k) => result(id, json!([])),
            _ => failure(Some(id), "unknown_command", "Unknown command."),
        };
        send(ws, reply).await
    }

    fn current_user(&self) -> Value {
        match &self.user {
            Some(u) => json!({
                "id": u.id.to_string(),
                "name": u.username,
                "is_owner": u.is_admin,
                "is_admin": u.is_admin,
                "credentials": [],
                "mfa_modules": [],
            }),
            None => json!({
                "id": "0", "name": "domus", "is_owner": false, "is_admin": false,
                "credentials": [], "mfa_modules": [],
            }),
        }
    }
}

pub fn router(app: AppState) -> Router {
    Router::new()
        .route("/api/websocket", get(upgrade))
        .with_state(app)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::core::Core;
    use crate::hue::HueManager;
    use crate::store::Store;
    use futures_util::{SinkExt, StreamExt};
    use std::sync::Arc;
    use tokio::net::TcpStream;
    use tokio_tungstenite::tungstenite::Message as Msg;
    use tokio_tungstenite::{MaybeTlsStream, WebSocketStream};

    type Client = WebSocketStream<MaybeTlsStream<TcpStream>>;

    const TOKEN: &str = "ws-token";

    async fn serve_app() -> (AppState, std::net::SocketAddr) {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let store = core.store().clone();
        let user = store.create_first_admin("admin", "x").await.unwrap();
        store
            .create_token(user, "t", &sha256_hex(TOKEN), None, false, None)
            .await
            .unwrap();
        let app = AppState::new(core.clone(), HueManager::new(core));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let router = crate::build_app(app.clone());
        tokio::spawn(async move { axum::serve(listener, router).await });
        (app, addr)
    }

    async fn connect(addr: std::net::SocketAddr) -> Client {
        let (c, _) = tokio_tungstenite::connect_async(format!("ws://{addr}/api/websocket"))
            .await
            .unwrap();
        c
    }

    async fn recv(c: &mut Client) -> Value {
        let m = tokio::time::timeout(Duration::from_secs(5), c.next())
            .await
            .expect("timed out")
            .expect("closed")
            .expect("error");
        match m {
            Msg::Text(t) => serde_json::from_str(&t).unwrap(),
            other => panic!("unexpected {other:?}"),
        }
    }

    async fn tell(c: &mut Client, v: Value) {
        c.send(Msg::text(v.to_string())).await.unwrap();
    }

    /// Whether the server ends the connection soon.
    async fn closed(c: &mut Client) -> bool {
        loop {
            match tokio::time::timeout(Duration::from_secs(5), c.next()).await {
                Ok(None | Some(Err(_)) | Some(Ok(Msg::Close(_)))) => return true,
                Ok(Some(Ok(_))) => continue,
                Err(_) => return false,
            }
        }
    }

    async fn authed(addr: std::net::SocketAddr, token: &str) -> Client {
        let mut c = connect(addr).await;
        assert_eq!(recv(&mut c).await["type"], "auth_required");
        tell(&mut c, json!({"type": "auth", "access_token": token})).await;
        assert_eq!(recv(&mut c).await["type"], "auth_ok");
        c
    }

    #[test]
    fn epoch_matches_known_timestamps() {
        assert_eq!(epoch("1970-01-01T00:00:00.000000+00:00"), 0.0);
        assert_eq!(epoch("2026-10-01T12:34:56.250000+00:00"), 1_790_858_096.25);
        assert_eq!(epoch("2000-03-01T00:00:00.000000+00:00"), 951_868_800.0);
    }

    #[tokio::test]
    async fn bad_token_and_silence_are_refused() {
        let (_, addr) = serve_app().await;
        let mut c = connect(addr).await;
        assert_eq!(recv(&mut c).await["type"], "auth_required");
        tell(&mut c, json!({"type": "auth", "access_token": "nope"})).await;
        let v = recv(&mut c).await;
        assert_eq!(v["type"], "auth_invalid");
        assert!(closed(&mut c).await);

        // not an auth message at all
        let mut c = connect(addr).await;
        recv(&mut c).await;
        tell(&mut c, json!({"id": 1, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["type"], "auth_invalid");

        // nothing sent: the auth timeout ends it
        let mut c = connect(addr).await;
        recv(&mut c).await;
        assert!(closed(&mut c).await);
    }

    #[tokio::test]
    async fn answers_the_commands_the_apps_send() {
        let (app, addr) = serve_app().await;
        app.core.set_state(
            "light.lamp",
            "on",
            serde_json::from_value(json!({"brightness": 200})).unwrap(),
        );
        let mut c = authed(addr, TOKEN).await;

        tell(&mut c, json!({"id": 1, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await, json!({"id": 1, "type": "pong"}));

        tell(&mut c, json!({"id": 2, "type": "auth/current_user"})).await;
        let v = recv(&mut c).await;
        assert_eq!(v["result"]["name"], "admin");
        assert_eq!(v["result"]["is_admin"], true);

        tell(&mut c, json!({"id": 3, "type": "get_config"})).await;
        assert_eq!(recv(&mut c).await["result"]["version"], HA_VERSION);

        tell(&mut c, json!({"id": 4, "type": "get_states"})).await;
        let v = recv(&mut c).await;
        assert_eq!(v["result"][0]["entity_id"], "light.lamp");
        assert_eq!(v["result"][0]["attributes"]["brightness"], 200);

        for (i, kind) in EMPTY_LISTS.iter().enumerate() {
            let id = 10 + i as u64;
            tell(&mut c, json!({"id": id, "type": kind})).await;
            assert_eq!(recv(&mut c).await, result(id, json!([])));
        }

        tell(
            &mut c,
            json!({"id": 20, "type": "supported_features", "features": {"coalesce_messages": 1}}),
        )
        .await;
        assert_eq!(recv(&mut c).await["success"], true);

        tell(&mut c, json!({"id": 21, "type": "no/such_command"})).await;
        let v = recv(&mut c).await;
        assert_eq!(
            (
                v["id"].clone(),
                v["success"].clone(),
                v["error"]["code"].clone()
            ),
            (json!(21), json!(false), json!("unknown_command"))
        );

        tell(&mut c, json!({"type": "ping"})).await;
        assert_eq!(recv(&mut c).await["error"]["code"], "invalid_format");
        c.send(Msg::text("not json")).await.unwrap();
        assert_eq!(recv(&mut c).await["error"]["code"], "invalid_format");
    }

    #[tokio::test]
    async fn subscribe_entities_sends_a_snapshot_then_changes() {
        let (app, addr) = serve_app().await;
        app.core.set_state("light.a", "on", Map::new());
        app.core.set_state("light.b", "off", Map::new());
        let mut c = authed(addr, TOKEN).await;

        tell(&mut c, json!({"id": 5, "type": "subscribe_entities"})).await;
        assert_eq!(recv(&mut c).await, result(5, Value::Null));
        let snap = recv(&mut c).await;
        assert_eq!(snap["id"], 5);
        assert_eq!(snap["type"], "event");
        let added = snap["event"]["a"].as_object().unwrap();
        assert_eq!(added.len(), 2);
        assert_eq!(added["light.a"]["s"], "on");
        assert!(added["light.a"]["lu"].as_f64().unwrap() > 1.7e9);

        app.core.set_state("light.a", "off", Map::new());
        let v = recv(&mut c).await;
        assert_eq!(v["event"]["a"]["light.a"]["s"], "off");
        app.core.remove_state("light.b");
        let v = recv(&mut c).await;
        assert_eq!(v["event"], json!({"r": ["light.b"]}));

        tell(
            &mut c,
            json!({"id": 6, "type": "unsubscribe_events", "subscription": 5}),
        )
        .await;
        assert_eq!(recv(&mut c).await["success"], true);
        tell(
            &mut c,
            json!({"id": 7, "type": "unsubscribe_events", "subscription": 5}),
        )
        .await;
        assert_eq!(recv(&mut c).await["error"]["code"], "not_found");
        // nothing is pushed any more: the next reply is the pong
        app.core.set_state("light.a", "on", Map::new());
        tell(&mut c, json!({"id": 8, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["type"], "pong");
    }

    #[tokio::test]
    async fn subscription_can_be_limited_to_entities() {
        let (app, addr) = serve_app().await;
        app.core.set_state("light.a", "on", Map::new());
        app.core.set_state("light.b", "on", Map::new());
        let mut c = authed(addr, TOKEN).await;
        tell(
            &mut c,
            json!({"id": 1, "type": "subscribe_entities", "entity_ids": ["light.b"]}),
        )
        .await;
        recv(&mut c).await;
        let snap = recv(&mut c).await;
        assert_eq!(snap["event"]["a"].as_object().unwrap().len(), 1);
        app.core.set_state("light.a", "off", Map::new());
        app.core.set_state("light.b", "off", Map::new());
        assert_eq!(recv(&mut c).await["event"]["a"]["light.b"]["s"], "off");
    }

    #[tokio::test]
    async fn scoped_tokens_only_see_their_groups() {
        let (app, addr) = serve_app().await;
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
        app.core.set_state("group.Garmin", "on", Map::new());
        app.core.set_state("light.other", "on", Map::new());
        let mut c = authed(addr, "scoped").await;

        tell(&mut c, json!({"id": 1, "type": "get_states"})).await;
        let v = recv(&mut c).await;
        assert_eq!(v["result"].as_array().unwrap().len(), 1);
        assert_eq!(v["result"][0]["entity_id"], "group.Garmin");

        tell(&mut c, json!({"id": 2, "type": "subscribe_entities"})).await;
        recv(&mut c).await;
        let snap = recv(&mut c).await;
        assert_eq!(snap["event"]["a"].as_object().unwrap().len(), 1);
        app.core.set_state("light.other", "off", Map::new());
        app.core.set_state("group.Garmin", "off", Map::new());
        assert_eq!(recv(&mut c).await["event"]["a"]["group.Garmin"]["s"], "off");
    }

    #[tokio::test]
    async fn connections_end_on_revocation_and_on_shutdown() {
        let (app, addr) = serve_app().await;
        app.core
            .store()
            .create_token(1, "temp", &sha256_hex("temp"), None, false, None)
            .await
            .unwrap();
        let mut revoked = authed(addr, "temp").await;
        let mut staying = authed(addr, TOKEN).await;
        let id = app.core.store().list_tokens(None).await[1].id;
        assert!(app.core.store().revoke_token(id, None).await);
        assert!(closed(&mut revoked).await);

        tell(&mut staying, json!({"id": 1, "type": "ping"})).await;
        assert_eq!(recv(&mut staying).await["type"], "pong");
        app.shutdown.send_replace(true);
        assert!(closed(&mut staying).await);
    }
}
