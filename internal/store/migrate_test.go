package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// baselineVersion is the schema version the baseline file creates. It stays the
// same when later migrations are added.
const baselineVersion = 8

// sentinel starts the one row a new database holds in _sqlx_migrations: version,
// success and the type of the checksum. Releases 0.2 to 0.12 refuse a database
// that records a migration they do not have, so it must be out of their range,
// successful and hold a blob.
var sentinel = []any{int64(99999999999999), int64(1), "blob"}

func openMemory(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// openRaw opens a database file without running any migration.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	noError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newRawDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return openRaw(t, filepath.Join(t.TempDir(), "raw.db"))
}

func mustExec(t *testing.T, db *sql.DB, script string) {
	t.Helper()
	_, err := db.Exec(script)
	noError(t, err)
}

func recordRows(t *testing.T, db *sql.DB) [][]any {
	t.Helper()
	rows, err := db.Query(`SELECT version, success, typeof(checksum), description,
		CAST(installed_on AS TEXT), hex(checksum), execution_time FROM _sqlx_migrations ORDER BY version`)
	noError(t, err)
	defer func() { _ = rows.Close() }()
	var all [][]any
	for rows.Next() {
		var version, success, executionTime int64
		var kind, description, installedOn, checksum string
		noError(t, rows.Scan(&version, &success, &kind, &description, &installedOn, &checksum, &executionTime))
		all = append(all, []any{version, success, kind, description, installedOn, checksum, executionTime})
	}
	noError(t, rows.Err())
	return all
}

func schemaVersion(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	version, err := userVersion(context.Background(), db)
	noError(t, err)
	return version
}

var (
	sqlComment        = regexp.MustCompile(`--[^\n]*`)
	sqlSpace          = regexp.MustCompile(`\s+`)
	sqlSpaceAtBracket = regexp.MustCompile(` ?([(),]) ?`)
)

// schemaOf maps each object to its statement without comments and layout, so a
// schema written in one piece compares with one that ALTER TABLE grew. Column
// order, types and constraints all still count.
func schemaOf(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL ORDER BY name")
	noError(t, err)
	defer func() { _ = rows.Close() }()
	schema := map[string]string{}
	for rows.Next() {
		var name, statement string
		noError(t, rows.Scan(&name, &statement))
		statement = sqlSpace.ReplaceAllString(sqlComment.ReplaceAllString(statement, ""), " ")
		schema[name] = sqlSpaceAtBracket.ReplaceAllString(statement, "$1")
	}
	noError(t, rows.Err())
	return schema
}

// release012Migrations are the statements of the eight migrations domus ran up to
// release 0.12, and the Rust server before it. They are never regenerated from the
// baseline.
const release012Migrations = `
CREATE TABLE _sqlx_migrations (
    version BIGINT PRIMARY KEY,
    description TEXT NOT NULL,
    installed_on TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    success BOOLEAN NOT NULL,
    checksum BLOB NOT NULL,
    execution_time BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS owner (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    password_hash TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    expires INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created INTEGER NOT NULL,
    last_used INTEGER
);
CREATE TABLE IF NOT EXISTS hue (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    ip TEXT NOT NULL,
    app_key TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS groups (
    name TEXT PRIMARY KEY,
    expose_light INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS group_members (
    group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
    entity_id TEXT NOT NULL,
    pos INTEGER NOT NULL,
    PRIMARY KEY (group_name, entity_id)
);

ALTER TABLE tokens ADD COLUMN scope TEXT;

CREATE TABLE users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    is_admin INTEGER NOT NULL DEFAULT 0,
    created INTEGER NOT NULL
);
DROP TABLE owner;
ALTER TABLE sessions ADD COLUMN user_id INTEGER;
ALTER TABLE tokens ADD COLUMN user_id INTEGER;

ALTER TABLE users ADD COLUMN scope TEXT;
ALTER TABLE tokens ADD COLUMN read_only INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tokens ADD COLUMN expires INTEGER;

ALTER TABLE sessions ADD COLUMN created INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN last_seen INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN user_agent TEXT;

CREATE TABLE refresh_tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    client_id TEXT NOT NULL,
    created INTEGER NOT NULL,
    last_used INTEGER
);
CREATE INDEX refresh_tokens_user ON refresh_tokens (user_id);
ALTER TABLE tokens ADD COLUMN refresh_id INTEGER;
CREATE INDEX tokens_refresh ON tokens (refresh_id);

CREATE TABLE registrations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_hash TEXT NOT NULL UNIQUE,
    user_id INTEGER NOT NULL,
    refresh_id INTEGER,
    app_id TEXT NOT NULL,
    app_name TEXT NOT NULL,
    app_version TEXT NOT NULL,
    device_id TEXT,
    device_name TEXT NOT NULL,
    manufacturer TEXT NOT NULL,
    model TEXT NOT NULL,
    os_name TEXT NOT NULL,
    os_version TEXT,
    created INTEGER NOT NULL,
    last_seen INTEGER
);
CREATE INDEX registrations_user ON registrations (user_id);
CREATE INDEX registrations_refresh ON registrations (refresh_id);
CREATE UNIQUE INDEX registrations_device ON registrations (user_id, app_id, device_id)
    WHERE device_id IS NOT NULL;

ALTER TABLE sessions ADD COLUMN refresh_id INTEGER;
ALTER TABLE sessions ADD COLUMN elevated_until INTEGER;
CREATE INDEX sessions_refresh ON sessions (refresh_id);
`

// release012Rows are the records sqlx wrote for those migrations, and a little
// data in every table.
const release012Rows = `
INSERT INTO _sqlx_migrations VALUES(1,'init','2026-11-17 06:01:43',1,x'f1c1edc563f1af8136b3c41943f87342cc33c9906900271c579096cc0a11ed4d85ec55860164b1e34747ec2013066af0',1001);
INSERT INTO _sqlx_migrations VALUES(2,'token scope','2026-11-17 06:01:43',1,x'396b2d6c782a42eb87a834750cff99ff600317d85e499db7feeb975fb953dbc8b1e3c96031488cafe7e3f870dcdd2900',1002);
INSERT INTO _sqlx_migrations VALUES(3,'users','2026-11-17 06:01:43',1,x'56e4e6ecfac3a5f43026eea2a1314aaa0e19aef6b3f2e9a11f5cdc42de664f368bd1bb5bc0189298295aa947f41ef988',1003);
INSERT INTO _sqlx_migrations VALUES(4,'least privilege','2026-11-17 06:01:43',1,x'300723a68449afbe90b7ea1fc93c8e3d8328fb1c0477da99aff62e9db6c8cd9306ddb0bf02aa106d4f9edf0781ed72ca',1004);
INSERT INTO _sqlx_migrations VALUES(5,'session meta','2026-11-17 06:01:43',1,x'e5a75dd116428e79d514574a51df5acb898b7fc9496fe549abe0d668a3e1667b9041b9e964b0dba3fb1b99e05c0571cb',1005);
INSERT INTO _sqlx_migrations VALUES(6,'refresh tokens','2026-11-17 06:01:43',1,x'42a7bc12b708eaa5cd6ca686e78e5bd8ebe794b1451b40d46e7ac62e83090c20c15a7be190a05f82c6255cdcb081fdfa',1006);
INSERT INTO _sqlx_migrations VALUES(7,'registrations','2026-11-17 06:01:43',1,x'415b6945ee8c2faa09c673df53f6cf42e916a0f71ea3aea00cbee7e7fdc7bbde8965da20d7b2b48f574a5a97e4073df2',1007);
INSERT INTO _sqlx_migrations VALUES(8,'app sessions','2026-11-17 06:01:43',1,x'e790fa3a766a010e9d9fbac4affd3741854bc3c2b046ef1884e9c323781e70ed622ae08a03272d3d00ce762e699f3a61',1008);
INSERT INTO users VALUES(1,'root','hash-root',1,1700000000,NULL);
INSERT INTO sessions VALUES('session-hash',4102444800,1,1700000000,1700000000,'domus-fixture',NULL,NULL);
INSERT INTO tokens VALUES(1,'watch','token-hash',1700000000,NULL,NULL,1,0,NULL,NULL);
INSERT INTO hue VALUES(1,'10.0.0.2','app-key');
INSERT INTO groups VALUES('garmin',1);
INSERT INTO group_members VALUES('garmin','light.hue_a1000001',0);
INSERT INTO refresh_tokens VALUES(1,'refresh-hash',1,'client',1700000000,NULL);
INSERT INTO registrations VALUES(1,'webhook-hash',1,1,'app','App','1','device','Phone','Maker','Model','OS',NULL,1700000000,NULL);
`

// createRelease012Database writes a database as domus 0.12 left it.
func createRelease012Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release012.db")
	db := openRaw(t, path)
	mustExec(t, db, release012Migrations)
	mustExec(t, db, release012Rows)
	return path
}

// v010Script is the database the v0.1.0 release made: no users table, no
// _sqlx_migrations, and tokens and groups without the columns added later.
const v010Script = `
CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1), password_hash TEXT NOT NULL);
CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);
CREATE TABLE tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created INTEGER NOT NULL
);
CREATE TABLE hue (id INTEGER PRIMARY KEY CHECK (id = 1), ip TEXT NOT NULL, app_key TEXT NOT NULL);
CREATE TABLE groups (name TEXT PRIMARY KEY);
CREATE TABLE group_members (
    group_name TEXT NOT NULL REFERENCES groups(name) ON DELETE CASCADE,
    entity_id TEXT NOT NULL,
    pos INTEGER NOT NULL,
    PRIMARY KEY (group_name, entity_id)
);
INSERT INTO owner VALUES (1, 'v010-owner-hash');`

func createNewDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "new.db")
	s, err := Open(ctx, path)
	noError(t, err)
	noError(t, s.Close())
	return path
}

func shippedMigrations(t *testing.T) []migration {
	t.Helper()
	files, err := fs.Sub(embeddedMigrations, "migrations")
	noError(t, err)
	shipped, err := loadMigrations(files)
	noError(t, err)
	return shipped
}

// onlyBaseline keeps a test about the baseline true after later migrations ship.
func onlyBaseline(t *testing.T) []migration {
	t.Helper()
	return shippedMigrations(t)[:1]
}

func applyTo(t *testing.T, db *sql.DB, migrations []migration) {
	t.Helper()
	noError(t, applyMigrations(context.Background(), db, migrations))
}

func TestNewDatabaseIsAtTheLatestVersionAndRecordsOnlyTheSentinel(t *testing.T) {
	db := openMemory(t).db
	shipped := shippedMigrations(t)
	if got, want := schemaVersion(t, db), shipped[len(shipped)-1].version; got != want {
		t.Fatalf("user_version = %d, want %d", got, want)
	}
	rows := recordRows(t, db)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0][:3], sentinel) {
		t.Fatalf("_sqlx_migrations = %v, want only %v", rows, sentinel)
	}
}

func TestBaselineBuildsTheSchemaTheOldMigrationsBuilt(t *testing.T) {
	fresh := newRawDatabase(t)
	applyTo(t, fresh, onlyBaseline(t))
	got := schemaOf(t, fresh)
	want := schemaOf(t, openRaw(t, createRelease012Database(t)))
	if !maps.Equal(got, want) {
		t.Fatalf("schema differs\n got: %q\nwant: %q", got, want)
	}
}

// A database domus 0.12 left has user_version 0. A stale number below the
// baseline, left by some other tool, must not matter either.
func TestOpenAdoptsARelease012Database(t *testing.T) {
	for _, stale := range []string{"0", "5", "7"} {
		t.Run("user_version "+stale, func(t *testing.T) {
			db := openRaw(t, createRelease012Database(t))
			mustExec(t, db, "PRAGMA user_version = "+stale)
			schema, records := schemaOf(t, db), recordRows(t, db)

			applyTo(t, db, onlyBaseline(t))

			if got := schemaVersion(t, db); got != baselineVersion {
				t.Fatalf("user_version = %d", got)
			}
			if got := schemaOf(t, db); !maps.Equal(got, schema) {
				t.Fatalf("schema changed\n got: %q\nwant: %q", got, schema)
			}
			after := recordRows(t, db)
			if len(after) != len(records)+1 || !reflect.DeepEqual(after[:len(records)], records) {
				t.Fatalf("old records changed\nbefore: %v\n after: %v", records, after)
			}
			if last := after[len(after)-1]; !reflect.DeepEqual(last[:3], sentinel) {
				t.Fatalf("last record = %v, want %v", last, sentinel)
			}
		})
	}
}

func TestAdoptedDatabaseKeepsItsDataAndIsWritable(t *testing.T) {
	s, err := Open(ctx, createRelease012Database(t))
	noError(t, err)
	defer func() { _ = s.Close() }()

	admin, hash, err := s.UserLogin(ctx, "root")
	noError(t, err)
	if admin.ID != 1 || !admin.IsAdmin || hash != "hash-root" {
		t.Fatalf("admin = %+v hash = %q", admin, hash)
	}
	_, err = s.TokenAuth(ctx, "token-hash")
	noError(t, err)
	bridge, err := s.HueGet(ctx)
	noError(t, err)
	if bridge != (HueBridge{IP: "10.0.0.2", Key: "app-key"}) {
		t.Fatalf("bridge = %+v", bridge)
	}
	members, err := s.GroupMembers(ctx, "garmin")
	noError(t, err)
	if want := []string{"light.hue_a1000001"}; !reflect.DeepEqual(members, want) {
		t.Fatalf("members = %v, want %v", members, want)
	}
	rest := queryInt(t, s, `SELECT (SELECT count(*) FROM sessions) + (SELECT count(*) FROM refresh_tokens) +
		(SELECT count(*) FROM registrations)`)
	if rest != 3 {
		t.Fatalf("sessions, app logins and registrations = %d, want one of each", rest)
	}
	_, err = s.CreateUser(ctx, "newcomer", "hash", false)
	noError(t, err)
}

// A restored `.dump` loses user_version.
func TestBaselineReplaysOverADatabaseThatLostItsVersion(t *testing.T) {
	tests := map[string]struct {
		create  func(*testing.T) string
		records int
	}{
		// The eight old records and the sentinel.
		"adopted from 0.12": {createRelease012Database, 9},
		"new":               {createNewDatabase, 1},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			db := openRaw(t, test.create(t))
			for range 2 {
				mustExec(t, db, "PRAGMA user_version = 0")
				applyTo(t, db, onlyBaseline(t))
				if got := len(recordRows(t, db)); got != test.records {
					t.Fatalf("%d records, want %d", got, test.records)
				}
			}
		})
	}
}

type snapshot struct {
	version int64
	schema  map[string]string
	records [][]any
}

func snapshotOf(t *testing.T, db *sql.DB) snapshot {
	t.Helper()
	s := snapshot{version: schemaVersion(t, db), schema: schemaOf(t, db)}
	if _, ok := s.schema["_sqlx_migrations"]; ok {
		s.records = recordRows(t, db)
	}
	return s
}

func TestOpenRefusesADatabaseItCannotUpgradeAndLeavesItAlone(t *testing.T) {
	changed := func(create func(*testing.T) string, change string) func(*testing.T) string {
		return func(t *testing.T) string {
			path := create(t)
			mustExec(t, openRaw(t, path), change)
			return path
		}
	}
	empty := func(t *testing.T) string { return filepath.Join(t.TempDir(), "other.db") }
	tests := map[string]struct {
		create func(*testing.T) string
		want   string
	}{
		"from domus 0.1": {changed(empty, v010Script), "schema version 8"},
		"one migration short": {
			changed(createRelease012Database, "DELETE FROM _sqlx_migrations WHERE version = 8"),
			"schema version 8"},
		"with a migration that failed halfway": {
			changed(createRelease012Database, "UPDATE _sqlx_migrations SET success = 0 WHERE version = 3"),
			"schema version 8"},
		"of another program": {changed(empty, "CREATE TABLE notes (body TEXT)"), "schema version 8"},
		"with only a view":   {changed(empty, "CREATE VIEW one AS SELECT 1"), "schema version 8"},
		// sqlx numbers the migrations of other programs the same way.
		"of another program that uses sqlx": {changed(empty, `
			CREATE TABLE _sqlx_migrations (version BIGINT PRIMARY KEY, description TEXT NOT NULL,
				installed_on TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, success BOOLEAN NOT NULL,
				checksum BLOB NOT NULL, execution_time BIGINT NOT NULL);
			INSERT INTO _sqlx_migrations VALUES (8, 'feed icons', '2026-11-20 07:01:38', 1, x'08', 1);
			CREATE TABLE feeds (url TEXT)`), "schema version 8"},
		"of another program that left sqlx": {changed(createNewDatabase, `
			UPDATE _sqlx_migrations SET description = 'feeds versions its schema with PRAGMA user_version';
			PRAGMA user_version = 0`), "schema version 8"},
		"from a newer domus": {changed(createNewDatabase, "PRAGMA user_version = 1000"), "newer than"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			path := test.create(t)
			raw := openRaw(t, path)
			before := snapshotOf(t, raw)

			s, err := Open(ctx, path)
			if err == nil {
				_ = s.Close()
				t.Fatal("the database was opened")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want it to say %q", err, test.want)
			}
			if after := snapshotOf(t, raw); !reflect.DeepEqual(after, before) {
				t.Fatalf("database changed\nbefore: %v\n after: %v", before, after)
			}
		})
	}
}

// An older domus may be creating its tables while this one starts. A check made
// before taking the write lock would still see the database as it was before.
func TestTheBaselineCheckWaitsForOtherWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race.db")
	other := openRaw(t, path)
	mustExec(t, other, "PRAGMA journal_mode = WAL")
	tx, err := other.Begin()
	noError(t, err)
	_, err = tx.Exec("CREATE TABLE tokens (id INTEGER)")
	noError(t, err)

	opened := make(chan error, 1)
	go func() {
		s, err := Open(ctx, path)
		if err == nil {
			_ = s.Close()
		}
		opened <- err
	}()
	// Long enough for Open to reach the lock, far shorter than its busy_timeout.
	time.Sleep(200 * time.Millisecond)
	noError(t, tx.Commit())

	if err := <-opened; err == nil || !strings.Contains(err.Error(), "schema version 8") {
		t.Fatalf("err = %v, want a refusal", err)
	}
}

// A second domus may hold the write lock while this one starts.
func TestAnUpToDateDatabaseNeedsNoWriteLock(t *testing.T) {
	s, err := Open(ctx, createNewDatabase(t))
	noError(t, err)
	defer func() { _ = s.Close() }()
	noError(t, s.inTx(ctx, func(*sql.Tx) error {
		return migrate(ctx, s.db)
	}))
}

// The baseline file replaced eight migrations; a lower or higher number would
// skip or repeat it on the databases those left.
func TestTheBaselineComesFirst(t *testing.T) {
	if got := shippedMigrations(t)[0].version; got != baselineVersion {
		t.Fatalf("first migration = %d, want the baseline %d", got, baselineVersion)
	}
}

func TestLaterMigrationsApplyOnceAndInOrder(t *testing.T) {
	db := newRawDatabase(t)
	later := append(onlyBaseline(t),
		migration{version: 9, sql: "CREATE TABLE later (id INTEGER)"},
		migration{version: 10, sql: "ALTER TABLE later ADD COLUMN name TEXT"})

	// Neither statement can run twice.
	for range 2 {
		applyTo(t, db, later)
		if got := schemaVersion(t, db); got != 10 {
			t.Fatalf("user_version = %d", got)
		}
	}
}

// From the baseline on, user_version alone counts: the old records are only for
// databases below it.
func TestLaterMigrationsDoNotNeedTheOldRecords(t *testing.T) {
	db := newRawDatabase(t)
	mustExec(t, db, fmt.Sprintf("CREATE TABLE kept (id INTEGER); PRAGMA user_version = %d", baselineVersion))

	applyTo(t, db, append(onlyBaseline(t), migration{version: 9, sql: "CREATE TABLE later (id INTEGER)"}))

	if got := schemaVersion(t, db); got != 9 {
		t.Fatalf("user_version = %d", got)
	}
}

func TestApplyMigrationSkipsOneAlreadyApplied(t *testing.T) {
	db := newRawDatabase(t)
	applyTo(t, db, onlyBaseline(t))
	done := migration{version: baselineVersion, sql: "this is not SQL"}
	known := versionRange{baseline: baselineVersion, latest: baselineVersion}
	noError(t, applyMigration(context.Background(), db, done, known))
}

// A newer domus may upgrade the database between the first read and the write lock.
func TestApplyMigrationRefusesADatabaseThatBecameNewer(t *testing.T) {
	db := newRawDatabase(t)
	mustExec(t, db, "PRAGMA user_version = 9")
	pending := migration{version: baselineVersion, sql: "this is not SQL"}
	known := versionRange{baseline: baselineVersion, latest: baselineVersion}
	err := applyMigration(context.Background(), db, pending, known)
	if err == nil || !strings.Contains(err.Error(), "newer than") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedMigrationLeavesNothingBehind(t *testing.T) {
	db := newRawDatabase(t)
	broken := append(onlyBaseline(t),
		migration{version: 9, sql: "CREATE TABLE done (id INTEGER)"},
		migration{version: 10, sql: "CREATE TABLE half_done (id INTEGER); INSERT INTO no_such_table VALUES (1);"})

	err := applyMigrations(context.Background(), db, broken)
	if err == nil || !strings.Contains(err.Error(), "migration 10") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := schemaOf(t, db)["half_done"]; ok {
		t.Fatal("the half-applied table survived")
	}
	if got := schemaVersion(t, db); got != 9 {
		t.Fatalf("user_version = %d, want the last migration that succeeded", got)
	}
}

func TestAVersionThatCannotBeRecordedRollsTheMigrationBack(t *testing.T) {
	db := newRawDatabase(t)
	// query_only makes the write of user_version fail after the script ran.
	unrecordable := []migration{{version: 8, sql: "CREATE TABLE unrecorded (id INTEGER); PRAGMA query_only = 1"}}

	if err := applyMigrations(context.Background(), db, unrecordable); err == nil {
		t.Fatal("the migration was reported as applied")
	}
	if _, ok := schemaOf(t, db)["unrecorded"]; ok {
		t.Fatal("the table survived without its version")
	}
}

func TestApplyMigrationsReportsADatabaseItCannotRead(t *testing.T) {
	db := newRawDatabase(t)
	noError(t, db.Close())
	err := applyMigrations(context.Background(), db, onlyBaseline(t))
	if err == nil || !strings.Contains(err.Error(), "read schema version") {
		t.Fatalf("err = %v", err)
	}
}

func TestApplyMigrationStopsWhenCancelled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	known := versionRange{baseline: baselineVersion, latest: baselineVersion}
	err := applyMigration(cancelled, newRawDatabase(t), onlyBaseline(t)[0], known)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

// Another program's table of the same name, without the columns sqlx gives it.
func TestOpenRefusesOldRecordsItCannotRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	raw := openRaw(t, path)
	mustExec(t, raw, "CREATE TABLE _sqlx_migrations (version INTEGER)")
	before := schemaOf(t, raw)

	s, err := Open(ctx, path)
	if err == nil {
		_ = s.Close()
		t.Fatal("the database was opened")
	}
	if !strings.Contains(err.Error(), "read old migration records") {
		t.Fatalf("err = %v", err)
	}
	if after := schemaOf(t, raw); !maps.Equal(after, before) || schemaVersion(t, raw) != 0 {
		t.Fatalf("database changed: %q", after)
	}
}

// A database error must not read as "no such row".
func TestExistenceChecksReportDatabaseErrors(t *testing.T) {
	s := openMemory(t)
	noError(t, s.Close())
	if found, err := s.GroupExists(ctx, "garmin"); err == nil || found {
		t.Fatalf("found = %v err = %v", found, err)
	}
}

func TestLoadMigrationsReportsAFileItCannotRead(t *testing.T) {
	// A directory named like a migration.
	if _, err := loadMigrations(fstest.MapFS{"0001_directory.sql/nested": {}}); err == nil {
		t.Fatal("the directory was loaded")
	}
}

func TestLoadMigrationsOrdersByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"0010_late_one.sql":  {Data: []byte("SELECT 10;")},
		"0002_early.sql":     {Data: []byte("SELECT 2;")},
		"2147483647_end.sql": {Data: []byte("SELECT 3;")},
		"notes.txt":          {Data: []byte("ignored")},
	}
	got, err := loadMigrations(fsys)
	noError(t, err)
	want := []migration{{2, "SELECT 2;"}, {10, "SELECT 10;"}, {2147483647, "SELECT 3;"}}
	if !slices.Equal(got, want) {
		t.Fatalf("migrations = %+v, want %+v", got, want)
	}
}

func TestLoadMigrationsRejectsBadNames(t *testing.T) {
	// user_version holds 1 to 2147483647 here.
	names := []string{"init.sql", "x_init.sql", "0001.sql", "0000_zero.sql", "2147483648_big.sql"}
	for _, name := range names {
		fsys := fstest.MapFS{name: {Data: []byte("SELECT 1;")}}
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestLoadMigrationsRejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_a.sql":  {Data: []byte("SELECT 1;")},
		"00001_b.sql": {Data: []byte("SELECT 1;")},
	}
	if _, err := loadMigrations(fsys); err == nil {
		t.Fatal("two files with version 1 were accepted")
	}
}

func TestLoadMigrationsRejectsAnEmptySet(t *testing.T) {
	if _, err := loadMigrations(fstest.MapFS{}); err == nil {
		t.Fatal("no files were accepted")
	}
}

func TestFileDatabaseUsesWALAndMemoryDoesNot(t *testing.T) {
	file, err := Open(context.Background(), filepath.Join(t.TempDir(), "domus.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	pragma := func(s *Store, name string) string {
		var value string
		if err := s.db.QueryRow("PRAGMA " + name).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if got := pragma(file, "journal_mode"); got != "wal" {
		t.Fatalf("file journal_mode = %s", got)
	}
	if got := pragma(file, "synchronous"); got != "2" {
		t.Fatalf("file synchronous = %s, want 2 (FULL)", got)
	}
	if got := pragma(file, "busy_timeout"); got != "5000" {
		t.Fatalf("file busy_timeout = %s", got)
	}
	memory := openMemory(t)
	if got := pragma(memory, "journal_mode"); got != "memory" {
		t.Fatalf("memory journal_mode = %s", got)
	}
	for name, s := range map[string]*Store{"file": file, "memory": memory} {
		if got := pragma(s, "foreign_keys"); got != "1" {
			t.Fatalf("%s foreign_keys = %s", name, got)
		}
	}
}

func TestPingAnswersUntilClosed(t *testing.T) {
	s := openMemory(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if err := s.Ping(context.Background()); err == nil {
		t.Fatal("a closed store must not answer")
	}
}
