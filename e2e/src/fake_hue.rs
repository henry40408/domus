//! A tiny stand-in for a Hue Bridge (CLIP v2), just enough for domus to pair
//! with it and list a believable house. Writes are accepted and ignored.

use anyhow::{Context, Result};
use axum::extract::Path;
use axum::routing::{get, post, put};
use axum::{Json, Router};
use serde_json::{Value, json};
use tokio::task::JoinHandle;

/// A light: id, name, owner device, kind ("color", "ct" or "dim"), on,
/// brightness percent, mirek, CIE xy.
type Light = (
    &'static str,
    &'static str,
    &'static str,
    &'static str,
    bool,
    f64,
    u16,
    (f64, f64),
);

const LIGHTS: &[Light] = &[
    (
        "a1000001-0000-4000-8000-000000000001",
        "Ceiling",
        "dev-1",
        "color",
        true,
        80.0,
        366,
        (0.46, 0.41),
    ),
    (
        "a1000002-0000-4000-8000-000000000002",
        "Floor lamp",
        "dev-2",
        "color",
        true,
        55.0,
        454,
        (0.52, 0.41),
    ),
    (
        "a1000003-0000-4000-8000-000000000003",
        "TV strip",
        "dev-3",
        "color",
        true,
        40.0,
        300,
        (0.17, 0.20),
    ),
    (
        "a1000004-0000-4000-8000-000000000004",
        "Bedside left",
        "dev-4",
        "ct",
        true,
        30.0,
        400,
        (0.0, 0.0),
    ),
    (
        "a1000005-0000-4000-8000-000000000005",
        "Bedside right",
        "dev-5",
        "ct",
        false,
        100.0,
        370,
        (0.0, 0.0),
    ),
    (
        "a1000006-0000-4000-8000-000000000006",
        "Desk lamp",
        "dev-6",
        "ct",
        true,
        100.0,
        250,
        (0.0, 0.0),
    ),
    (
        "a1000007-0000-4000-8000-000000000007",
        "Monitor light",
        "dev-7",
        "color",
        false,
        70.0,
        300,
        (0.31, 0.33),
    ),
];

/// A room: id, grouped_light id, name, devices.
type Room = (
    &'static str,
    &'static str,
    &'static str,
    &'static [&'static str],
);

const ROOMS: &[Room] = &[
    (
        "c3000001-0000-4000-8000-000000000001",
        "d4000001-0000-4000-8000-000000000001",
        "Living Room",
        &["dev-1", "dev-2", "dev-3"],
    ),
    (
        "c3000002-0000-4000-8000-000000000002",
        "d4000002-0000-4000-8000-000000000002",
        "Bedroom",
        &["dev-4", "dev-5"],
    ),
    (
        "c3000003-0000-4000-8000-000000000003",
        "d4000003-0000-4000-8000-000000000003",
        "Office",
        &["dev-6", "dev-7"],
    ),
];

/// A scene: id, name, room id.
const SCENES: &[(&str, &str, &str)] = &[
    ("b2000001-0000-4000-8000-000000000001", "Relax", ROOMS[0].0),
    (
        "b2000002-0000-4000-8000-000000000002",
        "Movie night",
        ROOMS[0].0,
    ),
    (
        "b2000003-0000-4000-8000-000000000003",
        "Energize",
        ROOMS[0].0,
    ),
    (
        "b2000004-0000-4000-8000-000000000004",
        "Nightlight",
        ROOMS[1].0,
    ),
    ("b2000005-0000-4000-8000-000000000005", "Read", ROOMS[1].0),
    ("b2000006-0000-4000-8000-000000000006", "Focus", ROOMS[2].0),
    (
        "b2000007-0000-4000-8000-000000000007",
        "Concentrate",
        ROOMS[2].0,
    ),
];

fn light_json(l: &Light) -> Value {
    let &(id, name, owner, kind, on, brightness, mirek, (x, y)) = l;
    let mut v = json!({
        "id": id, "type": "light", "metadata": {"name": name}, "on": {"on": on},
        "dimming": {"brightness": brightness},
        "owner": {"rid": owner, "rtype": "device"},
    });
    if kind == "color" || kind == "ct" {
        v["color_temperature"] = json!({"mirek": mirek, "mirek_valid": true});
    }
    if kind == "color" {
        v["color"] = json!({"xy": {"x": x, "y": y}});
    }
    v
}

fn room_json(r: &Room) -> Value {
    let &(id, grouped, name, devices) = r;
    json!({
        "id": id, "type": "room", "metadata": {"name": name},
        "children": devices.iter().map(|d| json!({"rid": d, "rtype": "device"})).collect::<Vec<_>>(),
        "services": [{"rid": grouped, "rtype": "grouped_light"}],
    })
}

fn scene_json(s: &(&str, &str, &str)) -> Value {
    json!({"id": s.0, "type": "scene", "metadata": {"name": s.1},
           "group": {"rid": s.2, "rtype": "room"}, "status": {"active": "inactive"}})
}

fn data(items: Vec<Value>) -> Json<Value> {
    Json(json!({"errors": [], "data": items}))
}

/// Serves the fake bridge on an ephemeral loopback port. Returns its base URL.
pub async fn spawn() -> Result<(String, JoinHandle<()>)> {
    async fn accept(Path(_): Path<String>) -> Json<Value> {
        data(vec![])
    }
    let app = Router::new()
        .route(
            "/clip/v2/resource/light",
            get(|| async { data(LIGHTS.iter().map(light_json).collect()) }),
        )
        .route(
            "/clip/v2/resource/room",
            get(|| async { data(ROOMS.iter().map(room_json).collect()) }),
        )
        .route(
            "/clip/v2/resource/scene",
            get(|| async { data(SCENES.iter().map(scene_json).collect()) }),
        )
        .route("/clip/v2/resource/zone", get(|| async { data(vec![]) }))
        .route(
            "/clip/v2/resource/smart_scene",
            get(|| async { data(vec![]) }),
        )
        .route(
            "/clip/v2/resource/zigbee_connectivity",
            get(|| async { data(vec![]) }),
        )
        .route("/clip/v2/resource/light/{id}", put(accept))
        .route("/clip/v2/resource/scene/{id}", put(accept))
        .route("/clip/v2/resource/grouped_light/{id}", put(accept))
        // Held open and silent, like a bridge with nothing to report.
        .route(
            "/eventstream/clip/v2",
            get(|| async {
                axum::body::Body::from_stream(futures_util::stream::pending::<
                    Result<Vec<u8>, std::io::Error>,
                >())
            }),
        )
        .route(
            "/api",
            post(|| async {
                Json(json!([{"success": {"username": "demo-app-key", "clientkey": "x"}}]))
            }),
        );
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0")
        .await
        .context("binding the fake bridge")?;
    let url = format!("http://{}", listener.local_addr()?);
    let task = tokio::spawn(async move {
        let _ = axum::serve(listener, app).await;
    });
    Ok((url, task))
}
