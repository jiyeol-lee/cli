package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"testing/fstest"
)

func TestMigrateConcurrentFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.sqlite3")
	dbs := openMigrationTestHandles(t, path)
	migrations := fstest.MapFS{
		"migrations/0001_create_table.sql": {Data: []byte(`CREATE TABLE example (id INTEGER PRIMARY KEY)`)},
	}

	runConcurrently(t, func(i int) error {
		return Migrate(context.Background(), dbs[i], "example", migrations, "migrations")
	})

	assertSchemaCount(t, dbs[0], "example", 1)
	assertLedgerCount(t, dbs[0], "example", 1)
}

func TestAdoptBaselineConcurrentFirstRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.sqlite3")
	dbs := openMigrationTestHandles(t, path)
	if _, err := dbs[0].Exec(`CREATE TABLE legacy_example (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	validate := func(ctx context.Context, query BaselineQueryer) (bool, error) {
		var count int
		err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'legacy_example'`).Scan(&count)
		return count == 1, err
	}

	runConcurrently(t, func(i int) error {
		return AdoptBaseline(context.Background(), dbs[i], "example", 1, "0001_legacy.sql", validate)
	})

	assertSchemaCount(t, dbs[0], "cli__schema_migrations", 1)
	assertLedgerCount(t, dbs[0], "example", 1)
}

func openMigrationTestHandles(t *testing.T, path string) [2]*sql.DB {
	t.Helper()
	var dbs [2]*sql.DB
	for i := range dbs {
		db, err := OpenPermanent(path)
		if err != nil {
			t.Fatal(err)
		}
		dbs[i] = db
		t.Cleanup(func() { db.Close() })
	}
	return dbs
}

func runConcurrently(t *testing.T, run func(int) error) {
	t.Helper()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i := range 2 {
		go func() {
			ready.Done()
			<-start
			errs <- run(i)
		}()
	}
	ready.Wait()
	close(start)
	var results [2]error
	for i := range results {
		results[i] = <-errs
	}
	for _, err := range results {
		if err != nil {
			t.Error(err)
		}
	}
}

func assertSchemaCount(t *testing.T, db *sql.DB, table string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("schema entries for %s = %d, want %d", table, count, want)
	}
}

func assertLedgerCount(t *testing.T, db *sql.DB, app string, want int) {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cli__schema_migrations WHERE app = ?`, app).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != want {
		t.Fatalf("ledger rows for %s = %d, want %d", app, count, want)
	}
}

func migrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := OpenPermanent(t.TempDir() + "/cli.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrateFreshAndRepeat(t *testing.T) {
	db := migrationTestDB(t)
	migrations := fstest.MapFS{
		"migrations/0002_add_value.sql":    {Data: []byte(`ALTER TABLE example ADD COLUMN value TEXT`)},
		"migrations/0001_create_table.sql": {Data: []byte(`CREATE TABLE example (id INTEGER PRIMARY KEY)`)},
	}

	for range 2 {
		if err := Migrate(context.Background(), db, "example", migrations, "migrations"); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.Query(`SELECT app, version, name, applied_at FROM cli__schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantNames := []string{"0001_create_table.sql", "0002_add_value.sql"}
	for version, wantName := range wantNames {
		if !rows.Next() {
			t.Fatalf("missing ledger row for version %d", version+1)
		}
		var app, name, appliedAt string
		var gotVersion int
		if err := rows.Scan(&app, &gotVersion, &name, &appliedAt); err != nil {
			t.Fatal(err)
		}
		if app != "example" || gotVersion != version+1 || name != wantName || appliedAt == "" {
			t.Fatalf("ledger row = app %q, version %d, name %q, applied_at %q", app, gotVersion, name, appliedAt)
		}
	}
	if rows.Next() {
		t.Fatal("repeat migration added a ledger row")
	}
}

func TestMigrateKeepsIndependentAppVersions(t *testing.T) {
	db := migrationTestDB(t)
	ctx := context.Background()
	for _, app := range []string{"alpha", "beta"} {
		migrationFS := fstest.MapFS{
			"migrations/0001_create_table.sql": {Data: []byte(`CREATE TABLE ` + app + ` (id INTEGER)`)},
		}
		if err := Migrate(ctx, db, app, migrationFS, "migrations"); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cli__schema_migrations WHERE version = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("version 1 ledger rows = %d, want 2", count)
	}
}

func TestMigrateRejectsInvalidFiles(t *testing.T) {
	tests := []struct {
		name string
		fs   fstest.MapFS
	}{
		{name: "malformed filename", fs: fstest.MapFS{"migrations/create.sql": {Data: []byte(`SELECT 1`)}}},
		{name: "zero version", fs: fstest.MapFS{"migrations/0000_create.sql": {Data: []byte(`SELECT 1`)}}},
		{name: "empty SQL", fs: fstest.MapFS{"migrations/0001_create.sql": {Data: []byte(" \n")}}},
		{name: "duplicate version", fs: fstest.MapFS{
			"migrations/0001_create.sql": {Data: []byte(`SELECT 1`)},
			"migrations/0001_update.sql": {Data: []byte(`SELECT 2`)},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Migrate(context.Background(), migrationTestDB(t), "example", tt.fs, "migrations"); err == nil {
				t.Fatal("expected migration validation error")
			}
		})
	}
}

func TestMigrateRollsBackFailedMigration(t *testing.T) {
	db := migrationTestDB(t)
	migrations := fstest.MapFS{
		"migrations/0001_broken.sql": {Data: []byte(`CREATE TABLE should_rollback (id INTEGER); INSERT INTO missing_table VALUES (1)`)},
	}
	if err := Migrate(context.Background(), db, "example", migrations, "migrations"); err == nil {
		t.Fatal("expected migration failure")
	}

	var tableCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'should_rollback'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}
	if tableCount != 0 {
		t.Fatal("failed migration was not rolled back")
	}
	var ledgerCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cli__schema_migrations WHERE app = ?`, "example").Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}
	if ledgerCount != 0 {
		t.Fatal("failed migration was recorded")
	}
}

func TestMigrateKeepsEarlierMigrationWhenLaterMigrationFails(t *testing.T) {
	db := migrationTestDB(t)
	migrations := fstest.MapFS{
		"migrations/0001_create_first.sql": {Data: []byte(`CREATE TABLE first_table (id INTEGER)`)},
		"migrations/0002_broken.sql":       {Data: []byte(`CREATE TABLE second_table (id INTEGER); INSERT INTO missing_table VALUES (1)`)},
	}
	if err := Migrate(context.Background(), db, "example", migrations, "migrations"); err == nil {
		t.Fatal("expected second migration to fail")
	}

	assertSchemaCount(t, db, "first_table", 1)
	assertSchemaCount(t, db, "second_table", 0)
	assertLedgerCount(t, db, "example", 1)
}

func TestMigrateRejectsNonPrefixHistory(t *testing.T) {
	tests := []struct {
		name            string
		loaded          fstest.MapFS
		recordedVersion int
		recordedName    string
	}{
		{
			name: "unknown version",
			loaded: fstest.MapFS{
				"migrations/0001_create.sql": {Data: []byte(`SELECT 1`)},
			},
			recordedVersion: 9,
			recordedName:    "0009_unknown.sql",
		},
		{
			name: "name mismatch",
			loaded: fstest.MapFS{
				"migrations/0001_create.sql": {Data: []byte(`SELECT 1`)},
			},
			recordedVersion: 1,
			recordedName:    "0001_other.sql",
		},
		{
			name: "missing earlier migration",
			loaded: fstest.MapFS{
				"migrations/0001_create.sql": {Data: []byte(`SELECT 1`)},
				"migrations/0002_update.sql": {Data: []byte(`SELECT 2`)},
			},
			recordedVersion: 2,
			recordedName:    "0002_update.sql",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := migrationTestDB(t)
			if _, err := db.Exec(createMigrationLedgerSQL); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(insertMigrationSQL, "example", tt.recordedVersion, tt.recordedName, "2026-01-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(context.Background(), db, "example", tt.loaded, "migrations"); err == nil {
				t.Fatal("expected migration history error")
			}
		})
	}
}

func TestMigrateAcceptsPrefixWithSkippedVersionNumber(t *testing.T) {
	db := migrationTestDB(t)
	if _, err := db.Exec(createMigrationLedgerSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(insertMigrationSQL, "example", 1, "0001_create.sql", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{
		"migrations/0001_create.sql": {Data: []byte(`SELECT 1`)},
		"migrations/0003_update.sql": {Data: []byte(`CREATE TABLE skipped_version_is_valid (id INTEGER)`)},
	}
	if err := Migrate(context.Background(), db, "example", migrations, "migrations"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cli__schema_migrations WHERE app = 'example' AND version = 3`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("version 3 ledger rows = %d, want 1", count)
	}
}
