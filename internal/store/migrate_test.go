package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
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

// rawRows reads the migration rows of a database file without the store.
func rawRows(t *testing.T, path string) []migrationRow {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	return readMigrationRows(t, db)
}

// copyFixture copies a committed database into a temp dir; the fixtures are
// never opened in place.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "domus.db")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
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

func TestNewDatabaseRecordsTheEightMigrationsWithRustChecksums(t *testing.T) {
	s := openMemory(t)
	got := readMigrationRows(t, s.db)
	want := rawRows(t, copyFixture(t, "rust-created.db"))
	if len(got) != 8 {
		t.Fatalf("got %d migrations, want 8", len(got))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows differ from the ones sqlx wrote\n got %v\nwant %v", got, want)
	}
}

func TestRustCreatedDatabaseOpensWithoutChangingItsMigrations(t *testing.T) {
	path := copyFixture(t, "rust-created.db")
	before := rawRows(t, path)
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var users int64
	if err := s.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 4 {
		t.Fatalf("users = %d, want 4", users)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after := rawRows(t, path); !reflect.DeepEqual(before, after) {
		t.Fatalf("migrations changed\nbefore %v\n after %v", before, after)
	}
}

func TestRustCreatedExecutionTimesSurviveReopen(t *testing.T) {
	path := copyFixture(t, "rust-created.db")
	times := func() string {
		db, err := sql.Open("sqlite", "file:"+path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		var got string
		rows, err := db.Query("SELECT installed_on || ':' || execution_time FROM _sqlx_migrations ORDER BY version")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var one string
			if err := rows.Scan(&one); err != nil {
				t.Fatal(err)
			}
			got += one + ";"
		}
		return got
	}
	before := times()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if after := times(); before != after {
		t.Fatalf("bookkeeping rewritten\nbefore %s\n after %s", before, after)
	}
}

func TestV010DatabaseIsUpgradedAndKeepsItsData(t *testing.T) {
	path := copyFixture(t, "v0.1.0.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if n := len(readMigrationRows(t, s.db)); n != 8 {
		t.Fatalf("migrations = %d, want 8", n)
	}
	count := func(query string) int64 {
		var n int64
		if err := s.db.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	checks := map[string]int64{
		"SELECT COUNT(*) FROM users WHERE id = 1 AND username = 'admin' AND is_admin = 1": 1,
		"SELECT COUNT(*) FROM tokens WHERE user_id = 1":                                   2,
		"SELECT COUNT(*) FROM sessions":                                                   0,
		"SELECT COUNT(*) FROM hue WHERE ip = '10.0.0.2'":                                  1,
		"SELECT COUNT(*) FROM groups WHERE expose_light = 0":                              2,
		"SELECT COUNT(*) FROM group_members":                                              4,
	}
	for query, want := range checks {
		if got := count(query); got != want {
			t.Errorf("%s = %d, want %d", query, got, want)
		}
	}
	var members string
	err = s.db.QueryRow(
		"SELECT group_concat(entity_id) FROM (SELECT entity_id FROM group_members WHERE group_name = 'garmin' ORDER BY pos)").
		Scan(&members)
	if err != nil {
		t.Fatal(err)
	}
	if want := "light.hue_a1000002,scene.hue_b2000001,light.hue_a1000001"; members != want {
		t.Fatalf("garmin members = %s, want %s", members, want)
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
