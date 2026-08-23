package xdg

import (
	"fmt"
	"os"
	"path/filepath"
)

type Dirs struct {
	DataRoot    string
	RuntimeRoot string
}

func Resolve() (Dirs, error) {
	data := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(data) {
		return Dirs{}, fmt.Errorf("XDG_DATA_HOME is not set")
	}
	return Dirs{DataRoot: filepath.Join(data, "cli"), RuntimeRoot: runtimeRoot()}, nil
}

func runtimeRoot() string {
	if root := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(root) {
		return filepath.Join(root, "cli")
	}
	return ""
}

func (d Dirs) DataApp(app string) string {
	return filepath.Join(d.DataRoot, app)
}

func (d Dirs) RuntimeApp(app string) (string, error) {
	if d.RuntimeRoot == "" {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(d.RuntimeRoot, app), nil
}

func (d Dirs) PermanentDB() string {
	return filepath.Join(d.DataRoot, "cli.sqlite3")
}

func (d Dirs) RuntimeDB() (string, error) {
	if d.RuntimeRoot == "" {
		return "", fmt.Errorf("XDG_RUNTIME_DIR is not set")
	}
	return filepath.Join(d.RuntimeRoot, "cli.sqlite3"), nil
}

func (d Dirs) GoogleToken() string {
	return filepath.Join(d.DataRoot, "google", "oauth-token.json")
}

func EnsurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}
