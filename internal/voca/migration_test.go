package voca

import (
	"context"
	"database/sql"
	"testing"

	"github.com/jiyeol-lee/cli/internal/database"
)

const legacyVocabularySchemaSQL = `CREATE TABLE voca__vocabulary (
	id INTEGER PRIMARY KEY,
	word TEXT NOT NULL UNIQUE,
	read_count INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

func TestMigrateAdoptsValidLegacyVocabularySchema(t *testing.T) {
	db := migrationTestDatabase(t)
	if _, err := db.Exec(legacyVocabularySchemaSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO voca__vocabulary(word, created_at, updated_at) VALUES ('legacy', 'now', 'now')`); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if err := Migrate(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	var version, rows int
	var name string
	if err := db.QueryRow(`SELECT version, name FROM cli__schema_migrations WHERE app = 'voca'`).Scan(&version, &name); err != nil {
		t.Fatal(err)
	}
	if version != 1 || name != "0001_create_vocabulary.sql" {
		t.Fatalf("ledger row = version %d, name %q", version, name)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM voca__vocabulary`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("legacy rows = %d, want 1", rows)
	}
}

func TestMigrateRejectsInvalidLegacyVocabularySchema(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{
			name: "missing column",
			schema: `CREATE TABLE voca__vocabulary (
				id INTEGER PRIMARY KEY,
				word TEXT NOT NULL UNIQUE,
				read_count INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL
			)`,
		},
		{
			name: "missing uniqueness",
			schema: `CREATE TABLE voca__vocabulary (
				id INTEGER PRIMARY KEY,
				word TEXT NOT NULL,
				read_count INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL,
				updated_at TEXT NOT NULL
			)`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := migrationTestDatabase(t)
			if _, err := db.Exec(tt.schema); err != nil {
				t.Fatal(err)
			}
			if err := Migrate(context.Background(), db); err == nil {
				t.Fatal("expected legacy schema validation error")
			}
			var ledgerRows int
			if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'cli__schema_migrations'`).Scan(&ledgerRows); err != nil {
				t.Fatal(err)
			}
			if ledgerRows != 0 {
				t.Fatal("invalid legacy schema created a migration ledger")
			}
		})
	}
}

func migrationTestDatabase(t *testing.T) *sql.DB {
	t.Helper()
	db, err := database.OpenPermanent(t.TempDir() + "/cli.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
