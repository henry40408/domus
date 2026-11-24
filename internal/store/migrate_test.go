package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/henry40408/domus/internal/auth"
)

// migrationRow is a _sqlx_migrations row without installed_on and
// execution_time, which differ on every run.
type migrationRow struct {
	Version     int64
	Description string
	Success     bool
	Checksum    []byte
}

func readMigrationRows(t *testing.T, db *sql.DB) []migrationRow {
	t.Helper()
	rows, err := db.Query("SELECT version, description, success, checksum FROM _sqlx_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var result []migrationRow
	for rows.Next() {
		var row migrationRow
		if err := rows.Scan(&row.Version, &row.Description, &row.Success, &row.Checksum); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// sqlxMigrations are the rows sqlx wrote for the eight migrations (the
// checksum is the SHA-384 of the file). A database made by the Rust server
// carries them, and the Rust server refuses a database whose rows differ, so
// they must never change.
var sqlxMigrations = []struct {
	version     int64
	description string
	checksum    string
}{
	{1, "init",
		"f1c1edc563f1af8136b3c41943f87342cc33c9906900271c579096cc0a11ed4d85ec55860164b1e34747ec2013066af0"},
	{2, "token scope",
		"396b2d6c782a42eb87a834750cff99ff600317d85e499db7feeb975fb953dbc8b1e3c96031488cafe7e3f870dcdd2900"},
	{3, "users",
		"56e4e6ecfac3a5f43026eea2a1314aaa0e19aef6b3f2e9a11f5cdc42de664f368bd1bb5bc0189298295aa947f41ef988"},
	{4, "least privilege",
		"300723a68449afbe90b7ea1fc93c8e3d8328fb1c0477da99aff62e9db6c8cd9306ddb0bf02aa106d4f9edf0781ed72ca"},
	{5, "session meta",
		"e5a75dd116428e79d514574a51df5acb898b7fc9496fe549abe0d668a3e1667b9041b9e964b0dba3fb1b99e05c0571cb"},
	{6, "refresh tokens",
		"42a7bc12b708eaa5cd6ca686e78e5bd8ebe794b1451b40d46e7ac62e83090c20c15a7be190a05f82c6255cdcb081fdfa"},
	{7, "registrations",
		"415b6945ee8c2faa09c673df53f6cf42e916a0f71ea3aea00cbee7e7fdc7bbde8965da20d7b2b48f574a5a97e4073df2"},
	{8, "app sessions",
		"e790fa3a766a010e9d9fbac4affd3741854bc3c2b046ef1884e9c323781e70ed622ae08a03272d3d00ce762e699f3a61"},
}

func sqlxMigrationRows(t *testing.T) []migrationRow {
	t.Helper()
	rows := make([]migrationRow, 0, len(sqlxMigrations))
	for _, m := range sqlxMigrations {
		checksum, err := hex.DecodeString(m.checksum)
		noError(t, err)
		rows = append(rows, migrationRow{Version: m.version, Description: m.description, Success: true, Checksum: checksum})
	}
	return rows
}

// bookkeeping is every column of _sqlx_migrations, one string per row.
func bookkeeping(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := queryAll(ctx, s.db, func(row rowScanner) (string, error) {
		var line string
		return line, row.Scan(&line)
	}, `SELECT version || '|' || description || '|' || success || '|' || hex(checksum) || '|' ||
	           installed_on || '|' || execution_time
	    FROM _sqlx_migrations ORDER BY version`)
	noError(t, err)
	return rows
}

func openMemory(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestANewDatabaseRecordsTheEightMigrationsWithSqlxChecksums(t *testing.T) {
	got := readMigrationRows(t, openMemory(t).db)
	want := sqlxMigrationRows(t)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows differ from the ones sqlx wrote\n got %v\nwant %v", got, want)
	}
}

// sqlx stamps every row with the time it ran and how long it took; a database
// it migrated keeps those columns, and reopening it must not rewrite any.
func TestReopeningADatabaseMigratedBySqlxKeepsEveryBookkeepingColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domus.db")
	s, err := Open(ctx, path)
	noError(t, err)
	_, err = s.CreateFirstAdmin(ctx, "root", "hash")
	noError(t, err)
	exec(t, s, "UPDATE _sqlx_migrations SET installed_on = '2026-11-17 06:01:43', execution_time = 1000 + version")
	before := bookkeeping(t, s)
	noError(t, s.Close())

	s, err = Open(ctx, path)
	noError(t, err)
	defer func() { _ = s.Close() }()
	if after := bookkeeping(t, s); !reflect.DeepEqual(before, after) {
		t.Fatalf("bookkeeping rewritten\nbefore %s\n after %s", before, after)
	}
	if got := readMigrationRows(t, s.db); !reflect.DeepEqual(got, sqlxMigrationRows(t)) {
		t.Fatalf("rows differ from the ones sqlx wrote: %v", got)
	}
	users, err := s.UserCount(ctx)
	noError(t, err)
	if users != 1 {
		t.Fatalf("users = %d, want 1", users)
	}
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
INSERT INTO owner VALUES (1, 'v010-owner-hash');
INSERT INTO sessions VALUES ('v010-session', 4102444800);
INSERT INTO hue VALUES (1, '10.0.0.2', 'v010-app-key');
INSERT INTO groups VALUES ('garmin'), ('old');
INSERT INTO group_members VALUES
    ('garmin', 'light.hue_a1000002', 0), ('garmin', 'scene.hue_b2000001', 1),
    ('garmin', 'light.hue_a1000001', 2), ('old', 'light.hue_a1000003', 0);`

var v010Tokens = []struct{ name, plaintext string }{
	{"watch", "domus_v010fixturetoken000000000000"},
	{"phone", "domus_v010fixtureother0000000000000"},
}

// createV010Database writes the v0.1.0 database (journal mode delete, as
// then) and returns its path.
func createV010Database(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "domus.db")
	db, err := sql.Open("sqlite", "file:"+path)
	noError(t, err)
	defer func() { _ = db.Close() }()
	_, err = db.Exec(v010Script)
	noError(t, err)
	for _, token := range v010Tokens {
		_, err = db.Exec("INSERT INTO tokens (name, token_hash, created) VALUES (?1, ?2, 1700000000)",
			token.name, auth.SHA256Hex(token.plaintext))
		noError(t, err)
	}
	return path
}

func TestV010DatabaseIsUpgradedAndKeepsItsData(t *testing.T) {
	before := nowSecs()
	s, err := Open(ctx, createV010Database(t))
	noError(t, err)
	defer func() { _ = s.Close() }()

	if got := readMigrationRows(t, s.db); !reflect.DeepEqual(got, sqlxMigrationRows(t)) {
		t.Fatalf("migrations = %v, want the eight sqlx rows", got)
	}
	assertV010OwnerBecameAdmin(t, s, before)
	assertV010TokensStillAuthenticate(t, s)
	if n := queryInt(t, s, "SELECT COUNT(*) FROM sessions"); n != 0 {
		t.Fatalf("sessions = %d, want them emptied so everyone logs in with a username", n)
	}
	assertV010BridgeAndGroups(t, s)
}

func assertV010OwnerBecameAdmin(t *testing.T, s *Store, notBefore int64) {
	t.Helper()
	admin, hash, err := s.UserLogin(ctx, "admin")
	noError(t, err)
	if admin.ID != 1 || !admin.IsAdmin || hash != "v010-owner-hash" {
		t.Fatalf("admin = %+v hash = %q", admin, hash)
	}
	if admin.Created < notBefore || admin.Created > nowSecs() {
		t.Fatalf("created = %d, want the time of the migration", admin.Created)
	}
}

func assertV010TokensStillAuthenticate(t *testing.T, s *Store) {
	t.Helper()
	for _, token := range v010Tokens {
		got, err := s.TokenAuth(ctx, auth.SHA256Hex(token.plaintext))
		noError(t, err)
		if got.UserID == nil || *got.UserID != 1 || got.ReadOnly || got.Scope != nil {
			t.Errorf("token %s = %+v, want an unrestricted token of user 1", token.name, got)
		}
	}
}

func assertV010BridgeAndGroups(t *testing.T, s *Store) {
	t.Helper()
	bridge, err := s.HueGet(ctx)
	noError(t, err)
	if bridge != (HueBridge{IP: "10.0.0.2", Key: "v010-app-key"}) {
		t.Fatalf("bridge = %+v", bridge)
	}
	names, err := s.GroupNames(ctx)
	noError(t, err)
	if want := []string{"garmin", "old"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("groups = %v, want %v", names, want)
	}
	exposed, err := s.ExposedGroupNames(ctx)
	noError(t, err)
	if len(exposed) != 0 {
		t.Fatalf("exposed groups = %v, want none", exposed)
	}
	members := map[string][]string{
		"garmin": {"light.hue_a1000002", "scene.hue_b2000001", "light.hue_a1000001"},
		"old":    {"light.hue_a1000003"},
	}
	for group, want := range members {
		got, err := s.GroupMembers(ctx, group)
		noError(t, err)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s members = %v, want %v", group, got, want)
		}
	}
}

func TestReopeningAFileDatabaseDoesNotRerunMigrations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domus.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE _sqlx_migrations SET execution_time = 12345"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var untouched int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM _sqlx_migrations WHERE execution_time = 12345").Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	if untouched != 8 {
		t.Fatalf("%d rows kept their time, want 8", untouched)
	}
}

func TestOpenFailsOnAnInconsistentMigrationTable(t *testing.T) {
	cases := map[string]struct {
		tamper string
		want   string
	}{
		"changed checksum": {"UPDATE _sqlx_migrations SET checksum = x'00' WHERE version = 3", "migration 3 was previously applied but has been modified"},
		"dirty row":        {"UPDATE _sqlx_migrations SET success = false WHERE version = 5", "migration 5 is partially applied"},
		"unknown version": {
			"INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time) VALUES (9, 'later', 1, x'01', 0)",
			"migration 9 was previously applied but is missing",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "domus.db")
			s, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(tc.tamper); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()
			if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
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

func TestEmbeddedMigrationsAreTheEightSqlxFiles(t *testing.T) {
	migrations, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range migrations {
		got = append(got, m.description)
		if strings.Contains(m.sql, "\r") {
			t.Errorf("migration %d has CRLF line endings; its checksum would not match", m.version)
		}
	}
	want := []string{"init", "token scope", "users", "least privilege", "session meta", "refresh tokens", "registrations", "app sessions"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descriptions = %v, want %v", got, want)
	}
}

func TestLoadMigrationsParsesNamesLikeSqlx(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0010_add_things_now.sql": {Data: []byte("SELECT 1;")},
		"m/2_two.sql":               {Data: []byte("SELECT 2;")},
		"m/README.md":               {Data: []byte("not a migration")},
		"m/nounderscore.sql":        {Data: []byte("SELECT 3;")},
		"m/3_notsql.txt":            {Data: []byte("SELECT 4;")},
	}
	migrations, err := loadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) != 2 || migrations[0].version != 2 || migrations[1].version != 10 {
		t.Fatalf("migrations = %+v", migrations)
	}
	if migrations[1].description != "add things now" {
		t.Fatalf("description = %q", migrations[1].description)
	}
	if len(migrations[0].checksum) != 48 {
		t.Fatalf("checksum is %d bytes, want 48 (SHA-384)", len(migrations[0].checksum))
	}
}

func TestLoadMigrationsRejectsANonIntegerVersion(t *testing.T) {
	fsys := fstest.MapFS{"m/abc_thing.sql": {Data: []byte("SELECT 1;")}}
	if _, err := loadMigrations(fsys, "m"); err == nil {
		t.Fatal("expected an error")
	}
}

func TestAFailingMigrationLeavesNothingBehind(t *testing.T) {
	s := openMemory(t)
	bad := []migration{{version: 100, description: "bad", sql: "CREATE TABLE half (a INTEGER); SELECT * FROM missing_table;", checksum: []byte{1}}}
	all, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	err = migrate(context.Background(), s.db, append(all, bad...))
	if err == nil || !strings.Contains(err.Error(), "while executing migration 100") {
		t.Fatalf("err = %v", err)
	}
	var tables int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'half'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatal("the half-applied migration was not rolled back")
	}
	if n := len(readMigrationRows(t, s.db)); n != 8 {
		t.Fatalf("rows = %d, want 8", n)
	}
}

func TestPendingMigrationsApplyInVersionOrderAndRecordTheirTime(t *testing.T) {
	s := openMemory(t)
	extra := []migration{
		{version: 20, description: "second", sql: "CREATE TABLE b (x INTEGER);", checksum: []byte{2}},
		{version: 10, description: "first", sql: "CREATE TABLE a (x INTEGER);", checksum: []byte{1}},
	}
	all, err := loadMigrations(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	// migrate takes the list sorted, as loadMigrations returns it.
	all = append(all, extra[1], extra[0])
	if err := migrate(context.Background(), s.db, all); err != nil {
		t.Fatal(err)
	}
	var times int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM _sqlx_migrations WHERE version >= 10 AND execution_time > 0").Scan(&times); err != nil {
		t.Fatal(err)
	}
	if times != 2 {
		t.Fatalf("%d new rows have a time, want 2", times)
	}
}

func TestUpgradeUnversionedAddsTheMissingColumns(t *testing.T) {
	ctx := context.Background()
	db, err := openMemoryPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`CREATE TABLE groups (name TEXT PRIMARY KEY);
	    CREATE TABLE tokens (id INTEGER PRIMARY KEY, name TEXT);
	    INSERT INTO groups (name) VALUES ('old');`); err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second pass finds nothing to do
		if err := upgradeUnversioned(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	var exposed, lastUsed int64
	if err := db.QueryRow("SELECT expose_light FROM groups WHERE name = 'old'").Scan(&exposed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('tokens') WHERE name = 'last_used'").Scan(&lastUsed); err != nil {
		t.Fatal(err)
	}
	if exposed != 0 || lastUsed != 1 {
		t.Fatalf("expose_light = %d, last_used columns = %d", exposed, lastUsed)
	}
}

func TestUpgradeUnversionedLeavesAFreshDatabaseAlone(t *testing.T) {
	ctx := context.Background()
	db, err := openMemoryPool(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := upgradeUnversioned(ctx, db); err != nil {
		t.Fatal(err)
	}
	var tables int64
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("%d objects created in an empty database", tables)
	}
}
