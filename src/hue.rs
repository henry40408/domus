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
const SCENE_PREFIX: &str = "scene.hue_";
const SMART_SCENE_TYPE: &str = "smart_scene";
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
        format!("{ENTITY_PREFIX}{}", short_id(&self.id))
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

#[derive(Debug, Clone, PartialEq)]
pub struct HueScene {
    pub id: String,
    pub name: String,
    /// Name of the room or zone the scene belongs to.
    pub group: Option<String>,
    pub active: bool,
    /// A Hue smart scene (e.g. Natural light): its own resource type and recall action.
    pub smart: bool,
}

fn short_id(uuid: &str) -> String {
    uuid.chars().filter(|c| *c != '-').take(8).collect()
}

impl HueScene {
    /// `groups` maps room/zone uuid to its name.
    pub fn from_resource(v: &Value, groups: &HashMap<String, String>) -> Option<Self> {
        Some(Self {
            id: v.get("id")?.as_str()?.to_string(),
            name: v
                .pointer("/metadata/name")
                .and_then(Value::as_str)
                .unwrap_or("Hue scene")
                .to_string(),
            group: v
                .pointer("/group/rid")
                .and_then(Value::as_str)
                .and_then(|rid| groups.get(rid).cloned()),
            active: scene_active(v).unwrap_or(false),
            smart: v.get("type").and_then(Value::as_str) == Some(SMART_SCENE_TYPE),
        })
    }

    pub fn entity_id(&self) -> String {
        format!("{SCENE_PREFIX}{}", short_id(&self.id))
    }

    pub fn friendly_name(&self) -> String {
        match &self.group {
            Some(g) => format!("{g}: {}", self.name),
            None => self.name.clone(),
        }
    }

    pub fn attributes(&self) -> Map<String, Value> {
        let mut m = Map::new();
        m.insert("friendly_name".into(), json!(self.friendly_name()));
        m
    }
}

/// A scene's `status.active` is `inactive`, `static` or `dynamic_palette`; a smart scene's
/// `state` is `inactive` or `active`.
fn scene_active(v: &Value) -> Option<bool> {
    v.pointer("/status/active")
        .or_else(|| v.get("state"))
        .and_then(Value::as_str)
        .map(|s| s != "inactive")
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

/// A PUT succeeds only with a 2xx status and an empty `errors` list.
async fn check_put(resp: reqwest::Response) -> Result<(), String> {
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

    async fn list_resource(&self, kind: &str) -> Result<Vec<Value>, String> {
        let resp = self
            .http
            .get(format!("{}/clip/v2/resource/{kind}", self.base))
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
            .cloned()
            .unwrap_or_default())
    }

    pub async fn list_lights(&self) -> Result<Vec<HueLight>, String> {
        Ok(self
            .list_resource("light")
            .await?
            .iter()
            .filter_map(HueLight::from_resource)
            .collect())
    }

    /// Scenes and smart scenes with their room/zone names resolved.
    pub async fn list_scenes(&self) -> Result<Vec<HueScene>, String> {
        let mut scenes = self.list_resource("scene").await?;
        // Older bridges may not know smart scenes: keep the plain scenes in that case.
        match self.list_resource(SMART_SCENE_TYPE).await {
            Ok(smart) => scenes.extend(smart),
            Err(e) => warn!("hue: smart scene sync failed: {e}"),
        }
        let mut groups = HashMap::new();
        for kind in ["room", "zone"] {
            for g in self.list_resource(kind).await? {
                if let (Some(id), Some(name)) = (
                    g.get("id").and_then(Value::as_str),
                    g.pointer("/metadata/name").and_then(Value::as_str),
                ) {
                    groups.insert(id.to_string(), name.to_string());
                }
            }
        }
        Ok(scenes
            .iter()
            .filter_map(|s| HueScene::from_resource(s, &groups))
            .collect())
    }

    pub async fn recall_scene(&self, id: &str, smart: bool) -> Result<(), String> {
        let (kind, action) = if smart {
            (SMART_SCENE_TYPE, "activate")
        } else {
            ("scene", "active")
        };
        let resp = self
            .http
            .put(format!("{}/clip/v2/resource/{kind}/{id}", self.base))
            .header("hue-application-key", &self.key)
            .json(&json!({"recall": {"action": action}}))
            .send()
            .await
            .map_err(|e| e.to_string())?;
        check_put(resp).await
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
        check_put(resp).await
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
    scenes: HashMap<String, HueScene>,
    scene_by_entity: HashMap<String, String>,
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
        // Scenes are optional: a failure here must not take the lights down with it.
        let scenes = match self.client.list_scenes().await {
            Ok(scenes) => Some(scenes),
            Err(e) => {
                warn!("hue: scene sync failed: {e}");
                None
            }
        };
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
        if let Some(scenes) = scenes {
            Self::replace_scenes(&self.core, &mut inner, scenes);
        }
        Ok(n)
    }

    /// Replaces the known scenes. A scene's state is its last activation time, so an existing
    /// state survives a resync; new scenes start as `unknown`.
    fn replace_scenes(core: &Core, inner: &mut Inner, scenes: Vec<HueScene>) {
        let old: HashSet<String> = inner.scene_by_entity.keys().cloned().collect();
        inner.scenes.clear();
        inner.scene_by_entity.clear();
        for s in scenes {
            let eid = s.entity_id();
            let state = core
                .get_state(&eid)
                .map_or_else(|| "unknown".to_string(), |old| old.state);
            core.set_state(&eid, &state, s.attributes());
            inner.scene_by_entity.insert(eid, s.id.clone());
            inner.scenes.insert(s.id.clone(), s);
        }
        for gone in old
            .iter()
            .filter(|e| !inner.scene_by_entity.contains_key(*e))
        {
            core.remove_state(gone);
        }
    }

    /// A scene turning active (e.g. from the Hue app) counts as an activation.
    fn apply_scene_update(&self, inner: &mut Inner, item: &Value) {
        let Some(scene) = item
            .get("id")
            .and_then(Value::as_str)
            .and_then(|id| inner.scenes.get_mut(id))
        else {
            return;
        };
        let eid = scene.entity_id();
        if let Some(n) = item.pointer("/metadata/name").and_then(Value::as_str) {
            scene.name = n.to_string();
            if let Some(old) = self.core.get_state(&eid) {
                self.core.set_state(&eid, &old.state, scene.attributes());
            }
        }
        if let Some(active) = scene_active(item) {
            if active && !scene.active {
                self.core.mark_scene_activated(&eid);
            }
            scene.active = active;
        }
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
                let item_type = item.get("type").and_then(Value::as_str);
                if matches!(item_type, Some("scene" | SMART_SCENE_TYPE)) {
                    match kind {
                        "update" => self.apply_scene_update(&mut inner, item),
                        "add" | "delete" => resync = true,
                        _ => {}
                    }
                    continue;
                }
                if item_type != Some("light") {
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
        self.run_from(true).await;
    }

    /// Syncs (unless `first_sync` is false), then follows the event stream forever.
    async fn run_from(self: Arc<Self>, mut first_sync: bool) {
        loop {
            if first_sync {
                match self.sync().await {
                    Ok(n) => info!("hue: synced {n} lights"),
                    Err(e) => warn!("hue: sync failed: {e}"),
                }
            }
            first_sync = true;
            if let Err(e) = self.stream_once().await {
                warn!("hue: {e}");
            }
            tokio::time::sleep(RECONNECT_DELAY).await;
        }
    }
}

impl Integration for HueIntegration {
    fn owns(&self, entity_id: &str) -> bool {
        let inner = self.inner();
        inner.by_entity.contains_key(entity_id) || inner.scene_by_entity.contains_key(entity_id)
    }

    fn activate_scene<'a>(&'a self, entity_id: &'a str) -> BoxFut<'a, Result<(), String>> {
        Box::pin(async move {
            let (uuid, smart) = {
                let inner = self.inner();
                let uuid = inner
                    .scene_by_entity
                    .get(entity_id)
                    .ok_or_else(|| format!("unknown scene {entity_id}"))?;
                let smart = inner.scenes.get(uuid).is_some_and(|s| s.smart);
                (uuid.clone(), smart)
            };
            self.client.recall_scene(&uuid, smart).await
        })
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
        self.reset();
        self.start_inner(base, key, None);
    }

    /// Like `start`, but finishes the first sync before returning so callers see the lights.
    pub async fn start_synced(&self, base: &str, key: &str) {
        self.reset();
        let integration = HueIntegration::new(HueClient::new(base, key), self.core.clone());
        match integration.sync().await {
            Ok(n) => info!("hue: synced {n} lights"),
            Err(e) => warn!("hue: sync failed: {e}"),
        }
        self.start_inner(base, key, Some(integration));
    }

    /// Stops the running sync task and forgets every Hue entity.
    fn reset(&self) {
        let mut task = self.task.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(old) = task.take() {
            old.abort();
        }
        for prefix in [ENTITY_PREFIX, SCENE_PREFIX] {
            for id in self.core.entity_ids_with_prefix(prefix) {
                self.core.remove_state(&id);
            }
        }
    }

    fn start_inner(&self, base: &str, key: &str, synced: Option<Arc<HueIntegration>>) {
        let mut task = self.task.lock().unwrap_or_else(|e| e.into_inner());
        if let Some(old) = task.take() {
            old.abort();
        }
        let first_sync = synced.is_none();
        let integration = synced
            .unwrap_or_else(|| HueIntegration::new(HueClient::new(base, key), self.core.clone()));
        self.core
            .set_integration(INTEGRATION_NAME, integration.clone());
        *task = Some(tokio::spawn(integration.run_from(first_sync)));
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

    #[tokio::test]
    async fn brightness_conversion() {
        assert_eq!(pct_to_ha(100.0), 255);
        assert_eq!(pct_to_ha(50.0), 128);
        assert_eq!(pct_to_ha(0.0), 1);
        assert_eq!(ha_to_pct(255), 100.0);
        assert_eq!(ha_to_pct(128), 50.2);
        assert_eq!(ha_to_pct(0), 0.0);
    }

    #[tokio::test]
    async fn light_mapping() {
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

    #[tokio::test]
    async fn partial_update_merges() {
        let mut l = HueLight::from_resource(&light_json("a1", "Desk", true, 50.0)).unwrap();
        l.apply_update(&json!({"on": {"on": false}}));
        assert!(!l.on);
        assert_eq!(l.brightness, Some(50.0));
        l.apply_update(&json!({"dimming": {"brightness": 10.0}, "metadata": {"name": "Lamp"}}));
        assert_eq!(l.brightness, Some(10.0));
        assert_eq!(l.name, "Lamp");
    }

    #[tokio::test]
    async fn sse_parser_handles_chunks_comments_and_crlf() {
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
        scenes: Arc<StdMutex<Vec<Value>>>,
        smart_scenes: Arc<StdMutex<Vec<Value>>>,
        rooms: Arc<StdMutex<Vec<Value>>>,
        puts: Arc<StdMutex<Vec<(String, Value)>>>,
        events: Arc<StdMutex<Vec<String>>>,
    }

    async fn mock_server(mock: Mock) -> String {
        async fn list(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.lights.lock().unwrap().clone()}))
        }
        async fn list_scenes(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.scenes.lock().unwrap().clone()}))
        }
        async fn list_smart_scenes(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.smart_scenes.lock().unwrap().clone()}))
        }
        async fn list_rooms(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.rooms.lock().unwrap().clone()}))
        }
        async fn list_zones() -> Json<Value> {
            Json(json!({"errors": [], "data": []}))
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
            .route("/clip/v2/resource/scene", get(list_scenes))
            .route("/clip/v2/resource/scene/{id}", put(put_light))
            .route("/clip/v2/resource/smart_scene", get(list_smart_scenes))
            .route("/clip/v2/resource/smart_scene/{id}", put(put_light))
            .route("/clip/v2/resource/room", get(list_rooms))
            .route("/clip/v2/resource/zone", get(list_zones))
            .route("/eventstream/clip/v2", get(events))
            .route("/api", post(pair_ok))
            .with_state(mock);
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{addr}")
    }

    #[tokio::test]
    async fn start_synced_has_the_lights_ready_on_return() {
        let mock = Mock::default();
        *mock.lights.lock().unwrap() = vec![light_json("aaaaaaaa-0000", "Desk", true, 40.0)];
        let base = mock_server(mock).await;
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let manager = HueManager::new(core.clone());
        manager.start_synced(&base, "key").await;
        assert!(manager.is_running());
        assert_eq!(core.get_state("light.hue_aaaaaaaa").unwrap().state, "on");
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

        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
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

    fn scene_json(id: &str, name: &str, room: &str, active: &str) -> Value {
        json!({"id": id, "type": "scene", "metadata": {"name": name},
               "group": {"rid": room, "rtype": "room"}, "status": {"active": active}})
    }

    fn smart_scene_json(id: &str, name: &str, room: &str, state: &str) -> Value {
        json!({"id": id, "type": "smart_scene", "metadata": {"name": name},
               "group": {"rid": room, "rtype": "room"}, "state": state})
    }

    #[tokio::test]
    async fn smart_scenes_sync_recall_and_events() {
        let mock = Mock::default();
        *mock.rooms.lock().unwrap() =
            vec![json!({"id": "room-1", "type": "room", "metadata": {"name": "Living Room"}})];
        *mock.scenes.lock().unwrap() =
            vec![scene_json("cccccccc-0000", "Relax", "room-1", "inactive")];
        *mock.smart_scenes.lock().unwrap() = vec![smart_scene_json(
            "dddddddd-0000",
            "Natural light",
            "room-1",
            "inactive",
        )];
        let base = mock_server(mock.clone()).await;

        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let hue = HueIntegration::new(HueClient::new(&base, "key"), core.clone());
        core.set_integration(INTEGRATION_NAME, hue.clone());
        hue.sync().await.unwrap();

        let id = "scene.hue_dddddddd";
        let s = core.get_state(id).unwrap();
        assert_eq!(s.state, "unknown");
        assert_eq!(s.attributes["friendly_name"], "Living Room: Natural light");
        assert!(hue.owns(id));
        assert!(core.get_state("scene.hue_cccccccc").is_some());

        // recall uses the smart scene action and leaves plain scenes alone
        core.activate_scene(id).await.unwrap();
        {
            let puts = mock.puts.lock().unwrap();
            assert_eq!(puts[0].0, "dddddddd-0000");
            assert_eq!(puts[0].1, json!({"recall": {"action": "activate"}}));
        }
        assert!(core.get_state(id).unwrap().state.contains('T'));

        // activation from the Hue app (inactive -> active) moves the stamp
        core.set_state(id, "unknown", Map::new());
        let ev = |v: &str| {
            json!([{"type": "update", "data": [{"id": "dddddddd-0000", "type": "smart_scene", "state": v}]}])
                .to_string()
        };
        assert!(!hue.apply_event_payload(&ev("active")));
        assert!(core.get_state(id).unwrap().state.contains('T'));

        // add/delete asks for a resync; removal drops the entity
        let del =
            json!([{"type": "delete", "data": [{"id": "x", "type": "smart_scene"}]}]).to_string();
        assert!(hue.apply_event_payload(&del));
        mock.smart_scenes.lock().unwrap().clear();
        hue.sync().await.unwrap();
        assert!(core.get_state(id).is_none());
        assert!(core.get_state("scene.hue_cccccccc").is_some());
    }

    #[tokio::test]
    async fn scene_mapping() {
        let groups = HashMap::from([("room-1".to_string(), "Living Room".to_string())]);
        let s = HueScene::from_resource(
            &scene_json("12345678-aaaa", "Relax", "room-1", "inactive"),
            &groups,
        )
        .unwrap();
        assert_eq!(s.entity_id(), "scene.hue_12345678");
        assert_eq!(s.friendly_name(), "Living Room: Relax");
        assert!(!s.active);
        let orphan =
            HueScene::from_resource(&scene_json("a", "Solo", "gone", "static"), &groups).unwrap();
        assert_eq!(orphan.friendly_name(), "Solo");
        assert!(orphan.active);
    }

    #[tokio::test]
    async fn scenes_sync_recall_and_events() {
        let mock = Mock::default();
        *mock.rooms.lock().unwrap() =
            vec![json!({"id": "room-1", "type": "room", "metadata": {"name": "Living Room"}})];
        *mock.scenes.lock().unwrap() =
            vec![scene_json("cccccccc-0000", "Relax", "room-1", "inactive")];
        let base = mock_server(mock.clone()).await;

        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let hue = HueIntegration::new(HueClient::new(&base, "key"), core.clone());
        core.set_integration(INTEGRATION_NAME, hue.clone());
        hue.sync().await.unwrap();

        let id = "scene.hue_cccccccc";
        let s = core.get_state(id).unwrap();
        assert_eq!(s.state, "unknown");
        assert_eq!(s.attributes["friendly_name"], "Living Room: Relax");
        assert!(hue.owns(id));

        // recall: sends the recall body and stamps the activation time
        core.activate_scene(id).await.unwrap();
        {
            let puts = mock.puts.lock().unwrap();
            assert_eq!(puts[0].0, "cccccccc-0000");
            assert_eq!(puts[0].1, json!({"recall": {"action": "active"}}));
        }
        let stamp = core.get_state(id).unwrap().state;
        assert!(stamp.contains('T'), "timestamp expected, got {stamp}");

        // the stamp survives a resync
        hue.sync().await.unwrap();
        assert_eq!(core.get_state(id).unwrap().state, stamp);

        // activation from the Hue app (inactive -> static) moves the stamp; staying active does not
        core.set_state(id, "unknown", Map::new());
        let ev = |v: &str| {
            json!([{"type": "update", "data": [{"id": "cccccccc-0000", "type": "scene", "status": {"active": v}}]}])
                .to_string()
        };
        assert!(!hue.apply_event_payload(&ev("static")));
        let after = core.get_state(id).unwrap().state;
        assert!(after.contains('T'));
        core.set_state(id, "unknown", Map::new());
        hue.apply_event_payload(&ev("dynamic_palette"));
        assert_eq!(core.get_state(id).unwrap().state, "unknown");
        hue.apply_event_payload(&ev("inactive"));
        hue.apply_event_payload(&ev("static"));
        assert!(core.get_state(id).unwrap().state.contains('T'));

        // add/delete of scenes asks for a resync; removal drops the entity
        let del = json!([{"type": "delete", "data": [{"id": "x", "type": "scene"}]}]).to_string();
        assert!(hue.apply_event_payload(&del));
        mock.scenes.lock().unwrap().clear();
        hue.sync().await.unwrap();
        assert!(core.get_state(id).is_none());
        assert!(!hue.owns(id));
    }

    #[tokio::test]
    async fn add_event_requests_resync() {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
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

    #[tokio::test]
    async fn bridge_base_forms() {
        assert_eq!(bridge_base("192.168.1.2"), "https://192.168.1.2");
        assert_eq!(bridge_base("http://127.0.0.1:9/"), "http://127.0.0.1:9");
    }
}
