# domus

> A lightweight, Home Assistant API-compatible server in Rust for Philips Hue lights.

[![CI](https://github.com/henry40408/domus/actions/workflows/ci.yml/badge.svg)](https://github.com/henry40408/domus/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/henry40408/domus/graph/badge.svg)](https://codecov.io/gh/henry40408/domus)
[![Release](https://img.shields.io/github/v/release/henry40408/domus)](https://github.com/henry40408/domus/releases/latest)
[![License](https://img.shields.io/github/license/henry40408/domus)](LICENSE.txt)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-blue.svg)](https://ghcr.io/henry40408/domus)
[![Casual Maintenance Intended](https://casuallymaintained.tech/badge.svg)](https://casuallymaintained.tech/)
[![Vibe Coded](https://img.shields.io/badge/vibe_coded-Claude-d97757?logo=anthropic&logoColor=white)](https://claude.com/claude-code)

domus controls Philips Hue lights through a Hue Bridge (CLIP v2). The MVP implements just the REST
subset needed by [hasscontrol](https://github.com/hatl/hasscontrol) (a Garmin watch widget).

## Features

- **Home Assistant REST subset** - Works with hasscontrol out of the box
- **Hue CLIP v2** - Lights and scenes from your Hue Bridge
- **Groups** - Pick and order the lights and scenes a watch sees; optional all-lights switch per group
- **Admin page** - Pair the bridge, edit groups, manage users and tokens, test devices; works on phones too
- **Docker Ready** - Single binary with all assets embedded, multi-platform images

## Quick Start

### Using Docker (Recommended)

```sh
docker run -d \
  --name domus \
  -p 8123:8123 \
  -v domus_data:/data \
  ghcr.io/henry40408/domus:main
```

`main` tracks the default branch; releases also get semver tags (`0.1.0`, `0.1`).
On first start with no users, domus prints a one-time setup code (`docker logs domus`, look for `Setup code:`).
Open `http://localhost:8123`, enter the code and create the admin account, then continue with [Usage](#usage).
The code stops working once the admin exists; set `DOMUS_SETUP_CODE` to choose your own.

### Building from Source

```sh
git clone https://github.com/henry40408/domus.git
cd domus
cargo run --release
```

## Configuration

Configuration is environment variables only:

| Variable          | Default          | Meaning                                             |
|-------------------|------------------|-----------------------------------------------------|
| `DOMUS_BIND`      | `127.0.0.1:8123` | Listen address (plain HTTP). The container image sets `0.0.0.0:8123`. |
| `DOMUS_DATA_DIR`  | `./data`         | SQLite location (dir mode `0700`, `domus.db` `0600`). The container image sets `/data`. |
| `DOMUS_LOG`       | `info`           | Log filter (`tracing` env-filter syntax)            |
| `DOMUS_SETUP_CODE`| random           | Code required to create the first admin (only while no user exists). When unset, a random one is printed at startup. |

Everything else is configured in the admin page at `/`.

## Usage

Admin page tabs: Dashboard, Groups (admin), Devices, Tokens, Users (admin), Settings. Regular users see Dashboard, Devices, Tokens and Settings, and only their own tokens.

1. Create the admin account (setup code from the log, username and password; first start only). Add more users in Users; admins manage the bridge, groups and users, regular users control lights and manage their own tokens. Upgrading from v0.1.0 turns the old password into the user `admin`; everyone must log in again.
2. Pair the Hue Bridge (Settings): enter its IP, press the bridge's link button, click **Pair**.
3. Create a group (e.g. `Garmin`, Groups), tick lights and scenes, order them, and save. A watch preview shows what hasscontrol will list.
4. Create a long-lived access token (Tokens; shown once, only its hash is stored; "last used" is tracked). Tokens look like `domus_` + 32 hex characters. Optionally limit a token to chosen groups: it can then only reach those groups, their members and the group's light switch. Out-of-scope reads return 404 and out-of-scope service targets are skipped. Tokens without a scope keep full access. Tokens can also be read-only (state reads work, light and scene calls get 403) and can expire after 7 days to a year. In Users, an admin can limit a regular user to chosen groups: their Groups, Lights and Devices views shrink to those groups, and their tokens can only use a subset of them (a token without a scope inherits the user's). A token's effective scope is the intersection of its own scope and its owner's, so narrowing a user narrows their existing tokens too. Admins are never limited.
5. Devices lets you toggle lights and activate scenes to test the setup without the watch.

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

Admin API lives under `/api/domus/*` (session cookie, used by the admin page), including `POST /api/domus/devices/test` (`{"entity_id", "on"}`).

## Docker

### Docker Compose

```yaml
services:
  domus:
    image: ghcr.io/henry40408/domus:main
    ports:
      - "8123:8123"
    volumes:
      - domus_data:/data
    restart: unless-stopped

volumes:
  domus_data:
```

### Building Docker Image

```sh
docker build -t domus:local .
```

Static musl binary cross-compiled with cargo-zigbuild, on a distroless base.

### Production Notes

- Mount `/data` so the SQLite database persists, and back it up: the Hue application key is stored in plaintext.
- The container runs as root so a bind-mounted `/data` stays writable without a permissions change.
- Terminate TLS at a reverse proxy; see [TLS / reverse proxy](#tls--reverse-proxy).
- Port 8123 is also Home Assistant's default; change the published port (e.g. `-p 8124:8123`) if both run on one host.

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
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

For LAN-only use, get a certificate via DNS-01 (Let's Encrypt) for a name that resolves to your LAN address.

## Security notes

- The Hue application key is stored in plaintext in the SQLite DB; protect the data directory.
- User passwords: argon2id. Access tokens: SHA-256 hashes. Sessions: HttpOnly, SameSite=Strict cookie.
- Passwords: 12 to 128 characters, not equal to the username. The rule applies when a user is created or a password is set; existing passwords keep working.
- Login throttling: after 5 failed attempts in a row for a username, further attempts are refused (HTTP 429, `Retry-After`) for 1 minute, doubling each time up to 15 minutes. A success resets it. It is kept in memory (a restart clears it), keyed by username rather than IP, and never locks an account permanently. Unknown usernames take the same time and are throttled the same way, so neither reveals which accounts exist. Add IP-based limits at the proxy if you expose the page to the internet.
- State-changing admin requests from another site are refused (`Sec-Fetch-Site`, or `Origin` against `Host` / `X-Forwarded-Host`). Behind a proxy that rewrites `Host`, pass it through (see nginx above) or set `X-Forwarded-Host`.
- Responses carry a CSP, `X-Frame-Options: DENY`, `nosniff` and `Referrer-Policy: no-referrer`; admin API responses are `no-store`, and HSTS is sent when the proxy sets `X-Forwarded-Proto: https`.
- Sessions end after 24 hours without activity (7 days at most). Settings lists where you are signed in and can sign out one device or all others; session ids are row ids, the cookie value is never shown.
- Security events (logins, lockouts, user and token changes) are logged at target `audit`, never with passwords or tokens.

## Not implemented (yet)

WebSocket API, registries, HA OAuth/onboarding, the official HA frontend / Companion app, other domains
(switch, scene, …) and other brands.

## Development

### Prerequisites

- Rust (edition 2024)
- [cargo-nextest](https://nexte.st/)

### Running Tests

```sh
cargo fmt
cargo clippy --all-targets -- -D warnings
cargo nextest run
```

### Coverage

CI uploads coverage to [Codecov](https://codecov.io/gh/henry40408/domus) using
[cargo-llvm-cov](https://github.com/taiki-e/cargo-llvm-cov) (`src/main.rs` is excluded, see `codecov.yml`).
Forks need a `CODECOV_TOKEN` repository secret. To run it locally:

```sh
cargo llvm-cov nextest --html
```

## License

MIT, see [LICENSE.txt](LICENSE.txt).
