# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## Overview

domus is a lightweight Home Assistant API-compatible server for Philips Hue lights (Go, standard library `net/http`, pure-Go SQLite). It serves the REST subset hasscontrol (a Garmin widget) needs, enough of OAuth login, `mobile_app` and the WebSocket API for the official Companion apps, and its own admin page. It was rewritten from Rust; the Rust server's behaviour is the specification, pinned by the expected values in the tests (see Tests).

## Commands

Run from the repository root. The Go version comes from `go.mod` alone (`go` + `toolchain` lines); CI and the Dockerfile install nothing version-specific.

- Build: `go build -o bin/domus ./cmd/domus`
- Run: `go run ./cmd/domus` (serves `./data` on `127.0.0.1:8123`)
- Tests: `go test -race ./...`; single test: `go test ./internal/server -run TestName`
- Coverage (as CI): `DOMUS_COVERDIR=$PWD/covdata go test -coverprofile=coverage.out ./...`, then `go tool covdata textfmt -i=covdata -o coverage-bin.out` and append the second profile without its `mode:` line. `-coverprofile` ignores the binary `cmd/domus`'s `TestMain` builds and execs; with `DOMUS_COVERDIR` set it builds that binary with `-cover` and sets `GOCOVERDIR` on every child.
- Lint (CI-enforced): `golangci-lint fmt --diff ./...` (must print nothing) and `golangci-lint run ./...`, at the root and again in `e2e/`. Config is `.golangci.yml` (defaults plus the gofmt formatter). `run` does not check formatting, so `fmt --diff` is a separate command. Suppress a finding with `//nolint:<linter> // reason` (`//lint:ignore` is not honoured). CI installs the binary with `golangci/golangci-lint-action` (`install-only`, no `version:`), so the linter release comes from the action and dependabot tracks only the action.
- Mutation testing (CI-enforced, efficacy at least 95%): `go clean -testcache`, then `go run github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0 unleash .`. The threshold (`unleash.threshold.efficacy: 95`) and the exclusions (`e2e/`, `vendor/`) live in `.gremlins.yaml`. **Never pass `--threshold-efficacy` on the command line**: gremlins v0.6.0 registers it as a float64 flag, viper hands it back as a string, gremlins reads that as 0 and the run always exits 0; the YAML integer works (a run below the threshold prints `ERROR: below efficacy-threshold` and exits non-zero). A cached coverage run makes gremlins' per-mutant timeout near zero, so most mutants "time out" and the score is meaningless. `e2e/` must stay excluded: it is a separate module with no coverage profile. A surviving `CONDITIONALS_BOUNDARY` mutant usually means a missing "exactly at the limit" test. Gremlins reports every `cmd/domus` mutant as killed whatever the tests do: it looks for a directory named after the package (`main`), finds none and runs `go test` at the module root, which fails, and a failing run counts as killed; CI behaves the same, so that package's score is meaningless. To limit CPU locally use `GOMAXPROCS=4` and `--workers 2`; never `--test-cpu`, which gremlins v0.6.0 passes as the single argument `-cpu 2`, so `go test` fails at once and every mutant looks killed (a run of a few seconds is the symptom).
- Dependency checks (CI-enforced): `go mod tidy -diff`, `go mod vendor`, then `git status --porcelain -- vendor go.mod go.sum` must print nothing, `go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...`. This replaces `cargo deny`'s advisory check. Licences: `GOROOT=$(go env GOROOT) go run github.com/google/go-licenses/v2@v2.0.1 check ./... --include_tests --ignore github.com/henry40408/domus --allowed_licenses=<list>`, run at the root and again in `e2e/` (with `--ignore github.com/henry40408/domus/e2e`). The allow-list lives only in the `deps` job's `ALLOWED_LICENSES` in `.github/workflows/ci.yml`; `e2e/` also allows MPL-2.0 (godog pulls in MPL-2.0 modules), the root module must not. `GOROOT` must be set explicitly or go-licenses fails on the standard library. If it reports an unknown or disallowed licence, do not widen the list or swap the module without asking.

Subcommand: `domus healthcheck` calls `GET /health` on `DOMUS_BIND` and exits 0 or 1 (the image's `HEALTHCHECK`).

### Dependencies

- Third-party modules are **vendored and committed** (`vendor/`, about 140 MB); builds and tests use `-mod=vendor` (Go selects it while `vendor/` exists). To add or update one: `go get <module>@<version>`, `go mod tidy`, `go mod vendor`, and commit `go.mod`, `go.sum` and `vendor/` together. Do the same inside `e2e/` for its own module. Never ignore a path inside `vendor/` in `.gitignore`.
- **Never use a dependency version published less than 7 days ago** (Go modules, GitHub Actions, base images, the Go toolchain), indirect ones included. Check with `go list -m -f '{{.Path}} {{.Version}} {{.Time}}' all`; pin an older version with `go get <module>@<older>` when needed. Dependabot uses `cooldown.default-days: 7`.
- GitHub Actions are pinned by commit SHA with the tag as a trailing comment (`uses: owner/repo@<sha> # v1.2.3`).

### E2E (`e2e/`)

Gherkin scenarios (`e2e/features/*.feature`) run through godog and drive `bin/domus` in a real headless browser via chromedp (no driver to download).

- A **separate Go module** with its own `go.mod` and committed `vendor/`; `go test ./...` at the root does not run it. `e2e/go.mod` mirrors the root `go`/`toolchain` lines (CI checks it).
- Run from `e2e/`: `go test -count=1 -v ./...` (`-count=1` because the test cache cannot see a rebuilt `bin/domus`). `E2E_FEATURES=features/login.feature` runs chosen files (comma-separated). `E2E_CHROME` pins the browser binary; on macOS `/Applications/Chromium.app` is found.
- Each scenario starts its **own** domus (free port, temp data dir, `DOMUS_MDNS=0`), its own fake Hue bridge (`e2e/internal/fakehue`) and its own Chrome; scenarios run one at a time. `e2e/internal/harness` builds `bin/domus` if it is missing; it is not rebuilt, so rebuild after server changes.
- The admin page has no test ids; controls are found by placeholder, label or text (`harness.App`). The toast is `#toast.show`.
- Every click goes through `clickUntil` (`e2e/internal/harness/click.go`): on CI Chrome sometimes accepts a click and delivers no event; it then dispatches a scripted `click()` and logs a `WARNING`.
- Regenerate `screenshots/` with `go build -o bin/domus ./cmd/domus && cd e2e && go run ./cmd/screenshots` in the same change as visual edits (CSS, markup, front-end JS).
- Run `golangci-lint fmt --diff ./...` and `golangci-lint run ./...` inside `e2e/` too (the CI `lint` job does).

### Tests

- There are no `testdata/` directories: every expected value is written in the test that uses it. The status, headers and bodies the Rust server answered are pinned by the cases in `internal/server/*_test.go` (helpers `expect`, `expectEach`, `expectInOrder` in `harness_test.go`; random values appear as `<HEX64>` and `<HEX32>`). A database domus 0.12 left (or the Rust server from 0.8) is adopted with its schema and rows as they are, plus one row in `_sqlx_migrations` and `user_version`: `internal/store/migrate_test.go` builds one from the inline SQL of the eight retired migrations (`release012Migrations`, `release012Rows`; never regenerate them from the baseline), and `internal/auth/auth_test.go` holds the Argon2 hashes Rust wrote (`rustPasswordVectors`).
- `cmd/domus/main_test.go` builds the binary once in `TestMain` and `exec`s it. Read the listening address from the log line `listening on http://<addr>` (slog quotes the message).
- Known, accepted differences from Rust: JSON parser error wording (statuses match), WebSocket close code 1000 instead of 1005, JSON object keys that `encoding/json` matches case-insensitively with the last duplicate key winning, and a form body with invalid percent-encoding answered 422 where Rust answered 400 `invalid_grant`. The last two only affect the caller's own request, and the official apps encode correctly.

## Architecture

Thin binary (`cmd/domus`) over internal packages, so tests build the handler directly (`server.New(cfg).Handler()`).

| Package | Responsibility |
| --- | --- |
| `cmd/domus` | env config (`env.go`), `slog` logging, graceful shutdown (`serve.go`), `healthcheck`, `legacyEnvVars` |
| `internal/core` | Entity state model, integration interface, groups, call routing, the state broadcast hub |
| `internal/hue` | Hue Bridge CLIP v2 integration: pairing, sync, SSE event stream, lights, rooms, scenes |
| `internal/store` | SQLite persistence (users, sessions, tokens, groups, Hue pairing, refresh tokens, registrations) and the migration runner |
| `internal/server` | `App`, `Config`, routes, middleware, REST API, admin API, OAuth, `mobile_app`, WebSocket |
| `internal/auth` | Argon2id hashing, random tokens, login throttle, username and password rules |
| `internal/assets` | The admin page (`static/`), embedded with `go:embed` and served stamped with `?v=<version>` |
| `internal/discovery` | mDNS advertisement of `_home-assistant._tcp` |
| `internal/timefmt` | Home Assistant style timestamps |
| `internal/version` | `Version()` and `HAVersion` |
| `vendor/` | Vendored third-party modules, committed |

### Routes and layers

`Handler` (`internal/server/routes.go`) registers `GET /health`, the HA REST API (`/api/states`, `/api/services/...`, bearer token), the admin API under `/api/domus/*` (session cookie), OAuth under `/auth/*`, `mobile_app` and `GET /api/websocket`, then a fallback serving the embedded page. The fallback answers 405 plus `Allow` for a path other methods serve, like axum did. Everything sits behind `securityHeaders`.

Admin routes are declared once in `adminRoutes` with a requirement mask (`needSession`, `needAdmin`, `needRecentAuth`); they are mounted method-less so the same-origin check and the session checks run before the method is judged, which the tests pin. `needRecentAuth` applies to sessions that came from an app token. Token routes check the token before the method.

### Security design

- Passwords are Argon2id; access tokens are stored as SHA-256 hashes; sessions are HttpOnly, SameSite=Strict cookies. Verification must not short-circuit on a wrong username (`auth.WarmDummy`, same cost as a wrong password).
- Login throttling is in memory, keyed by username, doubling from 1 to 15 minutes; a success resets it.
- State-changing admin requests from another origin are refused (`Sec-Fetch-Site`, else `Origin` against `Host` / `X-Forwarded-Host`).
- The first admin needs the one-time setup code printed on stderr; it is cleared once the admin exists.
- WebSocket `Accept` runs with the Origin check off on purpose (no ambient credential; the token is sent in the first message). The reasoning is in the comment in `ws.go`.
- Never log passwords or tokens; audit events use target `audit`.
- The Hue application key is stored in plaintext (documented in the README).

### Storage and migrations

`internal/store/migrate.go` runs the embedded `internal/store/migrations/<version>_<description>.sql` files. `PRAGMA user_version` holds the version the database has reached. There are no checksums and nothing depends on a Go library, so another language can follow the same rules.

- Only files newer than `user_version` run, in order, each in one `BEGIN IMMEDIATE` transaction (the DSN's `_txlock=immediate`) that also moves `user_version`, so a failure leaves neither half. The version is read again inside the transaction, so two processes starting at once apply each file once, and an older domus stops if a newer one got there first. An up-to-date database takes no write lock.
- A database newer than the newest file is refused.
- `0008_baseline.sql`, the first file, creates the whole schema of version 8. It replaced the eight files that built that schema, because SQLite has no `ADD COLUMN IF NOT EXISTS` and they could not be replayed. It runs on an empty database, or on one that reached version 8 before, as `_sqlx_migrations` shows: no failed row, and either a row 8 described `app sessions` (releases 0.8 to 0.12) or the baseline's own row (see below; the database lost `user_version` since). The descriptions count, because the numbers alone would also match another sqlx program. There it only creates what is missing. Anything else below version 8 (an older domus, another program's file) is refused without a change to its schema or data.
- The baseline also writes version `99999999999999` into `_sqlx_migrations`. Releases 0.2 to 0.12 (the Rust server up to 0.10, the Go one from 0.11) refuse a database that records a migration they lack, so they stop instead of running on a schema they do not know; 0.1.0 checks nothing. Never remove the row or the table. Never drop or rename `tokens.last_used` or `groups.expose_light` either: those releases add a missing one before they check the table, which would change a newer database.
- A released file is never edited: databases that ran it do not run it again, and nothing detects the change. Add a new numbered file.
- A new file must be safe to run again wherever SQLite allows it: a database that lost its `user_version` (a restored `.dump`) runs every file again, from the baseline. Guard `CREATE` and `DROP TABLE` or `INDEX` with `IF NOT EXISTS` or `IF EXISTS`, and write a data change so that a second run changes nothing: a conversion applied twice corrupts data without an error. A statement SQLite cannot guard, such as `ADD COLUMN`, is the exception: the second run fails, which rolls that file back and stops startup.
- A migration that rebuilds a table needs foreign keys off, and SQLite ignores `PRAGMA foreign_keys` inside a transaction, so the runner has to change before such a file can ship.

## Versioning & Release

The version is never stored in a manifest: `internal/version.Version()` returns the `-ldflags "-X github.com/henry40408/domus/internal/version.version=$GIT_VERSION"` value when set (not empty, not `dev`), else the module version Go stamps from VCS, else `unknown`. Docker builds pass `GIT_VERSION` as a build arg because `.dockerignore` excludes `.git`. Assets are cached for a year only when stamped with a stable version; `dev` and `-dirty` builds are never cached. Release with `gh release create --generate-notes`; its tag makes `.github/workflows/docker.yml` publish the versioned image (multi-arch to GHCR), and every push to `main` publishes `main`. Never hand-edit versions or `git tag`.

## Conventions

- `go.mod` is the only source of the Go version; `e2e/go.mod` mirrors it.
- **`version.HAVersion` (`2025.12.0`) must not be raised** without checking what the Companion apps then expect: the older value keeps them on their older bridge and skips the `frontend/loaded` handshake.
- Config env vars: `DOMUS_BIND`, `DOMUS_DATA_DIR`, `DOMUS_LOG_LEVEL` (`debug|info|warn|error`, default `info`), `DOMUS_MDNS`; everything else is configured in the admin page. `RUST_LOG` is **retired**: setting it, even empty, fails startup with exit code **1** and the text `RUST_LOG -> DOMUS_LOG_LEVEL`. A bad `DOMUS_BIND` or `DOMUS_LOG_LEVEL` exits with 2.
- `ensureNoLegacyEnvVars` (`cmd/domus/env.go`) fails startup on retired names. Update `legacyEnvVars` (and `liveEnvVars`) whenever an environment variable is added, renamed or removed: a silently ignored variable is worse than a startup failure.
- Logs go to stdout (slog text); stderr carries the setup code and fatal errors.
- `http.Server.WriteTimeout` stays 0: `/api/domus/events` (SSE) and the WebSocket are long-lived.
- Keep API error texts and statuses compatible with the Rust server (the expected values in the tests) when touching handlers; change an expected value only on purpose and say why.
- The Docker image runs as root so a bind-mounted `/data` stays writable.
