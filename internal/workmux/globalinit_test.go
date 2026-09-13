package workmux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGlobalInitParser(t *testing.T) {
	for _, flag := range []string{"-g", "--global"} {
		for _, args := range [][]string{{"init", flag}, {"init", flag, "--help"}, {"init", flag, "--"}} {
			got, err := ParseCommand(args)
			if err != nil || !got.Global || got.Kind != "init" {
				t.Fatalf("%v = %+v, %v", args, got, err)
			}
		}
		for _, args := range [][]string{
			{"init", flag, "--global"}, {"init", flag, "repo"}, {"init", flag + "=true"}, {"init", "--", flag},
			{"sandbox", "build", flag}, {"sandbox", "shell", flag},
			{"add", "topic", flag}, {"merge", flag}, {"remove", flag}, {"open", "topic", flag}, {"close", flag},
		} {
			if _, err := ParseCommand(args); err == nil {
				t.Fatalf("accepted %v", args)
			}
		}
	}
	if !strings.Contains(Usage, "init [-g|--global]") {
		t.Fatal("help omits global init")
	}
}

func TestGlobalInitOutsideGit(t *testing.T) {
	for _, flag := range []string{"-g", "--global"} {
		t.Run(flag, func(t *testing.T) {
			home := t.TempDir()
			var output bytes.Buffer
			app := App{HomeDir: home, StateDir: filepath.Join(home, "state"), Stdout: &output,
				Getwd: func() (string, error) { return home, nil },
				Runner: sandboxCommandRunner(func(_ context.Context, p Process) ([]byte, error) {
					t.Fatalf("global init ran external command: %+v", p)
					return nil, nil
				}),
			}
			command, err := ParseCommand([]string{"init", flag})
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, ".config", "cli", "workmux")
			path := filepath.Join(dir, "config.yaml")
			data, err := os.ReadFile(path)
			if err != nil || string(data) != globalInitExample || output.String() != path+"\n" {
				t.Fatalf("created config = %q, output = %q, %v", data, &output, err)
			}
			for _, path := range []string{filepath.Join(home, ".config"), filepath.Dir(dir), dir, path} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				want := os.FileMode(0700)
				if !info.IsDir() {
					want = 0600
				}
				if info.Mode().Perm() != want {
					t.Fatalf("%s permissions = %o", path, info.Mode().Perm())
				}
			}
			config, err := loadGlobalConfig(dir)
			if err != nil || config.Sandbox.Enabled || !reflect.DeepEqual(config.Panes, defaultPanes()) || config.PostCreate != nil {
				t.Fatalf("init enabled settings: %+v, %v", config, err)
			}
			if err := os.WriteFile(path, []byte("# preserve user edits\n"), 0600); err != nil {
				t.Fatal(err)
			}
			output.Reset()
			if err := app.Run(t.Context(), command); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing config = %v", err)
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "# preserve user edits\n" || output.Len() != 0 {
				t.Fatalf("changed existing config: %q, %v", data, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(home, "missing")
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), command); !errors.Is(err, os.ErrExist) {
				t.Fatalf("existing symlink = %v", err)
			}
			for _, path := range []string{target, app.StateDir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("unexpected creation of %s: %v", path, err)
				}
			}
		})
	}
}

func TestGlobalInitRepositoryOverrides(t *testing.T) {
	dir, root := t.TempDir(), filepath.Join(t.TempDir(), "project")
	writeConfigFixture(t, dir, "config.yaml", `panes: [{command: global}]
files:
  copy: [global-copy]
  symlink: [global-link]
post_create: [global-create]
pre_merge: [global-merge]
pre_remove: [global-remove]
layouts:
  review: {panes: [{command: global-review}]}
  inherited: {panes: [{command: inherited}]}
sandbox:
  enabled: true
  image: localhost/global:latest
  target: all
`)
	writeConfigFixture(t, dir, "project.yaml", `panes: [{command: repository}]
files:
  copy: []
  symlink: null
post_create: [repository-create]
pre_merge: ["<global>", repository-merge]
pre_remove: null
layouts:
  review: {panes: [{command: repository-review}]}
sandbox:
  enabled: false
  image: null
  target: agent
`)
	config, err := LoadConfigForRepo(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Panes:      []Pane{{Command: "repository"}},
		Files:      FilesConfig{Copy: []string{}, Symlink: []string{"global-link"}},
		PostCreate: []string{"repository-create"}, PreMerge: []string{"global-merge", "repository-merge"}, PreRemove: []string{"global-remove"},
		Layouts: map[string]Layout{"review": {Panes: []Pane{{Command: "repository-review"}}}, "inherited": {Panes: []Pane{{Command: "inherited"}}}},
		Sandbox: SandboxConfig{Enabled: false, Image: "localhost/global:latest", Target: "agent"},
	}
	if !reflect.DeepEqual(config, want) {
		t.Fatalf("merged config = %#v, want %#v", config, want)
	}
}
