//! The Home Assistant WebSocket API (`/api/websocket`), the subset the Companion apps need:
//! authentication, the state snapshot and its updates, and enough registry commands for the
//! apps to start. Anything else answers `unknown_command`.

use std::collections::HashSet;
use std::time::Duration;

use axum::Router;
use axum::extract::State;
use axum::extract::ws::{Message, WebSocket, WebSocketUpgrade};
use axum::http::StatusCode;
use axum::response::Response;
use axum::routing::get;
use serde_json::{Map, Value, json};
use tokio::sync::broadcast::error::RecvError;

use crate::api::AppState;
use crate::core::{State as EntityState, group_light_id};
use crate::mobile::{HA_VERSION, config};
use crate::store::{Scope, TokenAuth, User};
use crate::util::sha256_hex;

/// How long a client may take to send its `auth` message.
#[cfg(not(test))]
const AUTH_TIMEOUT: Duration = Duration::from_secs(10);
#[cfg(test)]
const AUTH_TIMEOUT: Duration = Duration::from_millis(300);

/// How often the login is checked for revocation and the user's groups for changes.
#[cfg(not(test))]
const RECHECK: Duration = Duration::from_secs(30);
#[cfg(test)]
const RECHECK: Duration = Duration::from_millis(100);

/// A peer that does not take what is sent to it this long is dropped instead of stalling.
const SEND_TIMEOUT: Duration = Duration::from_secs(10);
/// Largest message accepted, before and after authentication; the apps send tiny ones.
const MAX_MESSAGE: usize = 64 * 1024;
/// Open `subscribe_entities` / `subscribe_events` a connection may hold.
const MAX_SUBSCRIPTIONS: usize = 16;

/// Registries domus has nothing in; the apps ask for them and expect a list.
const EMPTY_LISTS: &[&str] = &[
    "config/device_registry/list",
    "config/entity_registry/list",
    "config/area_registry/list",
    "config/floor_registry/list",
    "config/label_registry/list",
];

/// An open `subscribe_entities`: its message id, the entities it was limited to (if any) and
/// the ones the client currently has, so removals can be told.
struct Subscription {
    id: u64,
    only: Option<HashSet<String>>,
    known: HashSet<String>,
}

impl Subscription {
    fn wants(&self, entity_id: &str) -> bool {
        self.only.as_ref().is_none_or(|s| s.contains(entity_id))
    }
}

struct Session {
    app: AppState,
    scope: Scope,
    read_only: bool,
    user: Option<User>,
    subs: Vec<Subscription>,
    /// Ids of `subscribe_events`, acknowledged but never fed.
    events: HashSet<u64>,
    /// Message ids must increase, as in Home Assistant.
    last_id: u64,
}

async fn upgrade(State(app): State<AppState>, ws: WebSocketUpgrade) -> Response {
    ws.max_message_size(MAX_MESSAGE)
        .max_frame_size(MAX_MESSAGE)
        .on_upgrade(move |socket| serve(app, socket))
}

async fn serve(app: AppState, mut ws: WebSocket) {
    let mut down = app.shutdown.subscribe();
    // Graceful shutdown waits for open connections, so end this one when it starts.
    tokio::select! {
        _ = run(&app, &mut ws) => {}
        _ = down.wait_for(|stopping| *stopping) => {}
    }
    let _ = send_message(&mut ws, Message::Close(None)).await;
}

async fn send_message(ws: &mut WebSocket, message: Message) -> Result<(), ()> {
    match tokio::time::timeout(SEND_TIMEOUT, ws.send(message)).await {
        Ok(Ok(())) => Ok(()),
        _ => Err(()),
    }
}

async fn send(ws: &mut WebSocket, value: Value) -> Result<(), ()> {
    send_message(ws, Message::text(value.to_string())).await
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
        read_only: auth.read_only,
        user,
        subs: Vec::new(),
        events: HashSet::new(),
        last_id: 0,
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
                // Missed some: bring every subscriber up to date.
                Err(RecvError::Lagged(_)) => session.resync(ws).await,
                Err(RecvError::Closed) => return,
            },
            _ = ticker.tick() => match session.recheck(&hash, &auth.refresh_id).await {
                Some(true) => session.resync(ws).await,
                Some(false) => Ok(()),
                None => return,
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
    /// Every state the caller may see, synthesised groups and group lights included.
    async fn visible_states(&self, only: Option<&HashSet<String>>) -> Vec<EntityState> {
        crate::api::visible_states(&self.app, &self.scope, only).await
    }

    /// Brings one subscription up to date: states it lacks or has stale are added (replacing
    /// whatever the client had), those it has but may no longer see are removed.
    async fn sync(&mut self, ws: &mut WebSocket, index: usize) -> Result<(), ()> {
        let sub = &self.subs[index];
        let states = self.visible_states(sub.only.as_ref()).await;
        let now: HashSet<String> = states.iter().map(|s| s.entity_id.clone()).collect();
        let added: Map<String, Value> = states
            .iter()
            .map(|s| (s.entity_id.clone(), compressed(s)))
            .collect();
        let mut event = json!({"a": added});
        let gone: Vec<&String> = sub.known.difference(&now).collect();
        if !gone.is_empty() {
            event["r"] = json!(gone);
        }
        send(ws, json!({"id": sub.id, "type": "event", "event": event})).await?;
        self.subs[index].known = now;
        Ok(())
    }

    async fn resync(&mut self, ws: &mut WebSocket) -> Result<(), ()> {
        for index in 0..self.subs.len() {
            self.sync(ws, index).await?;
        }
        Ok(())
    }

    /// Pushes what a change of one entity does to the states the client can see: the entity
    /// itself and every group (and group light) it is a member of.
    async fn changed(&mut self, ws: &mut WebSocket, entity_id: &str) -> Result<(), ()> {
        if self.subs.is_empty() {
            return Ok(());
        }
        let core = self.app.core.clone();
        let mut affected = vec![entity_id.to_string()];
        for name in core.store().group_names().await {
            if core
                .store()
                .group_members(&name)
                .await
                .iter()
                .any(|m| m == entity_id)
            {
                affected.push(format!("group.{name}"));
                if core.store().group_exposed(&name).await {
                    affected.push(group_light_id(&name));
                }
            }
        }
        let mut current: Vec<(String, Option<EntityState>)> = Vec::new();
        for id in affected {
            let state = if core.allowed(&self.scope, &id).await {
                core.lookup(&id).await
            } else {
                None
            };
            current.push((id, state));
        }
        for sub in &mut self.subs {
            let mut added = Map::new();
            let mut removed = Vec::new();
            for (id, state) in &current {
                if !sub.wants(id) {
                    continue;
                }
                match state {
                    Some(s) => {
                        added.insert(id.clone(), compressed(s));
                        sub.known.insert(id.clone());
                    }
                    None if sub.known.remove(id) => removed.push(id.clone()),
                    None => {}
                }
            }
            let mut event = Map::new();
            if !added.is_empty() {
                event.insert("a".into(), Value::Object(added));
            }
            if !removed.is_empty() {
                event.insert("r".into(), json!(removed));
            }
            if !event.is_empty() {
                send(ws, json!({"id": sub.id, "type": "event", "event": event})).await?;
            }
        }
        Ok(())
    }

    /// Periodic check: `None` when the login is gone (the connection ends), `Some(true)` when
    /// what the caller may see changed (resync), `Some(false)` otherwise. Like Home Assistant,
    /// an app connection lives as long as its login, not its short-lived access token.
    async fn recheck(&mut self, hash: &str, refresh_id: &Option<i64>) -> Option<bool> {
        let store = self.app.core.store();
        let user = match self.user.as_ref() {
            Some(u) => store.user_by_id(u.id).await,
            None => None,
        };
        let scope = match refresh_id {
            Some(id) => {
                if !store.refresh_exists(*id).await {
                    return None;
                }
                // App logins carry no scope of their own: it is the user's.
                user.as_ref().and_then(|u| u.scope.clone())
            }
            None => store.token_view(hash).await?.scope,
        };
        if self.user.is_some() && user.is_none() {
            return None;
        }
        self.user = user;
        let changed = scope != self.scope;
        self.scope = scope;
        Some(changed)
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
        if id <= self.last_id {
            return send(
                ws,
                failure(Some(id), "id_reuse", "Identifier values have to increase."),
            )
            .await;
        }
        self.last_id = id;
        let kind = msg.get("type").and_then(Value::as_str).unwrap_or_default();
        let reply = match kind {
            "ping" => json!({"id": id, "type": "pong"}),
            "supported_features" => result(id, Value::Null),
            "subscribe_events" => {
                if self.subs.len() + self.events.len() >= MAX_SUBSCRIPTIONS {
                    failure(Some(id), "too_many", "Too many subscriptions.")
                } else {
                    self.events.insert(id);
                    result(id, Value::Null)
                }
            }
            "unsubscribe_events" => {
                let target = msg.get("subscription").and_then(Value::as_u64);
                let before = self.subs.len() + self.events.len();
                self.subs.retain(|s| Some(s.id) != target);
                if let Some(t) = target {
                    self.events.remove(&t);
                }
                if self.subs.len() + self.events.len() == before {
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
                if self.subs.len() + self.events.len() >= MAX_SUBSCRIPTIONS {
                    failure(Some(id), "too_many", "Too many subscriptions.")
                } else {
                    let only = msg.get("entity_ids").and_then(Value::as_array).map(|a| {
                        a.iter()
                            .filter_map(|v| v.as_str().map(str::to_string))
                            .collect()
                    });
                    self.subs.push(Subscription {
                        id,
                        only,
                        known: HashSet::new(),
                    });
                    // The result comes first, then the snapshot as the first event.
                    send(ws, result(id, Value::Null)).await?;
                    let index = self.subs.len() - 1;
                    return self.sync(ws, index).await;
                }
            }
            "config/entity_registry/list_for_display" => {
                let entities: Vec<Value> = self
                    .visible_states(None)
                    .await
                    .iter()
                    .map(|s| {
                        let mut e = json!({"ei": s.entity_id});
                        if let Some(name) = s.attributes.get("friendly_name") {
                            e["en"] = name.clone();
                        }
                        e
                    })
                    .collect();
                result(id, json!({"entity_categories": {}, "entities": entities}))
            }
            "call_service" => self.call_service(id, &msg).await,
            k if EMPTY_LISTS.contains(&k) => result(id, json!([])),
            _ => failure(Some(id), "unknown_command", "Unknown command."),
        };
        send(ws, reply).await
    }

    /// `call_service` for the domains domus has: lights and scenes.
    async fn call_service(&self, id: u64, msg: &Map<String, Value>) -> Value {
        if self.read_only {
            return failure(Some(id), "unauthorized", "This token is read-only.");
        }
        let mut body = match msg.get("service_data") {
            Some(Value::Object(o)) => o.clone(),
            _ => Map::new(),
        };
        if let Some(target) = msg.get("target") {
            body.insert("target".into(), target.clone());
        }
        let body = Value::Object(body);
        let domain = msg
            .get("domain")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let service = msg
            .get("service")
            .and_then(Value::as_str)
            .unwrap_or_default();
        let outcome = match domain {
            "light" => crate::api::run_light(&self.app, &self.scope, service, &body).await,
            "scene" => crate::api::run_scene(&self.app, &self.scope, service, &body).await,
            _ => Err((StatusCode::NOT_FOUND, String::new())),
        };
        match outcome {
            Ok(_) => result(
                id,
                json!({"context": {"id": "", "parent_id": null, "user_id": null}, "response": null}),
            ),
            Err((StatusCode::NOT_FOUND, _)) => failure(Some(id), "not_found", "Service not found."),
            Err((StatusCode::BAD_REQUEST, m)) => failure(Some(id), "invalid_format", &m),
            Err((_, m)) => failure(Some(id), "home_assistant_error", &m),
        }
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
        serde_json::from_str(&m.into_text().unwrap()).unwrap()
    }

    async fn tell(c: &mut Client, v: Value) {
        c.send(Msg::text(v.to_string())).await.unwrap();
    }

    /// Whether the server ends the connection soon.
    async fn closed(c: &mut Client) -> bool {
        let drain = async {
            while let Some(Ok(m)) = c.next().await {
                if m.is_close() {
                    break;
                }
            }
        };
        tokio::time::timeout(Duration::from_secs(5), drain)
            .await
            .is_ok()
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
        let store = app.core.store().clone();
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
            .await
            .unwrap();
        app.core.set_state("light.a", "on", Map::new());
        app.core.set_state("light.other", "on", Map::new());
        store
            .group_set("Garmin", &["light.a".to_string()])
            .await
            .unwrap();
        store.group_set_exposed("Garmin", true).await;
        let mut c = authed(addr, "scoped").await;

        // the real member, the group and its group light; not what is outside the group
        tell(&mut c, json!({"id": 1, "type": "get_states"})).await;
        let v = recv(&mut c).await;
        let ids: Vec<&str> = v["result"]
            .as_array()
            .unwrap()
            .iter()
            .map(|s| s["entity_id"].as_str().unwrap())
            .collect();
        assert_eq!(ids, ["group.Garmin", "light.a", "light.domus_group_Garmin"]);

        tell(&mut c, json!({"id": 2, "type": "subscribe_entities"})).await;
        recv(&mut c).await;
        let snap = recv(&mut c).await;
        assert_eq!(snap["event"]["a"].as_object().unwrap().len(), 3);
        // outside the scope: nothing is sent, so the next event is about the group
        app.core.set_state("light.other", "off", Map::new());
        app.core.set_state("light.a", "off", Map::new());
        let v = recv(&mut c).await;
        let a = v["event"]["a"].as_object().unwrap();
        assert_eq!(a["light.a"]["s"], "off");
        assert_eq!(a["group.Garmin"]["s"], "off");
        assert_eq!(a["light.domus_group_Garmin"]["s"], "off");
        assert!(!a.contains_key("light.other"));
    }

    #[tokio::test]
    async fn narrowing_a_users_groups_takes_effect_on_open_connections() {
        let (app, addr) = serve_app().await;
        let store = app.core.store().clone();
        let bob = store.create_user("bob", "x", false).await.unwrap();
        store
            .create_token(bob, "bob", &sha256_hex("bob"), None, false, None)
            .await
            .unwrap();
        app.core.set_state("light.a", "on", Map::new());
        app.core.set_state("light.b", "on", Map::new());
        store
            .group_set("One", &["light.a".to_string()])
            .await
            .unwrap();
        let mut c = authed(addr, "bob").await;
        tell(&mut c, json!({"id": 1, "type": "subscribe_entities"})).await;
        recv(&mut c).await;
        assert_eq!(
            recv(&mut c).await["event"]["a"].as_object().unwrap().len(),
            3
        );

        assert!(store.set_user_scope(bob, Some(&["One".to_string()])).await);
        let v = recv(&mut c).await;
        let removed = v["event"]["r"].as_array().unwrap();
        assert_eq!(removed, &[json!("light.b")]);
        tell(&mut c, json!({"id": 2, "type": "get_states"})).await;
        assert_eq!(recv(&mut c).await["result"].as_array().unwrap().len(), 2);
    }

    #[tokio::test]
    async fn app_connections_outlive_their_access_token() {
        let (app, addr) = serve_app().await;
        let store = app.core.store().clone();
        let refresh = store
            .refresh_create(1, "https://home-assistant.io/iOS", &sha256_hex("r"))
            .await
            .unwrap();
        assert!(
            store
                .create_app_token(1, "app", &sha256_hex("first"), i64::MAX, refresh)
                .await
        );
        let mut c = authed(addr, "first").await;
        // the app refreshed: the old access token is replaced by a new one
        assert!(
            store
                .create_app_token(1, "app", &sha256_hex("second"), i64::MAX, refresh)
                .await
        );
        tokio::time::sleep(RECHECK * 3).await;
        tell(&mut c, json!({"id": 1, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["type"], "pong");
        // logging the app out ends it
        store.delete_user_refresh_tokens(1).await;
        assert!(closed(&mut c).await);
    }

    #[tokio::test]
    async fn ids_must_increase_and_subscriptions_are_capped() {
        let (_, addr) = serve_app().await;
        let mut c = authed(addr, TOKEN).await;
        tell(&mut c, json!({"id": 5, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["type"], "pong");
        tell(&mut c, json!({"id": 5, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["error"]["code"], "id_reuse");
        for id in 6..6 + MAX_SUBSCRIPTIONS as u64 {
            tell(&mut c, json!({"id": id, "type": "subscribe_events"})).await;
            assert_eq!(recv(&mut c).await["success"], true);
        }
        tell(&mut c, json!({"id": 99, "type": "subscribe_events"})).await;
        assert_eq!(recv(&mut c).await["error"]["code"], "too_many");
        tell(&mut c, json!({"id": 100, "type": "subscribe_entities"})).await;
        assert_eq!(recv(&mut c).await["error"]["code"], "too_many");
        tell(
            &mut c,
            json!({"id": 101, "type": "unsubscribe_events", "subscription": 6}),
        )
        .await;
        assert_eq!(recv(&mut c).await["success"], true);
    }

    #[tokio::test]
    async fn entity_registry_for_display_lists_visible_entities() {
        let (app, addr) = serve_app().await;
        let mut attrs = Map::new();
        attrs.insert("friendly_name".into(), json!("Desk"));
        app.core.set_state("light.hue_a", "on", attrs);
        let mut c = authed(addr, TOKEN).await;
        tell(
            &mut c,
            json!({"id": 1, "type": "config/entity_registry/list_for_display"}),
        )
        .await;
        let r = recv(&mut c).await;
        assert_eq!(r["result"]["entity_categories"], json!({}));
        assert_eq!(
            r["result"]["entities"],
            json!([{"ei": "light.hue_a", "en": "Desk"}])
        );
    }

    #[tokio::test]
    async fn call_service_runs_lights_and_rejects_the_rest() {
        let (app, addr) = serve_app().await;
        app.core.set_state("light.hue_a", "on", Map::new());
        let mut c = authed(addr, TOKEN).await;
        tell(
            &mut c,
            json!({"id": 1, "type": "call_service", "domain": "light", "service": "toggle",
                   "target": {"entity_id": "light.hue_a"}}),
        )
        .await;
        let r = recv(&mut c).await;
        assert_eq!(
            (r["id"].clone(), r["success"].clone()),
            (json!(1), json!(true))
        );
        for (domain, service, code) in [
            ("switch", "toggle", "not_found"),
            ("light", "nope", "not_found"),
            ("light", "turn_on", "invalid_format"),
        ] {
            let mut msg = json!({"id": 2, "type": "call_service", "domain": domain,
                                 "service": service, "service_data": {"entity_id": 5}});
            if code != "invalid_format" {
                msg["service_data"] = json!({});
            }
            tell(&mut c, msg).await;
            let r = recv(&mut c).await;
            assert_eq!(r["error"]["code"], code, "{domain}.{service}");
            tell(&mut c, json!({"id": 3, "type": "ping"})).await;
            recv(&mut c).await;
            c = authed(addr, TOKEN).await;
        }
    }

    #[tokio::test]
    async fn a_lagging_connection_is_brought_up_to_date() {
        let (app, addr) = serve_app().await;
        app.core.set_state("light.a", "start", Map::new());
        let mut c = authed(addr, TOKEN).await;
        tell(&mut c, json!({"id": 1, "type": "subscribe_entities"})).await;
        recv(&mut c).await;
        recv(&mut c).await;
        // The server task cannot run during this loop (current-thread runtime), so more
        // changes than the channel holds pile up and the connection lags.
        for i in 0..1000 {
            app.core.set_state("light.a", &format!("s{i}"), Map::new());
        }
        assert_eq!(recv(&mut c).await["event"]["a"]["light.a"]["s"], "s999");
    }

    #[tokio::test]
    async fn binary_frames_are_ignored_and_close_ends_the_session() {
        let (_, addr) = serve_app().await;
        let mut c = authed(addr, TOKEN).await;
        c.send(Msg::Binary(vec![1, 2, 3].into())).await.unwrap();
        tell(&mut c, json!({"id": 1, "type": "ping"})).await;
        assert_eq!(recv(&mut c).await["type"], "pong");
        c.send(Msg::Close(None)).await.unwrap();
        assert!(closed(&mut c).await);
    }

    #[tokio::test]
    async fn deleting_the_user_ends_the_connection() {
        let (app, addr) = serve_app().await;
        let store = app.core.store().clone();
        let bob = store.create_user("bob", "x", false).await.unwrap();
        store
            .create_token(bob, "bob", &sha256_hex("bob"), None, false, None)
            .await
            .unwrap();
        let mut c = authed(addr, "bob").await;
        assert!(store.delete_user(bob).await);
        assert!(closed(&mut c).await);
    }

    #[tokio::test]
    async fn oversized_messages_end_the_connection() {
        let (_, addr) = serve_app().await;
        let mut c = connect(addr).await;
        recv(&mut c).await;
        c.send(Msg::text("x".repeat(MAX_MESSAGE + 1)))
            .await
            .unwrap();
        assert!(closed(&mut c).await);
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
