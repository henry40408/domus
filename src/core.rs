//! State machine, integration trait and group synthesis.

use serde::Serialize;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, RwLock};
use std::time::SystemTime;
use tokio::sync::broadcast;

use crate::store::{Scope, Store};
use crate::util::{random_hex, rfc3339_micros};

/// Entity id prefix of the light that stands for a whole group (see `Store::group_exposed`).
pub const GROUP_LIGHT_PREFIX: &str = "light.domus_group_";

pub fn group_light_id(name: &str) -> String {
    format!("{GROUP_LIGHT_PREFIX}{name}")
}

pub type BoxFut<'a, T> = Pin<Box<dyn Future<Output = T> + Send + 'a>>;

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct Context {
    pub id: String,
    pub parent_id: Option<String>,
    pub user_id: Option<String>,
}

impl Context {
    pub fn new() -> Self {
        Self {
            id: random_hex(13),
            parent_id: None,
            user_id: None,
        }
    }
}

impl Default for Context {
    fn default() -> Self {
        Self::new()
    }
}

#[derive(Debug, Clone, Serialize, PartialEq)]
pub struct State {
    pub entity_id: String,
    pub state: String,
    pub attributes: Map<String, Value>,
    pub last_changed: String,
    pub last_updated: String,
    pub context: Context,
}

/// What a `turn_on` asks for; unset fields leave the light as it is.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct TurnOn {
    /// Home Assistant brightness, 0-255.
    pub brightness: Option<u8>,
    /// Color temperature in mireds (Hue's `mirek`).
    pub color_temp: Option<u16>,
    /// CIE xy chromaticity.
    pub xy: Option<(f64, f64)>,
    /// Fade time in milliseconds.
    pub transition_ms: Option<u32>,
}

#[derive(Debug, Clone, PartialEq)]
pub enum LightAction {
    TurnOn(TurnOn),
    TurnOff { transition_ms: Option<u32> },
}

impl LightAction {
    pub fn on() -> Self {
        Self::TurnOn(TurnOn::default())
    }

    pub fn off() -> Self {
        Self::TurnOff {
            transition_ms: None,
        }
    }
}

/// Lights an integration can address together in one request, such as a Hue room or zone.
#[derive(Debug, Clone, PartialEq, Serialize)]
pub struct LightSet {
    pub name: String,
    /// "room" or "zone".
    pub kind: String,
    pub entity_ids: Vec<String>,
}

/// A source of entities that can also execute service calls for them.
pub trait Integration: Send + Sync {
    fn owns(&self, entity_id: &str) -> bool;
    fn call_light<'a>(
        &'a self,
        entity_id: &'a str,
        action: &'a LightAction,
    ) -> BoxFut<'a, Result<(), String>>;

    /// The light sets this integration can control with a single request.
    fn light_sets(&self) -> Vec<LightSet> {
        Vec::new()
    }

    /// Runs one action on several lights in a single request when the integration can (e.g. a
    /// Hue room). `None` means it cannot, and the caller goes light by light.
    fn call_light_set<'a>(
        &'a self,
        _entity_ids: &'a [String],
        _action: &'a LightAction,
    ) -> BoxFut<'a, Option<Result<(), String>>> {
        Box::pin(async { None })
    }

    /// Recalls a scene. Integrations without scenes keep the default.
    fn activate_scene<'a>(&'a self, _entity_id: &'a str) -> BoxFut<'a, Result<(), String>> {
        Box::pin(async { Err("scenes are not supported".to_string()) })
    }
}

#[derive(Debug, PartialEq)]
pub enum CallError {
    NoIntegration,
    Failed(String),
}

pub struct Core {
    states: RwLock<HashMap<String, State>>,
    integrations: RwLock<HashMap<&'static str, Arc<dyn Integration>>>,
    store: Arc<Store>,
    /// Entity ids whose state changed or vanished, for the admin page's live updates.
    changes: broadcast::Sender<String>,
}

impl Core {
    pub fn new(store: Arc<Store>) -> Arc<Self> {
        Arc::new(Self {
            states: RwLock::new(HashMap::new()),
            integrations: RwLock::new(HashMap::new()),
            store,
            changes: broadcast::channel(256).0,
        })
    }

    /// Entity ids as they change. A slow receiver may lag and miss some; it should then
    /// treat everything as changed.
    pub fn subscribe(&self) -> broadcast::Receiver<String> {
        self.changes.subscribe()
    }

    pub fn store(&self) -> &Arc<Store> {
        &self.store
    }

    pub fn set_integration(&self, name: &'static str, integration: Arc<dyn Integration>) {
        self.integrations
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .insert(name, integration);
    }

    pub fn remove_integration(&self, name: &'static str) {
        self.integrations
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(name);
    }

    /// Sets a state. `last_changed` only moves when the state string changes.
    pub fn set_state(&self, entity_id: &str, state: &str, attributes: Map<String, Value>) {
        let now = rfc3339_micros(SystemTime::now());
        let mut states = self.states.write().unwrap_or_else(|e| e.into_inner());
        let last_changed = match states.get(entity_id) {
            Some(old) if old.state == state => old.last_changed.clone(),
            _ => now.clone(),
        };
        let changed = states
            .get(entity_id)
            .is_none_or(|old| old.state != state || old.attributes != attributes);
        states.insert(
            entity_id.to_string(),
            State {
                entity_id: entity_id.to_string(),
                state: state.to_string(),
                attributes,
                last_changed,
                last_updated: now,
                context: Context::new(),
            },
        );
        drop(states);
        if changed {
            let _ = self.changes.send(entity_id.to_string());
        }
    }

    pub fn remove_state(&self, entity_id: &str) {
        let removed = self
            .states
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(entity_id)
            .is_some();
        if removed {
            let _ = self.changes.send(entity_id.to_string());
        }
    }

    pub fn get_state(&self, entity_id: &str) -> Option<State> {
        self.states
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .get(entity_id)
            .cloned()
    }

    pub fn all_states(&self) -> Vec<State> {
        let mut v: Vec<State> = self
            .states
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .values()
            .cloned()
            .collect();
        v.sort_by(|a, b| a.entity_id.cmp(&b.entity_id));
        v
    }

    /// Entity ids owned by an integration are those with this prefix; used for cleanup.
    pub fn entity_ids_with_prefix(&self, prefix: &str) -> Vec<String> {
        self.states
            .read()
            .unwrap_or_else(|e| e.into_inner())
            .keys()
            .filter(|k| k.starts_with(prefix))
            .cloned()
            .collect()
    }

    /// Synthesises `group.<name>`: "on" if any member is on, listing the member ids.
    pub async fn group_state(&self, name: &str) -> Option<State> {
        if !self.store.group_exists(name).await {
            return None;
        }
        let members = self.store.group_members(name).await;
        let mut any_on = false;
        for m in &members {
            if Box::pin(self.lookup(m))
                .await
                .is_some_and(|s| s.state == "on")
            {
                any_on = true;
                break;
            }
        }
        // An exposed group lists its own all-lights switch first, so a client that imports the
        // group (hasscontrol) gets it without a second group.
        let listed = if self.store.group_exposed(name).await {
            let mut v = vec![group_light_id(name)];
            v.extend(members);
            v
        } else {
            members
        };
        let mut attributes = Map::new();
        attributes.insert("entity_id".into(), json!(listed));
        attributes.insert("friendly_name".into(), json!(name));
        let now = rfc3339_micros(SystemTime::now());
        Some(State {
            entity_id: format!("group.{name}"),
            state: if any_on { "on" } else { "off" }.into(),
            attributes,
            last_changed: now.clone(),
            last_updated: now,
            context: Context::new(),
        })
    }

    /// Name of the exposed group a `light.domus_group_*` id stands for.
    async fn exposed_group_of(&self, entity_id: &str) -> Option<String> {
        let name = entity_id.strip_prefix(GROUP_LIGHT_PREFIX)?;
        self.store
            .group_exposed(name)
            .await
            .then(|| name.to_string())
    }

    /// Members that are real lights. Group lights are skipped, so expansion is one level deep
    /// and groups that include each other's lights cannot recurse.
    async fn real_members(&self, name: &str) -> Vec<String> {
        let mut members = self.store.group_members(name).await;
        members.retain(|m| !m.starts_with(GROUP_LIGHT_PREFIX));
        members
    }

    /// State of a group's light: "on" if any real member is on; deliberately minimal attributes.
    async fn group_light_state(&self, name: &str) -> State {
        let any_on = self
            .real_members(name)
            .await
            .iter()
            .any(|m| self.get_state(m).is_some_and(|s| s.state == "on"));
        let mut attributes = Map::new();
        attributes.insert("friendly_name".into(), json!(name));
        let now = rfc3339_micros(SystemTime::now());
        State {
            entity_id: group_light_id(name),
            state: if any_on { "on" } else { "off" }.into(),
            attributes,
            last_changed: now.clone(),
            last_updated: now,
            context: Context::new(),
        }
    }

    /// States of every group exposed as a light.
    pub async fn group_lights(&self) -> Vec<State> {
        let mut out = Vec::new();
        for n in self.store.exposed_group_names().await {
            out.push(self.group_light_state(&n).await);
        }
        out
    }

    /// Looks up any state, including synthesised groups and group lights.
    pub async fn lookup(&self, entity_id: &str) -> Option<State> {
        if let Some(name) = self.exposed_group_of(entity_id).await {
            return Some(self.group_light_state(&name).await);
        }
        match entity_id.strip_prefix("group.") {
            Some(name) => self.group_state(name).await,
            None => self.get_state(entity_id),
        }
    }

    /// Expands `group.*` ids into their members; other ids pass through unchanged.
    pub async fn expand_entities(&self, ids: &[String]) -> Vec<String> {
        let mut out: Vec<String> = Vec::new();
        for id in ids {
            let expanded = match id.strip_prefix("group.") {
                Some(name) => self.store.group_members(name).await,
                None => vec![id.clone()],
            };
            for e in expanded {
                if !out.contains(&e) {
                    out.push(e);
                }
            }
        }
        out
    }

    /// Whether a token with this scope may read or control the entity: the scoped groups
    /// (`group.<name>`), their all-lights switches, and every member of those groups.
    pub async fn allowed(&self, scope: &Scope, entity_id: &str) -> bool {
        let Some(groups) = scope else {
            return true;
        };
        if let Some(name) = entity_id.strip_prefix("group.") {
            return groups.iter().any(|g| g == name);
        }
        if let Some(name) = entity_id.strip_prefix(GROUP_LIGHT_PREFIX)
            && groups.iter().any(|g| g == name)
        {
            return true;
        }
        for g in groups {
            if self
                .store
                .group_members(g)
                .await
                .iter()
                .any(|m| m == entity_id)
            {
                return true;
            }
        }
        false
    }

    /// Runs a light action. A group light fans out to its real members in parallel and succeeds
    /// if at least one member did; real lights go through their integration.
    pub async fn call_light(&self, entity_id: &str, action: &LightAction) -> Result<(), CallError> {
        let Some(name) = self.exposed_group_of(entity_id).await else {
            return self.call_real_light(entity_id, action).await;
        };
        let members = self.real_members(&name).await;
        self.call_lights(&members, action)
            .await
            .inspect_err(|e| tracing::warn!("group light {name}: {e:?}"))
    }

    /// Runs an action on several real lights: one request when they form a light set the
    /// integration can address together, otherwise one call per light in parallel. Succeeds if
    /// at least one light did.
    pub async fn call_lights(
        &self,
        members: &[String],
        action: &LightAction,
    ) -> Result<(), CallError> {
        if let Some(result) = self.call_light_set(members, action).await {
            return match result {
                Ok(()) => {
                    for m in members {
                        self.apply_optimistic(m, action);
                    }
                    Ok(())
                }
                Err(e) => Err(CallError::Failed(e)),
            };
        }
        let results =
            futures_util::future::join_all(members.iter().map(|m| self.call_real_light(m, action)))
                .await;
        let mut first_error = None;
        let mut any_ok = members.is_empty();
        for (m, r) in members.iter().zip(results) {
            match r {
                Ok(()) => any_ok = true,
                Err(e) => {
                    tracing::warn!("{m} failed: {e:?}");
                    first_error.get_or_insert(e);
                }
            }
        }
        match (any_ok, first_error) {
            (false, Some(e)) => Err(e),
            _ => Ok(()),
        }
    }

    /// The integration that owns every one of `entity_ids`, if there is exactly one.
    async fn call_light_set(
        &self,
        entity_ids: &[String],
        action: &LightAction,
    ) -> Option<Result<(), String>> {
        let first = entity_ids.first()?;
        let integration = self.owner_of(first).ok()?;
        if !entity_ids.iter().all(|e| integration.owns(e)) {
            return None;
        }
        integration.call_light_set(entity_ids, action).await
    }

    /// Every integration's light sets.
    pub fn light_sets(&self) -> Vec<LightSet> {
        let map = self.integrations.read().unwrap_or_else(|e| e.into_inner());
        map.values().flat_map(|i| i.light_sets()).collect()
    }

    fn owner_of(&self, entity_id: &str) -> Result<Arc<dyn Integration>, CallError> {
        let map = self.integrations.read().unwrap_or_else(|e| e.into_inner());
        map.values()
            .find(|i| i.owns(entity_id))
            .cloned()
            .ok_or(CallError::NoIntegration)
    }

    /// Recalls a scene; its state becomes the activation time (HA convention).
    pub async fn activate_scene(&self, entity_id: &str) -> Result<(), CallError> {
        let integration = self.owner_of(entity_id)?;
        integration
            .activate_scene(entity_id)
            .await
            .map_err(CallError::Failed)?;
        self.mark_scene_activated(entity_id);
        Ok(())
    }

    /// Sets a scene's state to now, keeping its attributes.
    pub fn mark_scene_activated(&self, entity_id: &str) {
        if let Some(old) = self.get_state(entity_id) {
            let now = rfc3339_micros(SystemTime::now());
            self.set_state(entity_id, &now, old.attributes);
        }
    }

    /// Runs a light action through the owning integration, then applies it optimistically.
    async fn call_real_light(
        &self,
        entity_id: &str,
        action: &LightAction,
    ) -> Result<(), CallError> {
        let integration = self.owner_of(entity_id)?;
        integration
            .call_light(entity_id, action)
            .await
            .map_err(CallError::Failed)?;

        self.apply_optimistic(entity_id, action);
        Ok(())
    }

    /// Reflects an accepted action in the state right away; the event stream confirms it later.
    fn apply_optimistic(&self, entity_id: &str, action: &LightAction) {
        let Some(old) = self.get_state(entity_id) else {
            return;
        };
        // An unreachable light stays unavailable until the bridge says otherwise.
        if old.state == "unavailable" {
            return;
        }
        let mut attrs = old.attributes;
        let new_state = match action {
            LightAction::TurnOn(p) => {
                if let Some(b) = p.brightness {
                    attrs.insert("brightness".into(), json!(b));
                }
                if let Some(m) = p.color_temp {
                    attrs.insert("color_mode".into(), json!("color_temp"));
                    attrs.insert("color_temp".into(), json!(m));
                    attrs.remove("xy_color");
                }
                if let Some((x, y)) = p.xy {
                    attrs.insert("color_mode".into(), json!("xy"));
                    attrs.insert("xy_color".into(), json!([x, y]));
                    attrs.remove("color_temp");
                }
                "on"
            }
            LightAction::TurnOff { .. } => {
                for k in ["brightness", "color_mode", "color_temp", "xy_color"] {
                    attrs.remove(k);
                }
                "off"
            }
        };
        self.set_state(entity_id, new_state, attrs);
    }
}

#[cfg(test)]
mod tests {
    #[tokio::test]
    async fn state_changes_are_broadcast_once() {
        let core = Core::new(Arc::new(Store::open_memory().await.unwrap()));
        let mut rx = core.subscribe();
        core.set_state("light.a", "on", Map::new());
        core.set_state("light.a", "on", Map::new()); // identical: no event
        core.set_state("light.a", "off", Map::new());
        core.remove_state("light.a");
        core.remove_state("light.a"); // already gone: no event
        for _ in 0..3 {
            assert_eq!(rx.try_recv().unwrap(), "light.a");
        }
        assert!(rx.try_recv().is_err());
    }

    use super::*;

    async fn core() -> Arc<Core> {
        Core::new(Arc::new(Store::open_memory().await.unwrap()))
    }

    #[tokio::test]
    async fn last_changed_only_moves_on_state_change() {
        let c = core().await;
        c.set_state("light.a", "on", Map::new());
        let first = c.get_state("light.a").unwrap();
        std::thread::sleep(std::time::Duration::from_millis(2));
        c.set_state("light.a", "on", Map::new());
        let same = c.get_state("light.a").unwrap();
        assert_eq!(first.last_changed, same.last_changed);
        assert_ne!(first.last_updated, same.last_updated);
        std::thread::sleep(std::time::Duration::from_millis(2));
        c.set_state("light.a", "off", Map::new());
        assert_ne!(
            first.last_changed,
            c.get_state("light.a").unwrap().last_changed
        );
    }

    #[tokio::test]
    async fn group_state_follows_members() {
        let c = core().await;
        c.set_state("light.a", "off", Map::new());
        c.set_state("light.b", "off", Map::new());
        assert!(c.group_state("garmin").await.is_none());
        c.store()
            .group_set("garmin", &["light.a".into(), "light.b".into()])
            .await
            .unwrap();
        let g = c.group_state("garmin").await.unwrap();
        assert_eq!(g.entity_id, "group.garmin");
        assert_eq!(g.state, "off");
        assert_eq!(g.attributes["entity_id"], json!(["light.a", "light.b"]));
        c.set_state("light.b", "on", Map::new());
        assert_eq!(c.group_state("garmin").await.unwrap().state, "on");
        assert_eq!(c.lookup("group.garmin").await.unwrap().state, "on");

        // exposing lists the group's own light first, but never as a stored member
        c.store().group_set_exposed("garmin", true).await;
        let g = c.group_state("garmin").await.unwrap();
        assert_eq!(
            g.attributes["entity_id"],
            json!(["light.domus_group_garmin", "light.a", "light.b"])
        );
        assert_eq!(
            c.store().group_members("garmin").await,
            vec!["light.a", "light.b"]
        );
        assert_eq!(
            c.expand_entities(&["group.garmin".into()]).await,
            vec!["light.a", "light.b"]
        );
        assert!(c.lookup("group.nope").await.is_none());
    }

    #[tokio::test]
    async fn expand_groups_and_dedupe() {
        let c = core().await;
        c.store()
            .group_set("g", &["light.a".into(), "light.b".into()])
            .await
            .unwrap();
        let out = c
            .expand_entities(&["group.g".into(), "light.b".into(), "light.c".into()])
            .await;
        assert_eq!(out, vec!["light.a", "light.b", "light.c"]);
    }

    struct Fake;
    impl Integration for Fake {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.fake")
        }
        fn call_light<'a>(
            &'a self,
            id: &'a str,
            _a: &'a LightAction,
        ) -> BoxFut<'a, Result<(), String>> {
            Box::pin(async move {
                if id == "light.fake_bad" {
                    Err("boom".into())
                } else {
                    Ok(())
                }
            })
        }
    }

    #[tokio::test]
    async fn call_light_applies_optimistic_state() {
        let c = core().await;
        c.set_integration("fake", Arc::new(Fake));
        c.set_state("light.fake1", "off", Map::new());
        c.call_light(
            "light.fake1",
            &LightAction::TurnOn(TurnOn {
                brightness: Some(128),
                ..TurnOn::default()
            }),
        )
        .await
        .unwrap();
        let s = c.get_state("light.fake1").unwrap();
        assert_eq!(s.state, "on");
        assert_eq!(s.attributes["brightness"], json!(128));
        c.call_light("light.fake1", &LightAction::off())
            .await
            .unwrap();
        let s = c.get_state("light.fake1").unwrap();
        assert_eq!(s.state, "off");
        assert!(!s.attributes.contains_key("brightness"));
    }

    #[tokio::test]
    async fn call_light_errors() {
        let c = core().await;
        c.set_integration("fake", Arc::new(Fake));
        assert_eq!(
            c.call_light("light.other", &LightAction::off()).await,
            Err(CallError::NoIntegration)
        );
        assert_eq!(
            c.call_light("light.fake_bad", &LightAction::off()).await,
            Err(CallError::Failed("boom".into()))
        );
    }

    struct Flaky;
    impl Integration for Flaky {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.flaky")
        }
        fn call_light<'a>(
            &'a self,
            id: &'a str,
            _a: &'a LightAction,
        ) -> BoxFut<'a, Result<(), String>> {
            Box::pin(async move {
                if id.ends_with("bad") {
                    Err("boom".into())
                } else {
                    Ok(())
                }
            })
        }
    }

    async fn group_core(members: &[&str]) -> Arc<Core> {
        let c = core().await;
        c.set_integration("flaky", Arc::new(Flaky));
        for m in members {
            c.set_state(m, "off", Map::new());
        }
        let ids: Vec<String> = members.iter().map(|m| m.to_string()).collect();
        c.store().group_set("room", &ids).await.unwrap();
        c
    }

    #[tokio::test]
    async fn group_light_exists_only_when_exposed() {
        let c = group_core(&["light.flaky_a", "light.flaky_b"]).await;
        let id = group_light_id("room");
        assert!(c.lookup(&id).await.is_none());
        assert!(c.group_lights().await.is_empty());
        c.store().group_set_exposed("room", true).await;
        let s = c.lookup(&id).await.unwrap();
        assert_eq!(
            (s.state.as_str(), s.entity_id.as_str()),
            ("off", id.as_str())
        );
        assert_eq!(s.attributes.len(), 1, "only friendly_name");
        c.set_state("light.flaky_b", "on", Map::new());
        assert_eq!(c.lookup(&id).await.unwrap().state, "on");
        assert_eq!(c.group_lights().await.len(), 1);
        c.store().group_delete("room").await;
        assert!(c.lookup(&id).await.is_none());
    }

    #[tokio::test]
    async fn group_light_fans_out_to_members() {
        let c = group_core(&["light.flaky_a", "light.flaky_b"]).await;
        c.store().group_set_exposed("room", true).await;
        let id = group_light_id("room");
        c.call_light(&id, &LightAction::on()).await.unwrap();
        assert_eq!(c.get_state("light.flaky_a").unwrap().state, "on");
        assert_eq!(c.get_state("light.flaky_b").unwrap().state, "on");
        c.call_light(&id, &LightAction::off()).await.unwrap();
        assert_eq!(c.lookup(&id).await.unwrap().state, "off");
    }

    #[tokio::test]
    async fn group_light_partial_and_total_failure() {
        let c = group_core(&["light.flaky_a", "light.flaky_bad"]).await;
        c.store().group_set_exposed("room", true).await;
        let id = group_light_id("room");
        c.call_light(&id, &LightAction::on()).await.unwrap();
        assert_eq!(c.get_state("light.flaky_a").unwrap().state, "on");
        assert_eq!(c.get_state("light.flaky_bad").unwrap().state, "off");

        let c = group_core(&["light.flaky_bad"]).await;
        c.store().group_set_exposed("room", true).await;
        assert_eq!(
            c.call_light(&group_light_id("room"), &LightAction::off())
                .await,
            Err(CallError::Failed("boom".into()))
        );
    }

    #[tokio::test]
    async fn group_light_without_exposure_or_members() {
        let c = group_core(&["light.flaky_a"]).await;
        assert_eq!(
            c.call_light(&group_light_id("room"), &LightAction::off())
                .await,
            Err(CallError::NoIntegration)
        );
        c.store().group_set("empty", &[]).await.unwrap();
        c.store().group_set_exposed("empty", true).await;
        c.call_light(&group_light_id("empty"), &LightAction::off())
            .await
            .unwrap();
    }

    /// Controls `light.set_*` lights as one room; `light.set_bad` makes the room request fail.
    struct Roomy;
    impl Integration for Roomy {
        fn owns(&self, id: &str) -> bool {
            id.starts_with("light.set_")
        }
        fn call_light<'a>(
            &'a self,
            _id: &'a str,
            _a: &'a LightAction,
        ) -> BoxFut<'a, Result<(), String>> {
            Box::pin(async { Err("must not go light by light".into()) })
        }
        fn light_sets(&self) -> Vec<LightSet> {
            vec![LightSet {
                name: "Room".into(),
                kind: "room".into(),
                entity_ids: vec!["light.set_a".into()],
            }]
        }
        fn call_light_set<'a>(
            &'a self,
            ids: &'a [String],
            _a: &'a LightAction,
        ) -> BoxFut<'a, Option<Result<(), String>>> {
            let bad = ids.iter().any(|i| i == "light.set_bad");
            Box::pin(async move { Some(if bad { Err("room down".into()) } else { Ok(()) }) })
        }
    }

    #[tokio::test]
    async fn light_set_goes_through_one_request_and_applies_optimistically() {
        let c = core().await;
        c.set_integration("roomy", Arc::new(Roomy));
        assert_eq!(c.light_sets().len(), 1);
        for id in ["light.set_a", "light.set_b"] {
            c.set_state(id, "off", Map::new());
        }
        c.store()
            .group_set("g", &["light.set_a".into(), "light.set_b".into()])
            .await
            .unwrap();
        c.store().group_set_exposed("g", true).await;
        let id = group_light_id("g");
        let action = LightAction::TurnOn(TurnOn {
            color_temp: Some(300),
            ..TurnOn::default()
        });
        c.call_light(&id, &action).await.unwrap();
        let s = c.get_state("light.set_b").unwrap();
        assert_eq!(s.state, "on");
        assert_eq!(s.attributes["color_temp"], json!(300));
        assert_eq!(s.attributes["color_mode"], json!("color_temp"));

        // xy replaces a previous white temperature
        let xy = LightAction::TurnOn(TurnOn {
            xy: Some((0.5, 0.4)),
            ..TurnOn::default()
        });
        c.call_light(&id, &xy).await.unwrap();
        let s = c.get_state("light.set_a").unwrap();
        assert_eq!(s.attributes["color_mode"], json!("xy"));
        assert!(!s.attributes.contains_key("color_temp"));

        // an unreachable light is left alone
        c.set_state("light.set_a", "unavailable", Map::new());
        c.call_light(&id, &LightAction::off()).await.unwrap();
        assert_eq!(c.get_state("light.set_a").unwrap().state, "unavailable");

        // a failing room request is the call's error
        c.set_state("light.set_bad", "off", Map::new());
        c.store()
            .group_set("g", &["light.set_a".into(), "light.set_bad".into()])
            .await
            .unwrap();
        assert_eq!(
            c.call_light(&id, &LightAction::off()).await,
            Err(CallError::Failed("room down".into()))
        );
    }

    #[tokio::test]
    async fn lights_from_different_integrations_are_not_one_set() {
        let c = core().await;
        c.set_integration("roomy", Arc::new(Roomy));
        c.set_integration("flaky", Arc::new(Flaky));
        assert!(
            c.call_light_set(&[], &LightAction::on()).await.is_none(),
            "no lights"
        );
        let mixed = ["light.set_a".to_string(), "light.flaky_a".to_string()];
        assert!(c.call_light_set(&mixed, &LightAction::on()).await.is_none());
        assert!(
            c.call_light_set(&["light.other".to_string()], &LightAction::on())
                .await
                .is_none()
        );
    }

    #[tokio::test]
    async fn group_lights_that_include_each_other_do_not_recurse() {
        let c = group_core(&["light.flaky_a"]).await;
        let (a, b) = (group_light_id("room"), group_light_id("other"));
        c.store()
            .group_set("room", &["light.flaky_a".into(), b.clone()])
            .await
            .unwrap();
        c.store()
            .group_set("other", &["light.flaky_a".into(), a.clone()])
            .await
            .unwrap();
        c.store().group_set_exposed("room", true).await;
        c.store().group_set_exposed("other", true).await;
        c.call_light(&a, &LightAction::on()).await.unwrap();
        assert_eq!(c.lookup(&b).await.unwrap().state, "on");
        assert_eq!(c.lookup("group.room").await.unwrap().state, "on");
    }
}
