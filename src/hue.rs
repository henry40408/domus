//! Philips Hue Bridge integration over the CLIP v2 API.
//!
//! The bridge serves HTTPS with a certificate signed by a private CA, so certificate
//! verification is disabled for bridge connections only (LAN traffic to a fixed address).

use futures_util::StreamExt;
use serde_json::{Map, Value, json};
use std::collections::{BTreeSet, HashMap, HashSet};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::{info, warn};

use crate::core::{BoxFut, Core, Integration, LightAction, LightSet};

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
    /// The device this light belongs to; `zigbee_connectivity` is reported per device.
    pub owner: Option<String>,
    /// False when the bridge cannot reach the light (unplugged, out of range).
    pub reachable: bool,
    /// Has a `color_temperature` capability.
    pub supports_ct: bool,
    /// Has a `color` capability.
    pub supports_color: bool,
    /// Color temperature in mireds while the light is in white mode.
    pub mirek: Option<u16>,
    /// CIE xy color.
    pub xy: Option<(f64, f64)>,
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
            owner: v
                .pointer("/owner/rid")
                .and_then(Value::as_str)
                .map(str::to_string),
            reachable: true,
            supports_ct: v.get("color_temperature").is_some_and(Value::is_object),
            supports_color: v.get("color").is_some_and(Value::is_object),
            mirek: mirek_of(v),
            xy: xy_of(v),
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
        if v.get("color_temperature").is_some() {
            self.mirek = mirek_of(v);
        } else if v.get("color").is_some() {
            // Only the color changed: the light left white mode.
            self.mirek = None;
        }
        if let Some(xy) = xy_of(v) {
            self.xy = Some(xy);
        }
    }

    /// Stable id derived from the Hue resource uuid, so renames never break groups.
    pub fn entity_id(&self) -> String {
        format!("{ENTITY_PREFIX}{}", short_id(&self.id))
    }

    pub fn ha_state(&self) -> &'static str {
        if !self.reachable {
            "unavailable"
        } else if self.on {
            "on"
        } else {
            "off"
        }
    }

    /// Deliberately small: constrained clients (watches) must parse the whole payload.
    pub fn attributes(&self) -> Map<String, Value> {
        let mut m = Map::new();
        m.insert("friendly_name".into(), json!(self.name));
        if !self.reachable {
            return m;
        }
        let mut modes = vec!["brightness"];
        if self.supports_ct {
            modes.push("color_temp");
        }
        if self.supports_color {
            modes.push("xy");
        }
        m.insert("supported_color_modes".into(), json!(modes));
        if !self.on {
            return m;
        }
        if let Some(b) = self.brightness {
            m.insert("brightness".into(), json!(pct_to_ha(b)));
        }
        match (self.mirek, self.xy) {
            (Some(mirek), _) if self.supports_ct => {
                m.insert("color_mode".into(), json!("color_temp"));
                m.insert("color_temp".into(), json!(mirek));
            }
            (_, Some((x, y))) if self.supports_color => {
                m.insert("color_mode".into(), json!("xy"));
                m.insert("xy_color".into(), json!([x, y]));
            }
            _ => {}
        }
        m
    }

    /// The bridge body for an action, leaving out what this light cannot do (a color for a
    /// white-only bulb would otherwise fail the whole request).
    pub fn request_body(&self, action: &LightAction) -> Value {
        light_body(action, self.supports_ct, self.supports_color)
    }
}

/// `color_temperature.mirek`, unless the bridge marks it stale (the light is showing a color).
fn mirek_of(v: &Value) -> Option<u16> {
    if v.pointer("/color_temperature/mirek_valid")
        .and_then(Value::as_bool)
        == Some(false)
    {
        return None;
    }
    v.pointer("/color_temperature/mirek")
        .and_then(Value::as_u64)
        .and_then(|m| u16::try_from(m).ok())
}

fn xy_of(v: &Value) -> Option<(f64, f64)> {
    Some((
        v.pointer("/color/xy/x").and_then(Value::as_f64)?,
        v.pointer("/color/xy/y").and_then(Value::as_f64)?,
    ))
}

/// The CLIP v2 body for an action on a light, or on a `grouped_light`.
fn light_body(action: &LightAction, supports_ct: bool, supports_color: bool) -> Value {
    let (mut body, transition) = match action {
        LightAction::TurnOn(p) => {
            let mut body = json!({"on": {"on": true}});
            if let Some(b) = p.brightness {
                body["dimming"] = json!({"brightness": ha_to_pct(b)});
            }
            if let Some(m) = p.color_temp
                && supports_ct
            {
                body["color_temperature"] = json!({"mirek": m});
            }
            if let Some((x, y)) = p.xy
                && supports_color
            {
                body["color"] = json!({"xy": {"x": x, "y": y}});
            }
            (body, p.transition_ms)
        }
        LightAction::TurnOff { transition_ms } => (json!({"on": {"on": false}}), *transition_ms),
    };
    if let Some(ms) = transition {
        body["dynamics"] = json!({"duration": ms});
    }
    body
}

/// A Hue room or zone, whose lights can be driven together through its `grouped_light`.
#[derive(Debug, Clone, PartialEq)]
pub struct HueRoom {
    pub name: String,
    /// "room" or "zone".
    pub kind: String,
    /// uuid of the room's `grouped_light` service.
    pub grouped_light: String,
    /// Rooms list their devices, zones their lights; resolved to light uuids on sync.
    pub children: Vec<(String, String)>,
}

impl HueRoom {
    pub fn from_resource(v: &Value, kind: &str) -> Option<Self> {
        let rids = |key: &str| -> Vec<(String, String)> {
            v.get(key)
                .and_then(Value::as_array)
                .into_iter()
                .flatten()
                .filter_map(|c| {
                    Some((
                        c.get("rtype")?.as_str()?.to_string(),
                        c.get("rid")?.as_str()?.to_string(),
                    ))
                })
                .collect()
        };
        let grouped_light = rids("services")
            .into_iter()
            .find(|(t, _)| t == "grouped_light")?
            .1;
        Some(Self {
            name: v.pointer("/metadata/name")?.as_str()?.to_string(),
            kind: kind.to_string(),
            grouped_light,
            children: rids("children"),
        })
    }

    /// uuids of the lights in this room. `owners` maps a device uuid to its lights.
    pub fn light_ids(&self, owners: &HashMap<String, Vec<String>>) -> BTreeSet<String> {
        let mut out = BTreeSet::new();
        for (kind, rid) in &self.children {
            match kind.as_str() {
                "light" => {
                    out.insert(rid.clone());
                }
                "device" => out.extend(owners.get(rid).into_iter().flatten().cloned()),
                _ => {}
            }
        }
        out
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

    /// Rooms and zones that have a `grouped_light`.
    pub async fn list_rooms(&self) -> Result<Vec<HueRoom>, String> {
        let mut out = Vec::new();
        for kind in ["room", "zone"] {
            out.extend(
                self.list_resource(kind)
                    .await?
                    .iter()
                    .filter_map(|r| HueRoom::from_resource(r, kind)),
            );
        }
        Ok(out)
    }

    /// Maps each `zigbee_connectivity` id to its device and whether the device is reachable.
    pub async fn list_connectivity(&self) -> Result<HashMap<String, (String, bool)>, String> {
        Ok(self
            .list_resource("zigbee_connectivity")
            .await?
            .iter()
            .filter_map(connectivity_of)
            .collect())
    }

    async fn put_resource(&self, kind: &str, id: &str, body: &Value) -> Result<(), String> {
        let resp = self
            .http
            .put(format!("{}/clip/v2/resource/{kind}/{id}", self.base))
            .header("hue-application-key", &self.key)
            .json(body)
            .send()
            .await
            .map_err(|e| e.to_string())?;
        check_put(resp).await
    }

    pub async fn set_light(&self, id: &str, body: &Value) -> Result<(), String> {
        self.put_resource("light", id, body).await
    }

    pub async fn set_grouped_light(&self, id: &str, body: &Value) -> Result<(), String> {
        self.put_resource("grouped_light", id, body).await
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

/// `(zigbee_connectivity id, (device id, reachable))`. Only a disconnected or troubled link
/// counts as unreachable; `unidirectional_incoming` still carries commands.
fn connectivity_of(v: &Value) -> Option<(String, (String, bool))> {
    let id = v.get("id")?.as_str()?.to_string();
    let device = v.pointer("/owner/rid")?.as_str()?.to_string();
    let status = v.get("status")?.as_str()?;
    Some((
        id,
        (
            device,
            !matches!(status, "disconnected" | "connectivity_issue"),
        ),
    ))
}

#[derive(Default)]
struct Inner {
    /// zigbee_connectivity id -> (device id, reachable)
    connectivity: HashMap<String, (String, bool)>,
    /// Rooms and zones with their lights resolved.
    rooms: Vec<(HueRoom, BTreeSet<String>)>,
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
        // Reachability and rooms are optional extras: without them lights still work.
        let connectivity = self.client.list_connectivity().await.unwrap_or_else(|e| {
            warn!("hue: connectivity sync failed: {e}");
            HashMap::new()
        });
        let rooms = self.client.list_rooms().await.unwrap_or_else(|e| {
            warn!("hue: room sync failed: {e}");
            Vec::new()
        });
        let mut owners: HashMap<String, Vec<String>> = HashMap::new();
        for l in &lights {
            if let Some(o) = &l.owner {
                owners.entry(o.clone()).or_default().push(l.id.clone());
            }
        }
        let mut inner = self.inner();
        inner.rooms = rooms
            .into_iter()
            .map(|r| {
                let ids = r.light_ids(&owners);
                (r, ids)
            })
            .collect();
        inner.connectivity = connectivity;
        let old: HashSet<String> = inner.by_entity.keys().cloned().collect();
        inner.lights.clear();
        inner.by_entity.clear();
        for mut l in lights {
            l.reachable = Self::device_reachable(&inner.connectivity, l.owner.as_deref());
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

    /// Lights are reachable unless the bridge says their device's link is down.
    fn device_reachable(
        connectivity: &HashMap<String, (String, bool)>,
        device: Option<&str>,
    ) -> bool {
        device.is_none_or(|d| {
            connectivity
                .values()
                .find(|(dev, _)| dev == d)
                .is_none_or(|(_, ok)| *ok)
        })
    }

    /// A `zigbee_connectivity` update: flips every light of that device.
    fn apply_connectivity_update(&self, inner: &mut Inner, item: &Value) {
        let Some(id) = item.get("id").and_then(Value::as_str) else {
            return;
        };
        let Some(status) = item.get("status").and_then(Value::as_str) else {
            return;
        };
        let reachable = !matches!(status, "disconnected" | "connectivity_issue");
        let device = match inner.connectivity.get_mut(id) {
            Some((device, ok)) => {
                *ok = reachable;
                device.clone()
            }
            None => return,
        };
        for light in inner
            .lights
            .values_mut()
            .filter(|l| l.owner.as_deref() == Some(device.as_str()))
        {
            light.reachable = reachable;
            self.core
                .set_state(&light.entity_id(), light.ha_state(), light.attributes());
        }
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
                if item_type == Some("zigbee_connectivity") {
                    match kind {
                        "update" => self.apply_connectivity_update(&mut inner, item),
                        "add" | "delete" => resync = true,
                        _ => {}
                    }
                    continue;
                }
                if matches!(item_type, Some("room" | "zone")) && matches!(kind, "add" | "delete") {
                    resync = true;
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
            let body = self.inner().lights.get(&uuid).map_or_else(
                || light_body(action, false, false),
                |l| l.request_body(action),
            );
            self.client.set_light(&uuid, &body).await
        })
    }

    fn light_sets(&self) -> Vec<LightSet> {
        let inner = self.inner();
        inner
            .rooms
            .iter()
            .map(|(room, ids)| LightSet {
                name: room.name.clone(),
                kind: room.kind.clone(),
                entity_ids: ids
                    .iter()
                    .filter_map(|id| inner.lights.get(id).map(HueLight::entity_id))
                    .collect(),
            })
            .collect()
    }

    fn call_light_set<'a>(
        &'a self,
        entity_ids: &'a [String],
        action: &'a LightAction,
    ) -> BoxFut<'a, Option<Result<(), String>>> {
        Box::pin(async move {
            let target = {
                let inner = self.inner();
                let wanted: Option<BTreeSet<String>> = entity_ids
                    .iter()
                    .map(|e| inner.by_entity.get(e).cloned())
                    .collect();
                let wanted = wanted.filter(|w| !w.is_empty())?;
                // Only a room or zone whose lights are exactly these can stand in for them.
                inner
                    .rooms
                    .iter()
                    .find(|(_, ids)| *ids == wanted)
                    .map(|(room, _)| room.grouped_light.clone())?
            };
            let body = light_body(action, true, true);
            Some(self.client.set_grouped_light(&target, &body).await)
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
    use crate::core::TurnOn;
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
        assert_eq!(a["supported_color_modes"], json!(["brightness"]));
        assert_eq!(a.len(), 3, "attributes must stay small");

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
        connectivity: Arc<StdMutex<Vec<Value>>>,
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
        async fn list_connectivity(State(m): State<Mock>) -> Json<Value> {
            Json(json!({"errors": [], "data": m.connectivity.lock().unwrap().clone()}))
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
            .route(
                "/clip/v2/resource/zigbee_connectivity",
                get(list_connectivity),
            )
            .route("/clip/v2/resource/grouped_light/{id}", put(put_light))
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
            &LightAction::TurnOn(TurnOn {
                brightness: Some(255),
                ..TurnOn::default()
            }),
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

    fn rich_light_json(id: &str, owner: &str) -> Value {
        json!({"id": id, "type": "light", "metadata": {"name": id}, "on": {"on": true},
               "dimming": {"brightness": 50.0}, "owner": {"rid": owner, "rtype": "device"},
               "color_temperature": {"mirek": 300, "mirek_valid": true},
               "color": {"xy": {"x": 0.5, "y": 0.4}}})
    }

    #[tokio::test]
    async fn color_state_and_request_bodies() {
        let mut l = HueLight::from_resource(&rich_light_json("aaaa1111-0", "dev-a")).unwrap();
        assert_eq!(l.attributes()["color_mode"], "color_temp");
        assert_eq!(l.attributes()["color_temp"], 300);
        assert_eq!(
            l.attributes()["supported_color_modes"],
            json!(["brightness", "color_temp", "xy"])
        );

        // a color change leaves white mode, a temperature change comes back
        l.apply_update(&json!({"color": {"xy": {"x": 0.1, "y": 0.2}}}));
        assert_eq!(l.attributes()["color_mode"], "xy");
        assert_eq!(l.attributes()["xy_color"], json!([0.1, 0.2]));
        assert!(!l.attributes().contains_key("color_temp"));
        l.apply_update(&json!({"color_temperature": {"mirek": 200, "mirek_valid": true}}));
        assert_eq!(l.attributes()["color_temp"], 200);
        // a stale mirek (light showing a color) is ignored
        l.apply_update(&json!({"color_temperature": {"mirek": 200, "mirek_valid": false}}));
        assert_eq!(l.attributes()["color_mode"], "xy");

        let on = LightAction::TurnOn(TurnOn {
            brightness: Some(255),
            color_temp: Some(250),
            xy: Some((0.3, 0.3)),
            transition_ms: Some(1500),
        });
        assert_eq!(
            l.request_body(&on),
            json!({"on": {"on": true}, "dimming": {"brightness": 100.0},
                   "color_temperature": {"mirek": 250}, "color": {"xy": {"x": 0.3, "y": 0.3}},
                   "dynamics": {"duration": 1500}})
        );
        // a dim-only light gets neither color field
        let white = HueLight::from_resource(&light_json("w1", "Bulb", true, 50.0)).unwrap();
        assert_eq!(
            white.request_body(&on),
            json!({"on": {"on": true}, "dimming": {"brightness": 100.0},
                   "dynamics": {"duration": 1500}})
        );
        assert_eq!(
            white.request_body(&LightAction::TurnOff {
                transition_ms: Some(400)
            }),
            json!({"on": {"on": false}, "dynamics": {"duration": 400}})
        );
    }

    #[tokio::test]
    async fn unreachable_lights_are_unavailable_until_the_link_returns() {
        let mock = Mock::default();
        *mock.lights.lock().unwrap() = vec![rich_light_json("aaaa1111-0", "dev-a")];
        *mock.connectivity.lock().unwrap() = vec![
            json!({"id": "z1", "type": "zigbee_connectivity", "status": "disconnected",
                   "owner": {"rid": "dev-a", "rtype": "device"}}),
        ];
        *mock.events.lock().unwrap() = vec![
            json!([{"type": "update", "data": [{"id": "z1", "type": "zigbee_connectivity",
                    "status": "connected"}]}])
            .to_string(),
        ];
        let base = mock_server(mock).await;
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let hue = HueIntegration::new(HueClient::new(&base, "key"), core.clone());
        core.set_integration(INTEGRATION_NAME, hue.clone());

        hue.sync().await.unwrap();
        let s = core.get_state("light.hue_aaaa1111").unwrap();
        assert_eq!(s.state, "unavailable");
        assert_eq!(s.attributes.len(), 1, "no stale brightness or color");

        let _ = hue.stream_once().await;
        assert_eq!(core.get_state("light.hue_aaaa1111").unwrap().state, "on");
    }

    #[tokio::test]
    async fn a_room_is_driven_through_its_grouped_light() {
        let mock = Mock::default();
        *mock.lights.lock().unwrap() = vec![
            rich_light_json("aaaa1111-0", "dev-a"),
            rich_light_json("bbbb2222-0", "dev-b"),
            rich_light_json("cccc3333-0", "dev-c"),
        ];
        *mock.rooms.lock().unwrap() = vec![json!({
            "id": "room1", "type": "room", "metadata": {"name": "Study"},
            "children": [{"rid": "dev-a", "rtype": "device"}, {"rid": "dev-b", "rtype": "device"}],
            "services": [{"rid": "gl-study", "rtype": "grouped_light"}]})];
        let base = mock_server(mock.clone()).await;
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let hue = HueIntegration::new(HueClient::new(&base, "key"), core.clone());
        core.set_integration(INTEGRATION_NAME, hue.clone());
        hue.sync().await.unwrap();

        let sets = core.light_sets();
        assert_eq!(sets.len(), 1);
        assert_eq!(sets[0].name, "Study");
        assert_eq!(
            sets[0].entity_ids,
            vec!["light.hue_aaaa1111", "light.hue_bbbb2222"]
        );

        // exactly the room's lights: one request to the grouped_light
        core.call_lights(&sets[0].entity_ids, &LightAction::off())
            .await
            .unwrap();
        {
            let puts = mock.puts.lock().unwrap();
            assert_eq!(puts.len(), 1);
            assert_eq!(puts[0].0, "gl-study");
            assert_eq!(puts[0].1, json!({"on": {"on": false}}));
        }
        assert_eq!(core.get_state("light.hue_bbbb2222").unwrap().state, "off");

        // any other combination goes light by light
        mock.puts.lock().unwrap().clear();
        let two = vec![
            "light.hue_aaaa1111".to_string(),
            "light.hue_cccc3333".to_string(),
        ];
        core.call_lights(&two, &LightAction::on()).await.unwrap();
        assert_eq!(mock.puts.lock().unwrap().len(), 2);
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
