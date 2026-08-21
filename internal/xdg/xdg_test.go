package xdg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveAndPaths(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user")
	d, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := d.PermanentDB(), filepath.Join("/data", "cli", "cli.sqlite3"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, want := d.GoogleToken(), filepath.Join("/data", "cli", "google", "oauth-token.json"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got, err := d.RuntimeApp("voca"); err != nil || got != filepath.Join("/run/user", "cli", "voca") {
		t.Fatalf("RuntimeApp = %q, %v", got, err)
	}
}

func TestDataHomeMustBeAbsolute(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "unset", data: ""},
		{name: "relative", data: "relative/data"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_DATA_HOME", tt.data)
			if _, err := Resolve(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRelativeRuntimeDirIsUnavailable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "relative/run")
	d, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RuntimeApp("voca"); err == nil {
		t.Fatal("expected relative runtime directory to be rejected")
	}
}

func TestEnsurePrivateDirCorrectsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePrivateDir(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0700 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRuntimeRequiresEnvironment(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", "")
	d, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.RuntimeDB(); err == nil {
		t.Fatal("expected error")
	}
}
