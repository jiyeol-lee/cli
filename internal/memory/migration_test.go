package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/jiyeol-lee/cli/internal/database"
)

func TestMigrateIsIdempotentAndUsesIndependentLedger(t *testing.T) {
	db := testDatabase(t)
	ctx := context.Background()
	otherFS := fstest.MapFS{"migrations/0001_create_other.sql": {Data: []byte("CREATE TABLE other__items (id INTEGER PRIMARY KEY)")}}
	if err := database.Migrate(ctx, db, "other", otherFS, "migrations"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Migrate(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.Query(`SELECT app, version, name FROM cli__schema_migrations ORDER BY app`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var records []string
	for rows.Next() {
		var app, name string
		var version int
		if err := rows.Scan(&app, &version, &name); err != nil {
			t.Fatal(err)
		}
		records = append(records, app+":"+name)
	}
	if len(records) != 2 || records[0] != "memory:0001_create_memories.sql" || records[1] != "other:0001_create_other.sql" {
		t.Fatalf("ledger = %#v", records)
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'index' AND name LIKE 'memory__memories_%_archived_idx'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 2 {
		t.Fatalf("memory indexes = %d, want 2", indexes)
	}
}

func TestMigrationEnforcesCrossFieldChecks(t *testing.T) {
	db := testDatabase(t)
	if err := Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`INSERT INTO memory__memories(scope, project_directory, category, memory) VALUES ('global', '/project', 'note', 'x')`,
		`INSERT INTO memory__memories(scope, project_directory, category, memory) VALUES ('project', NULL, 'note', 'x')`,
		`INSERT INTO memory__memories(scope, project_directory, category, memory) VALUES ('global', NULL, 'other', 'x')`,
		`INSERT INTO memory__memories(scope, project_directory, category, memory) VALUES ('global', NULL, 'note', ' ')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err == nil {
			t.Fatalf("constraint accepted %q", statement)
		}
	}
}

func testDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenPermanent(filepath.Join(t.TempDir(), "cli.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
