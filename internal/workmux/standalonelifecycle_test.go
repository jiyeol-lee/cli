package workmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestStandaloneLifecycleCheckoutDiscovery(t *testing.T) {
	sandboxTestNonroot(t)
	for _, checkout := range []string{"main", "linked"} {
		t.Run(checkout, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			root := w.Root
			if checkout == "linked" {
				root = w.Path
			}
			cwd := filepath.Join(root, "nested", "directory")
			if err := os.MkdirAll(cwd, 0700); err != nil {
				t.Fatal(err)
			}
			called := false
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				podman := sandboxTestPodmanProcess(p)
				if podman.Name == "podman" && podman.Args[0] == "run" {
					called = true
					index := slices.Index(podman.Args, "--workdir")
					if index < 0 || index+1 >= len(podman.Args) || podman.Args[index+1] != root {
						t.Fatalf("checkout %s workdir = %q, want %s", checkout, podman.Args, root)
					}
					found := sandboxTestSession(t, engine, podman.Args)
					if found.Config.Labels[sandboxLabel+"path"] != root || found.Config.Labels[sandboxLabel+"root"] != w.Root || found.Config.Labels[sandboxLabel+"common"] != w.CommonDir {
						t.Fatalf("checkout identity = %v", found.Config.Labels)
					}
				}
				return engine.Run(ctx, p)
			})
			app := sandboxCommandApp(c, w)
			app.Getwd = func() (string, error) { return cwd, nil }
			var stdout, stderr bytes.Buffer
			app.Stdout, app.Stderr = &stdout, &stderr
			command, err := ParseCommand([]string{"sandbox", "run", "--", "printf", "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if !called || len(engine.sessions) != 0 {
				t.Fatalf("run called = %t, live sessions = %d", called, len(engine.sessions))
			}
		})
	}
}

func standaloneLifecycleSnapshots(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[path] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("standalone run has no mount snapshots")
	}
	return files
}

func TestStandaloneLifecycleCleanupFailures(t *testing.T) {
	sandboxTestNonroot(t)
	for _, test := range []struct {
		name, label, failure, want string
	}{
		{name: "owner", label: "owner", want: "ownership label owner"},
		{name: "policy", label: "policy", want: "ownership label policy"},
		{name: "workspace", label: "workspace", want: "ownership label workspace"},
		{name: "repository", label: "repository", want: "ownership label repository"},
		{name: "root", label: "root", want: "ownership label root"},
		{name: "common", label: "common", want: "ownership label common"},
		{name: "path", label: "path", want: "ownership label path"},
		{name: "runtime", label: "runtime", want: "ownership label runtime"},
		{name: "source", label: "source", want: "ownership label source"},
		{name: "standalone", label: "standalone", want: "ownership label standalone"},
		{name: "image identity", label: "image-id", want: "image identity labels"},
		{name: "endpoint", label: "endpoint", want: "endpoint changed"},
		{name: "session", label: "session", want: "session identity changed"},
		{name: "inspection", failure: "container inspect", want: "inspect sandbox"},
		{name: "removal", failure: "rm", want: "rm sandbox"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			expected := fmt.Errorf("%s failed", test.name)
			var found *sandboxInspection
			var registrationPath, snapshotDir string
			var registration []byte
			var snapshots map[string][]byte
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				podman := sandboxTestPodmanProcess(p)
				if podman.Name == "podman" && podman.Args[0] == "run" {
					found = sandboxTestSession(t, engine, podman.Args)
					session := found.Config.Labels[sandboxLabel+"session"]
					registrationPath = filepath.Join(standaloneDirectory(c.StateDir, w), session+".json")
					var err error
					registration, err = os.ReadFile(registrationPath)
					if err != nil {
						t.Fatal(err)
					}
					var run standaloneRun
					if err := json.Unmarshal(registration, &run); err != nil {
						t.Fatal(err)
					}
					if run.Session != session || run.Workspace.Path != w.Path || run.Workspace.CommonDir != w.CommonDir || run.Endpoint == "" || run.Endpoint != found.Config.Labels[sandboxLabel+"endpoint"] {
						t.Fatalf("standalone registration = %+v", run)
					}
					snapshotDir = filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(w), "runs", session)
					snapshots = standaloneLifecycleSnapshots(t, snapshotDir)
					if err := c.checkStandalone(ctx, w); err == nil || !strings.Contains(err.Error(), "active standalone sandbox") {
						t.Fatalf("active registration check = %v", err)
					}
					if test.label != "" {
						found.Config.Labels[sandboxLabel+test.label] = "foreign"
					}
					if test.failure != "" {
						engine.fail[test.failure] = expected
					}
				}
				return engine.Run(ctx, p)
			})
			var stderr bytes.Buffer
			err := c.RunStandalone(t.Context(), w, []string{"printf", "hello"}, nil, nil, &stderr, nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("cleanup error = %v, want %q", err, test.want)
			}
			if test.failure != "" && !errors.Is(err, expected) {
				t.Fatalf("cleanup lost engine error: %v", err)
			}
			if found == nil || len(engine.sessions) != 1 || engine.sessions[0] != found || !found.State.Running {
				t.Fatal("failed cleanup removed or stopped the container")
			}
			data, err := os.ReadFile(registrationPath)
			if err != nil || !bytes.Equal(data, registration) {
				t.Fatalf("failed cleanup changed registration: %s, %v", data, err)
			}
			retained := standaloneLifecycleSnapshots(t, snapshotDir)
			if len(retained) != len(snapshots) {
				t.Fatalf("retained snapshots = %d, want %d", len(retained), len(snapshots))
			}
			for path, data := range snapshots {
				if current, ok := retained[path]; !ok || !bytes.Equal(current, data) {
					t.Fatalf("failed cleanup changed snapshot %s", path)
				}
			}
			if err := c.checkStandalone(t.Context(), w); err == nil {
				t.Fatal("failed cleanup no longer blocks checkout removal")
			}
			removals := 0
			for _, p := range sandboxEngineCalls(engine) {
				if p.Args[0] == "stop" {
					t.Fatal("standalone cleanup stopped the container")
				}
				if p.Args[0] == "rm" {
					removals++
					if test.failure != "rm" || !slices.Equal(p.Args, []string{"rm", "--force", found.ID}) {
						t.Fatalf("unsafe cleanup removal: %q", p.Args)
					}
				}
			}
			wantRemovals := 0
			if test.failure == "rm" {
				wantRemovals = 1
			}
			if removals != wantRemovals {
				t.Fatalf("removal attempts = %d, want %d", removals, wantRemovals)
			}
		})
	}
}

func TestStandaloneLifecycleModelSeedLeafSymlink(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	target := filepath.Join(c.HomeDir, "host-model.json")
	const content = "{\"recent\":[{\"providerID\":\"openai\",\"modelID\":\"test\"}]}\n"
	sandboxTestWrite(t, target, content)
	source := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
	if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, source); err != nil {
		t.Fatal(err)
	}
	called := false
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" && podman.Args[0] == "run" {
			called = true
			if !slices.Equal(podman.Args[len(podman.Args)-3:], []string{"bash", "-c", "printf hello"}) {
				t.Fatalf("symlinked seed changed command: %q", podman.Args)
			}
			sandboxTestSession(t, engine, podman.Args)
		}
		return engine.Run(ctx, p)
	})
	var stderr bytes.Buffer
	if err := c.RunStandalone(t.Context(), w, []string{"printf", "hello"}, nil, nil, &stderr, nil); err != nil {
		t.Fatal(err)
	}
	if !called || len(engine.sessions) != 0 {
		t.Fatalf("run called = %t, live sessions = %d", called, len(engine.sessions))
	}
	if !strings.HasPrefix(stderr.String(), "warning: sandbox model seed:") {
		t.Fatalf("missing seed warning: %q", stderr.String())
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != content {
		t.Fatalf("host seed target changed: %s, %v", data, err)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("host seed target permissions changed: %v, %v", info, err)
	}
	if link, err := os.Readlink(source); err != nil || link != target {
		t.Fatalf("host seed symlink changed: %q, %v", link, err)
	}
}
