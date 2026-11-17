package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigrationsAreOrderedByVersionNotByFileName(t *testing.T) {
	fsys := fstest.MapFS{
		"m/10_ten.sql": {Data: []byte("SELECT 1;")},
		"m/2_two.sql":  {Data: []byte("SELECT 1;")},
	}
	got, err := loadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].version != 2 || got[1].version != 10 {
		t.Fatalf("order %+v, want 2 then 10", got)
	}
}

func TestTheLowestUnknownAppliedMigrationIsReportedFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "domus.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{12, 9, 10} {
		_, err := s.db.Exec("INSERT INTO _sqlx_migrations (version, description, success, checksum, execution_time) VALUES (?1, 'later', 1, x'01', 0)", version)
		if err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	_, err = Open(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "migration 9 was previously applied but is missing") {
		t.Fatalf("err = %v, want it to name migration 9", err)
	}
}
