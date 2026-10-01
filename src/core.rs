//! State machine, integration trait and group synthesis.

use serde::Serialize;
use serde_json::{Map, Value, json};
use std::collections::HashMap;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, RwLock};
use std::time::SystemTime;

use crate::store::Store;
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

#[derive(Debug, Clone, PartialEq)]
pub enum LightAction {
    TurnOn { brightness: Option<u8> },
    TurnOff,
}

/// A source of entities that can also execute service calls for them.
pub trait Integration: Send + Sync {
    fn owns(&self, entity_id: &str) -> bool;
    fn call_light<'a>(
        &'a self,
        entity_id: &'a str,
        action: &'a LightAction,
    ) -> BoxFut<'a, Result<(), String>>;

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
}

impl Core {
    pub fn new(store: Arc<Store>) -> Arc<Self> {
        Arc::new(Self {
            states: RwLock::new(HashMap::new()),
            integrations: RwLock::new(HashMap::new()),
            store,
        })
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
    }

    pub fn remove_state(&self, entity_id: &str) {
        self.states
            .write()
            .unwrap_or_else(|e| e.into_inner())
            .remove(entity_id);
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
    pub fn group_state(&self, name: &str) -> Option<State> {
        if !self.store.group_exists(name) {
            return None;
        }
        let members = self.store.group_members(name);
        let any_on = members
            .iter()
            .any(|m| self.lookup(m).is_some_and(|s| s.state == "on"));
        // An exposed group lists its own all-lights switch first, so a client that imports the
        // group (hasscontrol) gets it without a second group.
        let listed = if self.store.group_exposed(name) {
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
    fn exposed_group_of(&self, entity_id: &str) -> Option<String> {
        let name = entity_id.strip_prefix(GROUP_LIGHT_PREFIX)?;
        self.store.group_exposed(name).then(|| name.to_string())
    }

    /// Members that are real lights. Group lights are skipped, so expansion is one level deep
    /// and groups that include each other's lights cannot recurse.
    fn real_members(&self, name: &str) -> Vec<String> {
        let mut members = self.store.group_members(name);
        members.retain(|m| !m.starts_with(GROUP_LIGHT_PREFIX));
        members
    }

    /// State of a group's light: "on" if any real member is on; deliberately minimal attributes.
    fn group_light_state(&self, name: &str) -> State {
        let any_on = self
            .real_members(name)
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
    pub fn group_lights(&self) -> Vec<State> {
        self.store
            .exposed_group_names()
            .iter()
            .map(|n| self.group_light_state(n))
            .collect()
    }

    /// Looks up any state, including synthesised groups and group lights.
    pub fn lookup(&self, entity_id: &str) -> Option<State> {
        if let Some(name) = self.exposed_group_of(entity_id) {
            return Some(self.group_light_state(&name));
        }
        match entity_id.strip_prefix("group.") {
            Some(name) => self.group_state(name),
            None => self.get_state(entity_id),
        }
    }

    /// Expands `group.*` ids into their members; other ids pass through unchanged.
    pub fn expand_entities(&self, ids: &[String]) -> Vec<String> {
        let mut out: Vec<String> = Vec::new();
        for id in ids {
            let expanded = match id.strip_prefix("group.") {
                Some(name) => self.store.group_members(name),
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

    /// Runs a light action. A group light fans out to its real members in parallel and succeeds
    /// if at least one member did; real lights go through their integration.
    pub async fn call_light(&self, entity_id: &str, action: &LightAction) -> Result<(), CallError> {
        let Some(name) = self.exposed_group_of(entity_id) else {
            return self.call_real_light(entity_id, action).await;
        };
        let members = self.real_members(&name);
        let results =
            futures_util::future::join_all(members.iter().map(|m| self.call_real_light(m, action)))
                .await;
        let mut first_error = None;
        let mut any_ok = members.is_empty();
        for (m, r) in members.iter().zip(results) {
            match r {
                Ok(()) => any_ok = true,
                Err(e) => {
                    tracing::warn!("group light {name}: {m} failed: {e:?}");
                    first_error.get_or_insert(e);
                }
            }
        }
        match (any_ok, first_error) {
            (false, Some(e)) => Err(e),
            _ => Ok(()),
        }
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

        if let Some(old) = self.get_state(entity_id) {
            let mut attrs = old.attributes;
            let new_state = match action {
                LightAction::TurnOn { brightness } => {
                    if let Some(b) = brightness {
                        attrs.insert("brightness".into(), json!(b));
                    }
                    "on"
                }
                LightAction::TurnOff => {
                    attrs.remove("brightness");
                    "off"
                }
            };
            self.set_state(entity_id, new_state, attrs);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn core() -> Arc<Core> {
        Core::new(Arc::new(Store::open_memory().unwrap()))
    }

    #[test]
    fn last_changed_only_moves_on_state_change() {
        let c = core();
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

    #[test]
    fn group_state_follows_members() {
        let c = core();
        c.set_state("light.a", "off", Map::new());
        c.set_state("light.b", "off", Map::new());
        assert!(c.group_state("garmin").is_none());
        c.store()
            .group_set("garmin", &["light.a".into(), "light.b".into()])
            .unwrap();
        let g = c.group_state("garmin").unwrap();
        assert_eq!(g.entity_id, "group.garmin");
        assert_eq!(g.state, "off");
        assert_eq!(g.attributes["entity_id"], json!(["light.a", "light.b"]));
        c.set_state("light.b", "on", Map::new());
        assert_eq!(c.group_state("garmin").unwrap().state, "on");
        assert_eq!(c.lookup("group.garmin").unwrap().state, "on");

        // exposing lists the group's own light first, but never as a stored member
        c.store().group_set_exposed("garmin", true);
        let g = c.group_state("garmin").unwrap();
        assert_eq!(
            g.attributes["entity_id"],
            json!(["light.domus_group_garmin", "light.a", "light.b"])
        );
        assert_eq!(
            c.store().group_members("garmin"),
            vec!["light.a", "light.b"]
        );
        assert_eq!(
            c.expand_entities(&["group.garmin".into()]),
            vec!["light.a", "light.b"]
        );
        assert!(c.lookup("group.nope").is_none());
    }

    #[test]
    fn expand_groups_and_dedupe() {
        let c = core();
        c.store()
            .group_set("g", &["light.a".into(), "light.b".into()])
            .unwrap();
        let out = c.expand_entities(&["group.g".into(), "light.b".into(), "light.c".into()]);
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
        let c = core();
        c.set_integration("fake", Arc::new(Fake));
        c.set_state("light.fake1", "off", Map::new());
        c.call_light(
            "light.fake1",
            &LightAction::TurnOn {
                brightness: Some(128),
            },
        )
        .await
        .unwrap();
        let s = c.get_state("light.fake1").unwrap();
        assert_eq!(s.state, "on");
        assert_eq!(s.attributes["brightness"], json!(128));
        c.call_light("light.fake1", &LightAction::TurnOff)
            .await
            .unwrap();
        let s = c.get_state("light.fake1").unwrap();
        assert_eq!(s.state, "off");
        assert!(!s.attributes.contains_key("brightness"));
    }

    #[tokio::test]
    async fn call_light_errors() {
        let c = core();
        c.set_integration("fake", Arc::new(Fake));
        assert_eq!(
            c.call_light("light.other", &LightAction::TurnOff).await,
            Err(CallError::NoIntegration)
        );
        assert_eq!(
            c.call_light("light.fake_bad", &LightAction::TurnOff).await,
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

    fn group_core(members: &[&str]) -> Arc<Core> {
        let c = core();
        c.set_integration("flaky", Arc::new(Flaky));
        for m in members {
            c.set_state(m, "off", Map::new());
        }
        let ids: Vec<String> = members.iter().map(|m| m.to_string()).collect();
        c.store().group_set("room", &ids).unwrap();
        c
    }

    #[test]
    fn group_light_exists_only_when_exposed() {
        let c = group_core(&["light.flaky_a", "light.flaky_b"]);
        let id = group_light_id("room");
        assert!(c.lookup(&id).is_none());
        assert!(c.group_lights().is_empty());
        c.store().group_set_exposed("room", true);
        let s = c.lookup(&id).unwrap();
        assert_eq!(
            (s.state.as_str(), s.entity_id.as_str()),
            ("off", id.as_str())
        );
        assert_eq!(s.attributes.len(), 1, "only friendly_name");
        c.set_state("light.flaky_b", "on", Map::new());
        assert_eq!(c.lookup(&id).unwrap().state, "on");
        assert_eq!(c.group_lights().len(), 1);
        c.store().group_delete("room");
        assert!(c.lookup(&id).is_none());
    }

    #[tokio::test]
    async fn group_light_fans_out_to_members() {
        let c = group_core(&["light.flaky_a", "light.flaky_b"]);
        c.store().group_set_exposed("room", true);
        let id = group_light_id("room");
        c.call_light(&id, &LightAction::TurnOn { brightness: None })
            .await
            .unwrap();
        assert_eq!(c.get_state("light.flaky_a").unwrap().state, "on");
        assert_eq!(c.get_state("light.flaky_b").unwrap().state, "on");
        c.call_light(&id, &LightAction::TurnOff).await.unwrap();
        assert_eq!(c.lookup(&id).unwrap().state, "off");
    }

    #[tokio::test]
    async fn group_light_partial_and_total_failure() {
        let c = group_core(&["light.flaky_a", "light.flaky_bad"]);
        c.store().group_set_exposed("room", true);
        let id = group_light_id("room");
        c.call_light(&id, &LightAction::TurnOn { brightness: None })
            .await
            .unwrap();
        assert_eq!(c.get_state("light.flaky_a").unwrap().state, "on");
        assert_eq!(c.get_state("light.flaky_bad").unwrap().state, "off");

        let c = group_core(&["light.flaky_bad"]);
        c.store().group_set_exposed("room", true);
        assert_eq!(
            c.call_light(&group_light_id("room"), &LightAction::TurnOff)
                .await,
            Err(CallError::Failed("boom".into()))
        );
    }

    #[tokio::test]
    async fn group_light_without_exposure_or_members() {
        let c = group_core(&["light.flaky_a"]);
        assert_eq!(
            c.call_light(&group_light_id("room"), &LightAction::TurnOff)
                .await,
            Err(CallError::NoIntegration)
        );
        c.store().group_set("empty", &[]).unwrap();
        c.store().group_set_exposed("empty", true);
        c.call_light(&group_light_id("empty"), &LightAction::TurnOff)
            .await
            .unwrap();
    }

    #[tokio::test]
    async fn group_lights_that_include_each_other_do_not_recurse() {
        let c = group_core(&["light.flaky_a"]);
        let (a, b) = (group_light_id("room"), group_light_id("other"));
        c.store()
            .group_set("room", &["light.flaky_a".into(), b.clone()])
            .unwrap();
        c.store()
            .group_set("other", &["light.flaky_a".into(), a.clone()])
            .unwrap();
        c.store().group_set_exposed("room", true);
        c.store().group_set_exposed("other", true);
        c.call_light(&a, &LightAction::TurnOn { brightness: None })
            .await
            .unwrap();
        assert_eq!(c.lookup(&b).unwrap().state, "on");
        assert_eq!(c.lookup("group.room").unwrap().state, "on");
    }
}
