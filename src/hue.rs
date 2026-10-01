//! Philips Hue Bridge integration over the CLIP v2 API.
//!
//! The bridge serves HTTPS with a certificate signed by a private CA, so certificate
//! verification is disabled for bridge connections only (LAN traffic to a fixed address).

use futures_util::StreamExt;
use serde_json::{Map, Value, json};
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::{info, warn};

use crate::core::{BoxFut, Core, Integration, LightAction};

pub const INTEGRATION_NAME: &str = "hue";
const ENTITY_PREFIX: &str = "light.hue_";
const STREAM_IDLE_TIMEOUT: Duration = Duration::from_secs(120);
const RECONNECT_DELAY: Duration = Duration::from_secs(5);

// ---------------------------------------------------------------- mapping

#[derive(Debug, Clone, PartialEq)]
pub struct HueLight {
    pub id: String,
    pub name: String,
    pub on: bool,
    /// Hue brightness, 0.0..=100.0
    pub brightness: Option<f64>,
}

/// Hue percent (0-100) to Home Assistant brightness (1-255).
pub fn pct_to_ha(pct: f64) -> u8 {
    (pct / 100.0 * 255.0).round().clamp(1.0, 255.0) as u8
}

/// Home Assistant brightness (0-255) to Hue percent, one decimal.
pub fn ha_to_pct(brightness: u8) -> f64 {
    (f64::from(brightness) / 255.0 * 1000.0).round() / 10.0
}

impl HueLight {
    pub fn from_resource(v: &Value) -> Option<Self> {
        Some(Self {
            id: v.get("id")?.as_str()?.to_string(),
            name: v
                .pointer("/metadata/name")
                .and_then(Value::as_str)
                .unwrap_or("Hue light")
                .to_string(),
            on: v
                .pointer("/on/on")
                .and_then(Value::as_bool)
                .unwrap_or(false),
            brightness: v.pointer("/dimming/brightness").and_then(Value::as_f64),
        })
    }

    /// Merges a (partial) update event into this light.
    pub fn apply_update(&mut self, v: &Value) {
        if let Some(on) = v.pointer("/on/on").and_then(Value::as_bool) {
            self.on = on;
        }
        if let Some(b) = v.pointer("/dimming/brightness").and_then(Value::as_f64) {
            self.brightness = Some(b);
        }
        if let Some(n) = v.pointer("/metadata/name").and_then(Value::as_str) {
            self.name = n.to_string();
        }
    }

    /// Stable id derived from the Hue resource uuid, so renames never break groups.
    pub fn entity_id(&self) -> String {
        let short: String = self.id.chars().filter(|c| *c != '-').take(8).collect();
        format!("{ENTITY_PREFIX}{short}")
    }

    pub fn ha_state(&self) -> &'static str {
        if self.on { "on" } else { "off" }
    }

    /// Deliberately small: constrained clients (watches) must parse the whole payload.
    pub fn attributes(&self) -> Map<String, Value> {
        let mut m = Map::new();
        m.insert("friendly_name".into(), json!(self.name));
        if self.on
            && let Some(b) = self.brightness
        {
            m.insert("brightness".into(), json!(pct_to_ha(b)));
        }
        m
    }
}

// -------------------------------------------------------------------- SSE

/// Incremental server-sent-events parser returning the `data:` payload of each event.
#[derive(Default)]
pub struct SseParser {
    buf: Vec<u8>,
}

impl SseParser {
    pub fn push(&mut self, chunk: &[u8]) -> Vec<String> {
        self.buf
            .extend(chunk.iter().copied().filter(|b| *b != b'\r'));
        let mut out = Vec::new();
        while let Some(pos) = self.buf.windows(2).position(|w| w == b"\n\n") {
            let block: Vec<u8> = self.buf.drain(..pos + 2).collect();
            let text = String::from_utf8_lossy(&block[..pos]);
            let data: Vec<&str> = text
                .lines()
                .filter_map(|l| l.strip_prefix("data:"))
                .map(|d| d.strip_prefix(' ').unwrap_or(d))
                .collect();
            if !data.is_empty() {
                out.push(data.join("\n"));
            }
        }
        out
    }
}

// ----------------------------------------------------------------- client

#[derive(Debug, PartialEq)]
pub enum PairError {
    LinkButtonNotPressed,
    Other(String),
}

/// `ip` is a bare host/IP; a full `http(s)://` base is passed through (used by tests).
pub fn bridge_base(ip: &str) -> String {
    if ip.starts_with("http://") || ip.starts_with("https://") {
        ip.trim_end_matches('/').to_string()
    } else {
        format!("https://{ip}")
    }
}

fn bridge_http(timeout: Option<Duration>) -> reqwest::Client {
    let mut b = reqwest::Client::builder()
        .danger_accept_invalid_certs(true)
        .connect_timeout(Duration::from_secs(10));
    if let Some(t) = timeout {
        b = b.timeout(t);
    }
    b.build().expect("reqwest client builds")
}

/// Asks the bridge for an application key. The bridge's link button must have been pressed.
pub async fn pair(base: &str) -> Result<String, PairError> {
    let resp = bridge_http(Some(Duration::from_secs(10)))
        .post(format!("{base}/api"))
        .json(&json!({"devicetype": "domus#server", "generateclientkey": true}))
        .send()
        .await
        .map_err(|e| PairError::Other(e.to_string()))?;
    let body: Value = resp
        .json()
        .await
        .map_err(|e| PairError::Other(e.to_string()))?;
    let first = body
        .get(0)
        .ok_or_else(|| PairError::Other("empty response".into()))?;
    if let Some(key) = first.pointer("/success/username").and_then(Value::as_str) {
        return Ok(key.to_string());
    }
    match first.pointer("/error/type").and_then(Value::as_i64) {
        Some(101) => Err(PairError::LinkButtonNotPressed),
        _ => Err(PairError::Other(first.to_string())),
    }
}

#[derive(Clone)]
pub struct HueClient {
    base: String,
    key: String,
    http: reqwest::Client,
    sse: reqwest::Client,
}

impl HueClient {
    pub fn new(base: &str, key: &str) -> Self {
        Self {
            base: base.trim_end_matches('/').to_string(),
            key: key.to_string(),
            http: bridge_http(Some(Duration::from_secs(10))),
            sse: bridge_http(None),
        }
    }

    pub async fn list_lights(&self) -> Result<Vec<HueLight>, String> {
        let resp = self
            .http
            .get(format!("{}/clip/v2/resource/light", self.base))
            .header("hue-application-key", &self.key)
            .send()
            .await
            .map_err(|e| e.to_string())?;
        if !resp.status().is_success() {
            return Err(format!("bridge returned {}", resp.status()));
        }
        let body: Value = resp.json().await.map_err(|e| e.to_string())?;
        Ok(body
            .get("data")
            .and_then(Value::as_array)
            .map(|a| a.iter().filter_map(HueLight::from_resource).collect())
            .unwrap_or_default())
    }

    pub async fn set_light(
        &self,
        id: &str,
        on: bool,
        brightness_pct: Option<f64>,
    ) -> Result<(), String> {
        let mut body = json!({"on": {"on": on}});
        if let Some(b) = brightness_pct {
            body["dimming"] = json!({"brightness": b});
        }
        let resp = self
            .http
            .put(format!("{}/clip/v2/resource/light/{id}", self.base))
            .header("hue-application-key", &self.key)
            .json(&body)
            .send()
            .await
            .map_err(|e| e.to_string())?;
        let status = resp.status();
        let text = resp.text().await.unwrap_or_default();
        if !status.is_success() {
            return Err(format!("bridge returned {status}"));
        }
        if let Ok(v) = serde_json::from_str::<Value>(&text)
            && v.get("errors")
                .and_then(Value::as_array)
                .is_some_and(|e| !e.is_empty())
        {
            return Err(format!("bridge rejected request: {}", v["errors"]));
        }
        Ok(())
    }

    async fn event_stream(&self) -> Result<reqwest::Response, String> {
        let resp = self
            .sse
            .get(format!("{}/eventstream/clip/v2", self.base))
            .header("hue-application-key", &self.key)
            .header("Accept", "text/event-stream")
            .send()
            .await
            .map_err(|e| e.to_string())?;
        if !resp.status().is_success() {
            return Err(format!("event stream returned {}", resp.status()));
        }
        Ok(resp)
    }
}

// ------------------------------------------------------------ integration

#[derive(Default)]
struct Inner {
    lights: HashMap<String, HueLight>,
    by_entity: HashMap<String, String>,
}

pub struct HueIntegration {
    client: HueClient,
    core: Arc<Core>,
    inner: Mutex<Inner>,
}

impl HueIntegration {
    pub fn new(client: HueClient, core: Arc<Core>) -> Arc<Self> {
        Arc::new(Self {
            client,
            core,
            inner: Mutex::new(Inner::default()),
        })
    }

    fn inner(&self) -> std::sync::MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Fetches all lights and replaces the known set (adds, updates and removes states).
    pub async fn sync(&self) -> Result<usize, String> {
        let lights = self.client.list_lights().await?;
        let n = lights.len();
        let mut inner = self.inner();
        let old: HashSet<String> = inner.by_entity.keys().cloned().collect();
        inner.lights.clear();
        inner.by_entity.clear();
        for l in lights {
            let eid = l.entity_id();
            self.core.set_state(&eid, l.ha_state(), l.attributes());
            inner.by_entity.insert(eid, l.id.clone());
            inner.lights.insert(l.id.clone(), l);
        }
        for gone in old.iter().filter(|e| !inner.by_entity.contains_key(*e)) {
            self.core.remove_state(gone);
        }
        Ok(n)
    }

    /// Applies one SSE payload. Returns true when a full resync is needed (light added/removed).
    pub fn apply_event_payload(&self, payload: &str) -> bool {
        let Ok(Value::Array(events)) = serde_json::from_str::<Value>(payload) else {
            return false;
        };
        let mut resync = false;
        let mut inner = self.inner();
        for ev in &events {
            let kind = ev.get("type").and_then(Value::as_str).unwrap_or("");
            for item in ev
                .get("data")
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
            {
                if item.get("type").and_then(Value::as_str) != Some("light") {
                    continue;
                }
                match kind {
                    "update" => {
                        let Some(id) = item.get("id").and_then(Value::as_str) else {
                            continue;
                        };
                        if let Some(light) = inner.lights.get_mut(id) {
                            light.apply_update(item);
                            self.core.set_state(
                                &light.entity_id(),
                                light.ha_state(),
                                light.attributes(),
                            );
                        }
                    }
                    "add" | "delete" => resync = true,
                    _ => {}
                }
            }
        }
        resync
    }

    /// Streams events until the connection ends or goes idle.
    pub async fn stream_once(&self) -> Result<(), String> {
        let resp = self.client.event_stream().await?;
        let mut stream = resp.bytes_stream();
        let mut parser = SseParser::default();
        loop {
            let next = tokio::time::timeout(STREAM_IDLE_TIMEOUT, stream.next())
                .await
                .map_err(|_| "event stream idle timeout".to_string())?;
            match next {
                None => return Err("event stream closed".into()),
                Some(Err(e)) => return Err(e.to_string()),
                Some(Ok(chunk)) => {
                    for payload in parser.push(&chunk) {
                        if self.apply_event_payload(&payload)
                            && let Err(e) = self.sync().await
                        {
                            warn!("hue resync failed: {e}");
                        }
                    }
                }
            }
        }
    }

    /// Sync + stream forever, reconnecting after failures.
    pub async fn run(self: Arc<Self>) {
        loop {
            match self.sync().await {
                Ok(n) => info!("hue: synced {n} lights"),
                Err(e) => warn!("hue: sync failed: {e}"),
            }
            if let Err(e) = self.stream_once().await {
                warn!("hue: {e}");
            }
            tokio::time::sleep(RECONNECT_DELAY).await;
        }
    }
}

impl Integration for HueIntegration {
    fn owns(&self, entity_id: &str) -> bool {
        self.inner().by_entity.contains_key(entity_id)
    }

    fn call_light<'a>(
        &'a self,
        entity_id: &'a str,
        action: &'a LightAction,
    ) -> BoxFut<'a, Result<(), String>> {
        Box::pin(async move {
            let uuid = self
                .inner()
                .by_entity
                .get(entity_id)
                .cloned()
                .ok_or_else(|| format!("unknown light {entity_id}"))?;
            match action {
                LightAction::TurnOn { brightness } => {
                    self.client
                        .set_light(&uuid, true, brightness.map(ha_to_pct))
                        .await
                }
                LightAction::TurnOff => self.client.set_light(&uuid, false, None).await,
            }
        })
    }
}

/// Owns the running Hue task so pairing can (re)start it at runtime.
pub struct HueManager {
    core: Arc<Core>,
    task: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

impl HueManager {
    pub fn new(core: Arc<Core>) -> Arc<Self> {
        Arc::new(Self {
            core,
            task: Mutex::new(None),
        })
    }

    pub fn is_running(&self) -> bool {
        self.task
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .is_some()
    }

    pub fn start(&self, base: &str, key: &str) {
        let mut task = self.task.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(old) = task.take() {
            old.abort();
        }
        for id in self.core.entity_ids_with_prefix(ENTITY_PREFIX) {
            self.core.remove_state(&id);
        }
        let integration = HueIntegration::new(HueClient::new(base, key), self.core.clone());
        self.core
            .set_integration(INTEGRATION_NAME, integration.clone());
        *task = Some(tokio::spawn(integration.run()));
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Store;
    use axum::body::Body;
    use axum::extract::{Path, State};
    use axum::routing::{get, post, put};
    use axum::{Json, Router};
    use std::sync::Mutex as StdMutex;

    fn light_json(id: &str, name: &str, on: bool, b: f64) -> Value {
        json!({"id": id, "type": "light", "metadata": {"name": name}, "on": {"on": on}, "dimming": {"brightness": b}})
    }

    #[test]
    fn brightness_conversion() {
        assert_eq!(pct_to_ha(100.0), 255);
        assert_eq!(pct_to_ha(50.0), 128);
        assert_eq!(pct_to_ha(0.0), 1);
        assert_eq!(ha_to_pct(255), 100.0);
        assert_eq!(ha_to_pct(128), 50.2);
        assert_eq!(ha_to_pct(0), 0.0);
    }

    #[test]
    fn light_mapping() {
        let l =
            HueLight::from_resource(&light_json("abcdef12-3456-7890", "Desk", true, 50.0)).unwrap();
        assert_eq!(l.entity_id(), "light.hue_abcdef12");
        assert_eq!(l.ha_state(), "on");
        let a = l.attributes();
        assert_eq!(a["friendly_name"], "Desk");
        assert_eq!(a["brightness"], 128);
        assert_eq!(a.len(), 2, "attributes must stay small");

        let off = HueLight {
            on: false,
            ..l.clone()
        };
        assert!(!off.attributes().contains_key("brightness"));
        assert!(HueLight::from_resource(&json!({"type": "light"})).is_none());
    }

    #[test]
    fn partial_update_merges() {
        let mut l = HueLight::from_resource(&light_json("a1", "Desk", true, 50.0)).unwrap();
        l.apply_update(&json!({"on": {"on": false}}));
        assert!(!l.on);
        assert_eq!(l.brightness, Some(50.0));
        l.apply_update(&json!({"dimming": {"brightness": 10.0}, "metadata": {"name": "Lamp"}}));
        assert_eq!(l.brightness, Some(10.0));
        assert_eq!(l.name, "Lamp");
    }

    #[test]
    fn sse_parser_handles_chunks_comments_and_crlf() {
        let mut p = SseParser::default();
        assert!(p.push(b": hi\n\n").is_empty());
        assert!(p.push(b"id: 1\r\ndata: {\"a\"").is_empty());
        assert_eq!(p.push(b":1}\r\n\r\ndata: x\n\nda"), vec![r#"{"a":1}"#, "x"]);
        assert_eq!(p.push(b"ta: y\ndata: z\n\n"), vec!["y\nz"]);
    }

    // ---- mock bridge

    #[derive(Clone, Default)]
    struct Mock {
        lights: Arc<StdMutex<Vec<Value>>>,
        puts: Arc<StdMutex<Vec<(String, Value)>>>,
        events: Arc<StdMutex<Vec<String>>>,
    }

    async fn mock_server(mock: Mock) -> String {
        async fn list(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.lights.lock().unwrap().clone()}))
        }
        async fn put_light(
            State(m): State<Mock>,
            Path(id): Path<String>,
            Json(b): Json<Value>,
        ) -> Json<Value> {
            m.puts.lock().unwrap().push((id, b));
            Json(json!({"errors": [], "data": []}))
        }
        async fn events(State(m): State<Mock>) -> Body {
            let chunks: Vec<Result<Vec<u8>, std::io::Error>> = m
                .events
                .lock()
                .unwrap()
                .iter()
                .map(|e| Ok(format!("id: 1:0\ndata: {e}\n\n").into_bytes()))
                .collect();
            Body::from_stream(futures_util::stream::iter(chunks))
        }
        async fn pair_ok(Json(b): Json<Value>) -> Json<Value> {
            assert_eq!(b["generateclientkey"], true);
            Json(json!([{"success": {"username": "app-key-1", "clientkey": "x"}}]))
        }
        let app = Router::new()
            .route("/clip/v2/resource/light", get(list))
            .route("/clip/v2/resource/light/{id}", put(put_light))
            .route("/eventstream/clip/v2", get(events))
            .route("/api", post(pair_ok))
            .with_state(mock);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{addr}")
    }

    #[tokio::test]
    async fn sync_control_and_events_against_mock_bridge() {
        let mock = Mock::default();
        *mock.lights.lock().unwrap() = vec![
            light_json("aaaaaaaa-0000", "Desk", true, 40.0),
            light_json("bbbbbbbb-0000", "Shelf", false, 100.0),
        ];
        *mock.events.lock().unwrap() = vec![
            json!([{"type": "update", "data": [{"id": "bbbbbbbb-0000", "type": "light", "on": {"on": true}}]}]).to_string(),
        ];
        let base = mock_server(mock.clone()).await;

        let core = Core::new(Arc::new(Store::open_memory().unwrap()));
        let hue = HueIntegration::new(HueClient::new(&base, "key"), core.clone());
        core.set_integration(INTEGRATION_NAME, hue.clone());

        assert_eq!(hue.sync().await.unwrap(), 2);
        assert_eq!(core.get_state("light.hue_aaaaaaaa").unwrap().state, "on");
        assert_eq!(core.get_state("light.hue_bbbbbbbb").unwrap().state, "off");

        // control
        core.call_light(
            "light.hue_bbbbbbbb",
            &LightAction::TurnOn {
                brightness: Some(255),
            },
        )
        .await
        .unwrap();
        {
            let puts = mock.puts.lock().unwrap();
            assert_eq!(puts[0].0, "bbbbbbbb-0000");
            assert_eq!(
                puts[0].1,
                json!({"on": {"on": true}, "dimming": {"brightness": 100.0}})
            );
        }

        // events: reset to off, then stream applies the update (-> on) and the stream ends
        core.set_state("light.hue_bbbbbbbb", "off", Map::new());
        assert_eq!(
            hue.stream_once().await,
            Err("event stream closed".to_string())
        );
        assert_eq!(core.get_state("light.hue_bbbbbbbb").unwrap().state, "on");

        // a light disappearing from the bridge is removed on resync
        mock.lights.lock().unwrap().pop();
        hue.sync().await.unwrap();
        assert!(core.get_state("light.hue_bbbbbbbb").is_none());
        assert!(!hue.owns("light.hue_bbbbbbbb"));
    }

    #[tokio::test]
    async fn add_event_requests_resync() {
        let core = Core::new(Arc::new(Store::open_memory().unwrap()));
        let hue = HueIntegration::new(HueClient::new("http://127.0.0.1:1", "k"), core);
        let add = json!([{"type": "add", "data": [{"id": "x", "type": "light"}]}]).to_string();
        assert!(hue.apply_event_payload(&add));
        let other =
            json!([{"type": "update", "data": [{"id": "x", "type": "motion"}]}]).to_string();
        assert!(!hue.apply_event_payload(&other));
        assert!(!hue.apply_event_payload("not json"));
    }

    #[tokio::test]
    async fn pairing_success_and_button_not_pressed() {
        let base = mock_server(Mock::default()).await;
        assert_eq!(pair(&base).await, Ok("app-key-1".to_string()));

        async fn pair_wait() -> Json<Value> {
            Json(
                json!([{"error": {"type": 101, "address": "", "description": "link button not pressed"}}]),
            )
        }
        let app = Router::new().route("/api", post(pair_wait));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        assert_eq!(
            pair(&format!("http://{addr}")).await,
            Err(PairError::LinkButtonNotPressed)
        );
    }

    #[test]
    fn bridge_base_forms() {
        assert_eq!(bridge_base("192.168.1.2"), "https://192.168.1.2");
        assert_eq!(bridge_base("http://127.0.0.1:9/"), "http://127.0.0.1:9");
    }
}
