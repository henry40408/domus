domus test fixtures captured from the Rust server
===================================================

Everything in here is TEST DATA. The passwords, tokens and webhook ids below are real
credentials of the throwaway database in rust-created.db and of nothing else.

Files
-----
internal/store/testdata/rust-created.db   database written by the Rust server (this directory)
internal/store/testdata/v0.1.0.db         hand-made database in the schema of the v0.1.0 release
internal/auth/testdata/password_vectors.json   three (password, stored hash) pairs made by Rust hash_password
internal/server/testdata/rust/*.json      golden HTTP responses of the Rust server (see README.txt there)
internal/server/testdata/rust-order.txt   golden names in the order the requests were sent
internal/server/testdata/rust-hue-puts.jsonl   the PUT requests the Rust server sent to the fake Hue bridge

Provenance
----------
Captured (UTC)      : 2026-11-17T06:01:43Z
Rust binary         : target/debug/domus built 2026-11-11, version string v0.9.4-3-g3ef091f
                      The binary was NOT rebuilt for this capture (cargo build was not allowed in the
                      Go-rewrite job). Every change in src/ since that build is tests-only
                      (commit 842b975, "test: close mutation testing gaps"), so the behaviour is the
                      behaviour of the Rust source in this tree. Its embedded web files are byte-identical
                      to web/ (the sha256 in the static_* goldens matches web/*, see "Golden files").
Environment         : DOMUS_BIND=127.0.0.1:60937, DOMUS_DATA_DIR=<empty temp dir>,
                      DOMUS_MDNS=0, RUST_LOG unset.
Fake Hue bridge     : a throwaway Go port of e2e/src/fake_hue.rs, listening on 127.0.0.1:60938
                      (7 lights, 3 rooms, 7 scenes, no zones; the event stream stays open and silent;
                      PUTs are accepted, logged and ignored; POST /api answers
                      [{"success":{"username":"demo-app-key","clientkey":"x"}}]).
                      That is why hue.ip in the database is "http://127.0.0.1:60938" (a dead port now).
Setup code          : read from the server stderr line "domus: no users yet. Setup code: XXXXX-XXXXX"; it is
                      not stored anywhere (it only lives in memory), so it is not part of the fixtures.

Database rust-created.db
------------------------
One file, journal_mode = wal (the mode is persistent in the file header, as the Rust server leaves it).
The server was stopped (SIGTERM), then the sqlite3 CLI ran PRAGMA wal_checkpoint(TRUNCATE) and
PRAGMA integrity_check (ok). Only domus.db was copied, so no -wal / -shm file is committed.
Row counts: users 4, sessions 8, tokens 15,
refresh_tokens 3, registrations 4, hue 1,
groups 3, group_members 9, _sqlx_migrations 8.
sqlite_sequence (AUTOINCREMENT high-water marks, they include rows that were created and deleted again):
  refresh_tokens=4, registrations=5, tokens=18, users=5

_sqlx_migrations (8 rows, all success = 1; checksum = SHA-384 of the file as committed, verified at capture):

  1  init               checksum F1C1EDC563F1AF8136B3C41943F87342CC33C9906900271C579096CC0A11ED4D85EC55860164B1E34747EC2013066AF0
  2  token scope        checksum 396B2D6C782A42EB87A834750CFF99FF600317D85E499DB7FEEB975FB953DBC8B1E3C96031488CAFE7E3F870DCDD2900
  3  users              checksum 56E4E6ECFAC3A5F43026EEA2A1314AAA0E19AEF6B3F2E9A11F5CDC42DE664F368BD1BB5BC0189298295AA947F41EF988
  4  least privilege    checksum 300723A68449AFBE90B7EA1FC93C8E3D8328FB1C0477DA99AFF62E9DB6C8CD9306DDB0BF02AA106D4F9EDF0781ED72CA
  5  session meta       checksum E5A75DD116428E79D514574A51DF5ACB898B7FC9496FE549ABE0D668A3E1667B9041B9E964B0DBA3FB1B99E05C0571CB
  6  refresh tokens     checksum 42A7BC12B708EAA5CD6CA686E78E5BD8EBE794B1451B40D46E7AC62E83090C20C15A7BE190A05F82C6255CDCB081FDFA
  7  registrations      checksum 415B6945EE8C2FAA09C673DF53F6CF42E916A0F71EA3AEA00CBEE7E7FDC7BBDE8965DA20D7B2B48F574A5A97E4073DF2
  8  app sessions       checksum E790FA3A766A010E9D9FBAC4AFFD3741854BC3C2B046EF1884E9C323781E70ED622AE08A03272D3D00CE762E699F3A61
  (execution_time differs on every run and must not be compared.)

Accounts (users table; the password is the plaintext the account was created with)
-----------------------------------------------------------------------------------
  id 1  admin  created 1794895303  admin, scope NULL (unrestricted)
        password: Fixture-Admin-Pass-1
  id 2  alice  created 1794895304  not admin, scope ["bedroom-lights"]; the password is non-ASCII on purpose
        password: 愛麗絲的密碼是很長很長的字串
  id 3  bob    created 1794895304  not admin, scope NULL; the password has a '$' and a space and a non-ASCII character
        password: bob$pass word!2026 ✓
  id 4  dave   created 1794895304  not admin, scope '[]' (HAND EDITED, see below); the password was changed once through POST /api/domus/password; this is the changed one
        password: dave-changed-pass-2
  A user "temp" (id 5) was created and deleted again to exercise DELETE /users/{id} and the session
  endpoints; only the autoincrement counter remembers it.
  Passwords of admin, alice and bob are the three entries of internal/auth/testdata/password_vectors.json.

Tokens (tokens table)
---------------------
id, name, owner, scope, read_only, created, last_used, expires, refresh_id, plaintext token
(only the SHA-256 hex of the plaintext is stored; "-" means NULL)
   1  full-access                    user 1  scope -                              ro 0  created 1794895305  last_used 1794895305  expires -  refresh_id -
        domus_dcf20250ccb0b5562c86574163e674cd
   2  living-only                    user 1  scope ["living"]                     ro 0  created 1794895305  last_used 1794895305  expires -  refresh_id -
        domus_befc1cc614f8d2e95fda5242bf62d99a
   3  read-only                      user 1  scope -                              ro 1  created 1794895305  last_used 1794895305  expires -  refresh_id -
        domus_349c77d4975b115bff3f189c7a009b0a
   4  valid-for-10-years             user 1  scope -                              ro 0  created 1794895305  last_used -  expires 2110255305  refresh_id -
        domus_7bd2bdce2407d800129b12b16476cec3
   5  expires-in-1-day               user 1  scope -                              ro 0  created 1794895305  last_used 1794895305  expires 1794895306  refresh_id -
        domus_4c2cecd67ded518651ebe60c565dd5f5
   6  scoped-read-only-expiring      user 1  scope ["bedroom-lights","living"]    ro 1  created 1794895305  last_used -  expires 2110255305  refresh_id -
        domus_2e2837c072ec291c72f46d886bcfe4ec
   7  alice-inherits-scope           user 2  scope ["bedroom-lights"]             ro 0  created 1794895305  last_used 1794895305  expires -  refresh_id -
        domus_e28c3d567d1f384aba67c678d1826f37
   8  alice-narrower                 user 2  scope ["bedroom-lights"]             ro 1  created 1794895305  last_used -  expires -  refresh_id -
        domus_a8e1aff7ab95dbaa572a5d78ab507415
   9  bob-watch                      user 3  scope -                              ro 0  created 1794895305  last_used 1794895306  expires -  refresh_id -
        domus_bac91c0a11366341161f7be935e745f7
  10  dave-token                     user 4  scope -                              ro 0  created 1794895305  last_used -  expires -  refresh_id -
        domus_edbb45dec3fcb844cd1289ab51d3db65
  11  dave-empty-scope               user 4  scope []                             ro 0  created 1794895305  last_used -  expires -  refresh_id -
        domus_9c158e5fab61b7f6e264d92b2319c3f2
  14  oauth access token (alice_android) user 2  scope -                              ro 0  created 1794895306  last_used 1794895306  expires 1794897106  refresh_id 2
        domus_1ef4c347e305149a3b3903fef84a6f79
  15  oauth access token (admin_ios) user 1  scope -                              ro 0  created 1794895306  last_used 1794895306  expires 1794897106  refresh_id 1
        domus_28bf264fc7a51b02efc24e85a2415d29
  17  oauth access token (bob_ios)   user 3  scope -                              ro 0  created 1794895306  last_used 1794895306  expires 1794897106  refresh_id 4
        domus_e9ded577205c0d2d1e3813f45e8bf25e
  18  created-from-app-session       user 1  scope -                              ro 0  created 1794895306  last_used -  expires -  refresh_id -
        domus_a4396c994eb7c38c6ceb0ee396b0f54e
  Notes:
  - "expires-in-1-day" was created with expires_days = 1 and then HAND EDITED to expires = created + 1, so it
    is expired at any time you read this. "valid-for-10-years" and "scoped-read-only-expiring" (3650 days)
    are valid until 2036. Every other API token never expires.
  - OAuth access tokens live 30 minutes, so the "oauth access token" rows are expired by now.
  - "alice-inherits-scope" was created without a scope by a limited user and so inherits ["bedroom-lights"].
  - "dave-empty-scope" is owned by dave and has scope '[]' (HAND EDITED, see below).
  - last_used is NULL for tokens that were never used, and set for those that were.
  - Token ids missing from the list (12, 13, 16) were created and revoked again while capturing.

OAuth app logins (refresh_tokens table)
---------------------------------------
  refresh id 1  user 1  client_id https://home-assistant.io/iOS   (admin_ios)
        refresh token: domus_r_fcadefbf8c6ba26acdfd5acd720ab81a4db2d3de170d232a401fcbadd4c133fc
  refresh id 2  user 2  client_id https://home-assistant.io/android   (alice_android)
        refresh token: domus_r_67e18546113375ef097027726d11897c60afbc54b9ea1fb5cf9022f0c44391da
  refresh id 4  user 3  client_id https://home-assistant.io/iOS   (bob_ios)
        refresh token: domus_r_5887e37b00b5ee27af91a81aea8834e82575bfc582b5d974c663d18eb6fd69e3
  Refresh ids missing (3) was a login that was revoked again. Refresh tokens are valid for 90 days of idle
  time. Minting an access token deletes the older access tokens of the same login, that is why each login has
  exactly one row in tokens.

Registrations (registrations table; the stored value is the SHA-256 of the webhook id)
--------------------------------------------------------------------------------------
  id 1  user 1  refresh_id 1  device_id pixel-8-device  device_name Pixel  last_seen set
        webhook id: b56c67a890f195d3518c0318421822fb
        made with the admin OAuth access token (refresh_id set), device_id set, a later update_registration changed app_version/os_version; replaced an earlier registration of the same device (user_id, app_id, device_id)
  id 2  user 3  refresh_id 4  device_id -  device_name Bob phone  last_seen NULL
        webhook id: 2fa168a086cd1799c6c2bdcc88c05bae
        made with bob's OAuth access token; device_id was '' so it is NULL; os_version '' so it is NULL; app_version was the number 2025 (stored as text)
  id 3  user 1  refresh_id -  device_id api-token-device  device_name Tablet  last_seen NULL
        webhook id: cc6650989ed7ded0140fb6e89eb1e5c9
        made with an API token (refresh_id NULL), device_id set
  id 4  user 3  refresh_id -  device_id bob-watch-1  device_name Bob watch  last_seen NULL
        webhook id: e7d2f869f7fb332d7146508288a54010
        made with bob's API token, os_version absent (NULL)
  Registration id 5 was created and revoked again (DELETE /api/domus/apps/{id}). Alice's OAuth token cannot
  register a device (her scope is limited): that is a golden, not a row.

Sessions (sessions table; only hashes are stored, so no plaintext is listed)
----------------------------------------------------------------------------
Expired for any reader of this file after 24 h (idle limit) / 7 days (password logins) / 24 h (app sessions):
do not write tests that expect these rows to be valid. They exist for the column combinations:
  rowid 1  user 1  password login refresh_id -  elevated_until -  user_agent 'domus-capture/1.0 (setup)'
  rowid 2  user 1  password login refresh_id -  elevated_until -  user_agent 'domus-capture/1.0 (https)'
  rowid 3  user 1  password login refresh_id -  elevated_until -  user_agent 'domus-capture/1.0 (case)'
  rowid 4  user 2  password login refresh_id -  elevated_until -  user_agent 'domus-capture/1.0 (alice)'
  rowid 5  user 3  password login refresh_id -  elevated_until -  user_agent 'domus-capture/1.0 (bob)'
  rowid 7  user 1  app session    refresh_id 1  elevated_until 1794895906  user_agent 'HA app · Mozilla/5.0 HomeAssistant/2026.5 (Android)'
  rowid 8  user 2  app session    refresh_id 2  elevated_until -  user_agent 'HA app · Mozilla/5.0 HomeAssistant/2026.5 (Android) alice'
  rowid 9  user 4  password login refresh_id -  elevated_until -  user_agent ''
  (rowid 6 was the "temp" user's last session and was revoked; the elevated app session is the one that
  POST /api/domus/session/reauth was called on.)

Groups and hue
--------------
  group living          expose_light 1   members in pos order: light.hue_a1000003,light.hue_a1000001,scene.hue_b2000002,light.hue_a1000002
  group bedroom-lights  expose_light 0   members in pos order: light.hue_a1000004,light.hue_a1000005
  group all_scenes      expose_light 0   members in pos order: scene.hue_b2000001,scene.hue_b2000004,scene.hue_b2000006
  hue: id 1, ip "http://127.0.0.1:60938", app_key "demo-app-key"
  The "living" members are deliberately not sorted (pos matters), mix lights and a scene, and the group is
  exposed as a light. "all_scenes" has only scenes and was created without expose_light (default 0).
  An empty group "nothing" was created and deleted again.

Column and combination coverage
-------------------------------
  users            scope NULL (admin, bob) / JSON array (alice) / '[]' (dave); is_admin 0/1; COLLATE NOCASE name
  sessions         refresh_id NULL / set; elevated_until NULL / set; user_agent '' / text / "HA app · ..."
  tokens           scope NULL / array / '[]'; read_only 0/1; expires NULL / future / past; last_used NULL / set;
                   refresh_id NULL / set; user_id always set (migration 0003 backfills NULL)
  refresh_tokens   last_used set (a refresh token has last_used after its access token was minted); iOS and android
  registrations    device_id NULL / set; os_version NULL / set; refresh_id NULL / set; last_seen NULL / set
  groups           expose_light 0 / 1
  group_members    out-of-order pos, lights and scenes
  hue              the single row
  owner            dropped by migration 0003 (see v0.1.0.db for it)

HAND EDITS (not producible through the API)
-------------------------------------------
After the server was stopped, the sqlite3 CLI ran:
  UPDATE users  SET scope = '[]' WHERE username = 'dave';
  UPDATE tokens SET scope = '[]' WHERE name = 'dave-empty-scope';
  UPDATE tokens SET expires = created + 1 WHERE name = 'expires-in-1-day';
The API refuses an empty scope list ("Pick at least one group, or leave it unrestricted.") so '[]' (which the
Rust code reads as "nothing allowed", like an unparsable value) can only be made by hand. The expiry edit
makes the expired-token fixture independent of the date the tests run.

v0.1.0.db
---------
Made with the sqlite3 CLI from the schema that tag v0.1.0 created (git show v0.1.0:src/store.rs), journal mode
delete (no WAL, as in v0.1.0). Tables: owner, sessions (token_hash, expires only), tokens WITHOUT last_used, hue,
groups WITHOUT expose_light, group_members. There is no _sqlx_migrations and no users table.
Rows:
  owner          id 1, password_hash = the admin hash of password_vectors.json (the admin password above;
                 v0.1.0 used the same salt$hash format)
  sessions       one row, expires 4102444800
  tokens         id 1 "watch"  created 1700000000, plaintext domus_v010fixturetoken000000000000
                 id 2 "phone"  created 1700000100, plaintext domus_v010fixtureother0000000000000
  hue            id 1, 10.0.0.2, app key v010-app-key
  groups         garmin (members light.hue_a1000002, scene.hue_b2000001, light.hue_a1000001 at pos 0, 1, 2) and
                 old (member light.hue_a1000003 at pos 0)
After opening it with the Go store (as with the Rust one) it must have: user 1 "admin" (is_admin 1, created = the
time of the migration) with that hash, both tokens owned by user 1 and still authenticating, hue 10.0.0.2, groups
with expose_light 0 and the members in the same order, the sessions table emptied (migration 0003 deletes the
rows) and _sqlx_migrations with 8 rows.
Never open the committed file in place from a test: copy it to t.TempDir() first.
The statements are those of the Rust tests v0_1_0_database_keeps_its_data and legacy_groups_table_is_migrated
(src/store.rs), widened to the full v0.1.0 schema.

password_vectors.json
---------------------
[{"user", "password", "stored"}] x 3 (admin ASCII, alice CJK, bob with '$', a space and a non-ASCII character).
"stored" was read from users.password_hash after the Rust server hashed the password (POST /api/domus/setup and
POST /api/domus/users), format "<32 hex salt>$<64 hex Argon2id>". Go VerifyPassword must accept all three.

Golden files (internal/server/testdata/rust/*.json)
---------------------------------------------------
275 files, one per request:
  {"name", "request": {method, path, headers (only those the script set), body | form | body_text},
   "response": {status, headers, body | body_text | body_empty | body_sha256 + body_length}}
- response.headers keeps only: content-type, cache-control, content-security-policy, x-content-type-options,
  x-frame-options, referrer-policy, strict-transport-security, set-cookie, retry-after, allow, location.
  content-length and date are never kept.
- body is the parsed JSON (the key order is the order the server sent; compare as parsed JSON). body_text is a
  non-JSON body (axum rejection texts, "401: Unauthorized", plain-text 400s). body_empty is an empty body.
- body_sha256 / body_length are used for HTML, JS, CSS, SVG and the empty static 404: sha256 of the body after
  replacing the version string v0.9.4-3-g3ef091f by <VERSION>. index.html contains __ASSET_VERSION__, which the
  server replaces by the version, so: sha256(web/index.html with "__ASSET_VERSION__" -> "<VERSION>") equals the
  golden. The other static files and src/oauth/login.html (the /auth/authorize page) are hashed as they are.
  All of them matched the files in this tree at capture time.
- Request headers and form values that carry secrets (Authorization, Cookie, codes, tokens) are normalised like
  responses, so a request is a description of what was sent and not a replayable value.

Normalisation rules (apply them to the Go response before comparing; the same rules were applied to the captured
responses; strings are rewritten everywhere: header values, JSON strings in bodies, request parts)
  1. http://127.0.0.1:<hue port>     -> <HUE_BRIDGE>
  2. 127.0.0.1:<domus port>          -> <BIND>
  3. the version string              -> <VERSION>   (Go: the version of the build under test)
  4. domus_r_ + 64 hex               -> <REFRESH_TOKEN>      (before rule 5)
  5. domus_ + 32 hex                 -> <TOKEN>
  6. a run of exactly 64 hex digits  -> <HEX64>   (session cookie value)
     a run of exactly 32 hex digits  -> <HEX32>   (OAuth code, webhook id)
     a run of exactly 26 hex digits  -> <HEX26>   (state context id)
     "exactly" means: not preceded and not followed by another [0-9a-f]
  7. YYYY-MM-DDTHH:MM:SS.ffffff+00:00 -> <TS>   (state last_changed / last_updated, scene state)
  8. "in <digits> seconds"            -> "in <N> seconds"   (lockout message)
  9. header retry-after              -> <N>
 10. JSON integers under the keys created, last_seen, last_used, expires, elevated_until become the string "<UNIX>";
     null stays null (so NULL versus set is still checked).
  Not normalised on purpose: autoincrement ids (the request order is fixed, see rust-order.txt), entity ids
  (light.hue_a1000001 ... come from the fake bridge ids), Max-Age values, the CSP.

Behaviour worth knowing before porting (all visible in the goldens)
  - POST /api/states without a token is 401 (api_states_post_no_token_401), with a token 405 (api_states_post_405):
    the token check runs before the method check on the HA routes.
  - Minting an access token deletes the older access tokens of the same app login
    (api_states_replaced_access_token_401).
  - Alice's OAuth access token cannot register a device because her user scope is limited (403).
  - /auth/login.js is served Cache-Control: no-store (the /auth/ prefix rule); other static files use no-cache.
  - A failed PUT with a body that is not JSON is 400 only when Content-Type is application/json; without it axum
    answers 415 (admin_login_bad_json_400 sends the header, admin_login_wrong_content_type_415 does not).
  - 405 responses carry an Allow header (recorded).

rust-hue-puts.jsonl: one JSON object per PUT the Rust server sent to the fake bridge, in arrival order
({"method","path","body"}). The order of the PUTs that belong to one request (groups, lists) is not
deterministic because the Rust server sends them concurrently. They belong to the api_service_* and
admin_devices_test_* goldens.

How to redo the capture (the scripts are not committed)
-------------------------------------------------------
Needs: the Rust binary (cargo build, or target/debug/domus), Go with the standard library only (fake bridge),
python3 with the standard library only (driver), and the sqlite3 CLI.
 1. Fake Hue bridge: port e2e/src/fake_hue.rs 1:1 to any language. Routes: GET /clip/v2/resource/{light,room,scene}
    with the 7 lights, 3 rooms and 7 scenes of fake_hue.rs; GET zone, smart_scene, zigbee_connectivity answer
    {"errors":[],"data":[]}; PUT light|scene|grouped_light/{id} answer {"errors":[],"data":[]}; GET
    /eventstream/clip/v2 answers 200 text/event-stream and never sends anything; POST /api answers the
    demo-app-key JSON above. Log every PUT (method, path, body).
 2. Start the bridge on a free port, then the Rust binary with DOMUS_BIND=127.0.0.1:<free port>,
    DOMUS_DATA_DIR=<new empty dir>, DOMUS_MDNS=0 and no RUST_LOG. Read the setup code from stderr.
 3. Send the requests in the order of rust-order.txt. The request of each golden is in its file (method, path,
    headers, body); the passwords are in this file. Use the same User-Agent strings as the session rows above
    (login of the setup session "domus-capture/1.0 (setup)", etc.). Requests that are not goldens but change
    the database: the user "temp" (create, two logins, revoke-others, revoke current session, delete), the
    "throwaway" token (create, revoke), the logins of alice/bob/dave, the OAuth login of bob that is revoked
    again (login_flow, code exchange, revoke of the access token, revoke of the refresh token), the OAuth code
    used by oauth_token_code_wrong_client, bob's OAuth login that stays, the app session of alice, the
    registration "Old" (API token, device_id "to-revoke") that is revoked again, and the loop of failed logins
    for the user "Mallory" until the first 429. The setup request carries the setup code in lower case and
    without the dash. Note that the order of the OAuth logins decides the refresh ids.
 4. Stop the server with SIGTERM. Run the HAND EDITS above, then in sqlite3
    PRAGMA wal_checkpoint(TRUNCATE); PRAGMA integrity_check; copy only domus.db. (The macOS sqlite3 CLI leaves an
    empty domus.db-wal and a domus.db-shm beside the file; they are not copied.)
 5. Normalise as above and write one file per request.
Ids, timestamps, tokens and webhook ids are random or time based, so a redo produces a different database file
and different plaintext secrets: regenerate the lists above from the new database (join through the SHA-256
hashes). Names, scopes, flags, ids, row counts and the passwords are deterministic.

