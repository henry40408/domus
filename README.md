# domus

A lightweight, Home Assistant API-compatible server in Rust. It controls Philips Hue lights through a
Hue Bridge (CLIP v2). The MVP implements just the REST subset needed by
[hasscontrol](https://github.com/hatl/hasscontrol) (a Garmin watch widget).

## Run

```sh
cargo run --release
```

Configuration is environment variables only:

| Variable          | Default          | Meaning                                             |
|-------------------|------------------|-----------------------------------------------------|
| `DOMUS_BIND`      | `127.0.0.1:8123` | Listen address (plain HTTP)                         |
| `DOMUS_DATA_DIR`  | `./data`         | SQLite location (dir mode `0700`, `domus.db` `0600`) |
| `DOMUS_LOG`       | `info`           | Log filter (`tracing` env-filter syntax)            |

Everything else is configured in the admin page at `/`:

1. Set the admin password (first start only).
2. Pair the Hue Bridge: enter its IP, press the bridge's link button, click **Pair**.
3. Create a group (e.g. `Garmin`) and tick the lights in it.
4. Create a long-lived access token (shown once; only its hash is stored).

In hasscontrol, set the server URL, paste the token, and set **group** to the same group name.
Keep groups small (about a dozen lights): older Garmin watches have very little memory.

## HTTP API

Home Assistant subset (`Authorization: Bearer <token>`):

- `GET /api/states/{entity_id}` — lights (`light.hue_<8 hex>`) and `group.<name>`
- `POST /api/services/light/turn_on` / `turn_off` — body `{"entity_id": "…"}`; also accepts
  `target.entity_id`, arrays, `group.*`, `brightness` (0–255) and `brightness_pct`
- `POST /api/services/scene/turn_on` — recalls Hue scenes (`scene.hue_<8 hex>`)

**Scenes.** Hue scenes appear as `scene.hue_<8 hex>`, named "Room: Scene". Add them as group members
(next to lights) and hasscontrol lists them as scenes. Their state is `unknown` until activated, then
the activation time; activating from the Hue app updates it too. Light calls on a group skip its scenes.

**Group lights.** hasscontrol only imports a group's members and toggles them one by one. To get a
single switch for several lights, tick "Add an all-lights switch to this group" on the group your
watch imports. The group then lists `light.domus_group_<name>` first, controlling all of its lights
(after changing it, run Refresh entities on the watch). The switch can also be added as a member of
another group. It is on if any member is on, so toggling an on/off mix turns everything off. Commands go to the real
members in parallel and succeed if at least one member did. Group lights are not expanded inside other
group lights (one level only), so groups cannot loop.

Admin API lives under `/api/domus/*` (session cookie, used by the admin page).

## TLS / reverse proxy

domus speaks plain HTTP. hasscontrol requires an `https://` URL with a publicly trusted certificate,
so put a reverse proxy in front. Caddy example:

```
domus.example.com {
    reverse_proxy 127.0.0.1:8123
}
```

nginx must pass the scheme so the admin session cookie gets the `Secure` flag:

```
location / {
    proxy_pass http://127.0.0.1:8123;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

For LAN-only use, get a certificate via DNS-01 (Let's Encrypt) for a name that resolves to your LAN address.

## Security notes

- The Hue application key is stored in plaintext in the SQLite DB; protect the data directory.
- Admin password: argon2id. Access tokens: SHA-256 hashes. Sessions: HttpOnly, SameSite=Strict cookie.
- There is no login rate limiting; do not expose the admin page to the open internet without one at the proxy.

## Not implemented (yet)

WebSocket API, registries, HA OAuth/onboarding, the official HA frontend / Companion app, other domains
(switch, scene, …) and other brands.

## Development

```sh
cargo fmt
cargo nextest run
```
