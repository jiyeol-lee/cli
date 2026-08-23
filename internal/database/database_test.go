package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenPermanentPragmas(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "cli.sqlite3")
	db, err := OpenPermanent(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var foreignKeys int
	if err := db.QueryRowContext(context.Background(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d", foreignKeys)
	}
	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q", journal)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fileInfo.Mode().Perm() != 0600 {
		t.Fatalf("database mode = %o", fileInfo.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0700 {
		t.Fatalf("database directory mode = %o", dirInfo.Mode().Perm())
	}
}

func TestOpenPermanentEncodesFileURIPath(t *testing.T) {
	tests := []struct {
		name      string
		component string
	}{
		{name: "question mark", component: "data?local"},
		{name: "fragment", component: "data#local"},
		{name: "escaped slash", component: "data%2Flocal"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), test.component, "cli.sqlite3")
			db, err := OpenPermanent(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec("CREATE TABLE entries (value TEXT NOT NULL)"); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if _, err := db.Exec("INSERT INTO entries (value) VALUES ('saved')"); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("stat intended database path: %v", err)
			}

			db, err = OpenPermanent(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var value string
			if err := db.QueryRow("SELECT value FROM entries").Scan(&value); err != nil {
				t.Fatal(err)
			}
			if value != "saved" {
				t.Fatalf("value = %q", value)
			}
		})
	}
}
