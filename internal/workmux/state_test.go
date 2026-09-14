package workmux

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func hostStateStore(t *testing.T, f *hostFixture) *stateStore {
	t.Helper()
	repo, err := (gitHost{runner: f.runner}).discover(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	store, err := lockState(f.state, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.unlock(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func TestStatePrivateAtomicAndNonblockingLock(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	if extra, err := lockState(f.state, store.repo); err == nil {
		if err := extra.unlock(); err != nil {
			t.Error(err)
		}
		t.Fatal("second nonblocking lock succeeded")
	}
	states, err := store.load()
	if err != nil || len(states) != 1 {
		t.Fatalf("load = %+v, %v", states, err)
	}
	path := filepath.Join(store.dir, states[0].ID+".json")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	states[0].Stage = "closed"
	if err := store.save(states[0]); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Fatal("state save was not an atomic replacement")
	}
	for _, path := range []string{f.state, store.dir} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("private directory mode for %s: %v %v", path, info, err)
		}
	}
	for _, path := range []string{path, filepath.Join(store.dir, "lock")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private file mode for %s: %v %v", path, info, err)
		}
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	if err := store.save(states[0]); err == nil {
		t.Fatal("saved without lock")
	}
	second, err := lockState(f.state, store.repo)
	if err != nil {
		t.Fatal("lock was not released", err)
	}
	if err := second.unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestStateRejectsRestoredPathAndIdentityInjection(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	for _, test := range []struct {
		name   string
		mutate func(*workspaceState)
	}{
		{"different repository", func(w *workspaceState) { w.CommonDir = "/other/.git" }},
		{"different root", func(w *workspaceState) { w.Root = "/other" }},
		{"path traversal", func(w *workspaceState) { w.Path = filepath.Join(w.Path, "..", "elsewhere") }},
		{"main worktree", func(w *workspaceState) { w.Path = w.Root }},
		{"handle traversal", func(w *workspaceState) { w.Handle = "../topic" }},
		{"option branch", func(w *workspaceState) { w.Branch = "-delete" }},
		{"window injection", func(w *workspaceState) { w.Window = "@1;kill-server" }},
		{"container injection", func(w *workspaceState) { w.Container = "unowned" }},
		{"id mismatch", func(w *workspaceState) { w.ID = strings.Repeat("a", 32) }},
		{"unknown stage", func(w *workspaceState) { w.Stage = "unknown" }},
		{"bad commit", func(w *workspaceState) { w.MergedCommit = "--all" }},
		{"kept merge without success", func(w *workspaceState) { w.MergeKept = true }},
		{"kept merge during cleanup", func(w *workspaceState) {
			w.MergeKept, w.MergedCommit = true, w.InitialCommit
			w.Removal = &removalState{Head: w.InitialCommit}
		}},
		{"removal checkpoint before shutdown", func(w *workspaceState) {
			w.Removal = &removalState{Head: w.InitialCommit, HookDone: true, WorktreeRemovalStarted: true}
		}},
		{"removal checkpoint before hook", func(w *workspaceState) {
			w.Removal = &removalState{Head: w.InitialCommit, Stopped: true, WorktreeRemovalStarted: true}
		}},
		{"ignored traversal", func(w *workspaceState) { w.OwnedIgnored = map[string]string{"../secrets": "hash"} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := f.load(t, "topic")
			test.mutate(&state)
			if err := store.validate(state); err == nil {
				t.Fatalf("accepted injected state %+v", state)
			}
		})
	}
}

func TestStateRejectsLinkedAndPublicFiles(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			store := hostStateStore(t, f)
			state := f.load(t, "topic")
			path := filepath.Join(store.dir, state.ID+".json")
			copy := filepath.Join(f.home, "state-copy")
			if kind == "public" {
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(path, copy); err != nil {
					t.Fatal(err)
				}
				var err error
				if kind == "symlink" {
					err = os.Symlink(copy, path)
				} else {
					err = os.Link(copy, path)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.load(); err == nil {
				t.Fatal("loaded unsafe state file")
			}
			if err := store.save(state); err == nil {
				t.Fatal("overwrote unsafe state file")
			}
		})
	}
}

func TestStateRejectsSymlinkedWorktreeParent(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	state := f.load(t, "topic")
	parent := filepath.Dir(state.Path)
	relocated := parent + "-relocated"
	if err := os.Rename(parent, relocated); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relocated, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.load(); err == nil {
		t.Fatal("accepted linked workspace parent")
	}
}

func TestStateRejectsForeignRecordAndTrailingData(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	state := f.load(t, "topic")
	path := filepath.Join(store.dir, state.ID+".json")
	state.RepoID = strings.Repeat("0", 32)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	hostWrite(t, path, string(data))
	if _, err := store.load(); err == nil {
		t.Fatal("loaded record from wrong repository")
	}
	hostWrite(t, path, string(data)+"{}")
	if _, err := readState(path); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func stateTestSandboxJSON(t *testing.T, state workspaceState, sandbox string) string {
	t.Helper()
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(raw["config"], &config); err != nil {
		t.Fatal(err)
	}
	config["sandbox"] = json.RawMessage(sandbox)
	raw["config"], err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestStateLoadsLegacySandboxAndWritesNewShape(t *testing.T) {
	for _, test := range []struct {
		name, container string
		disabled        bool
		removed         bool
	}{
		{name: "podman", container: `{"runtime":"podman"}`},
		{name: "empty runtime", container: `{"runtime":""}`},
		{name: "default runtime", container: `{}`},
		{name: "no container"},
		{name: "disabled podman", container: `{"runtime":"podman"}`, disabled: true},
		{name: "disabled previous backend", container: `{"runtime":"docker"}`, disabled: true},
		{name: "disabled unknown backend", container: `{"runtime":"unknown"}`, disabled: true},
		{name: "zero config tombstone", container: `{"runtime":""}`, disabled: true, removed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, fmt.Sprintf("sandbox: {enabled: %t}\n", !test.disabled))
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			state := f.load(t, "topic")
			if test.removed {
				state.Stage, state.Config = "removed", Config{}
			}
			sandbox := fmt.Sprintf(`{"enabled":%t,"image":%q`, state.Config.Sandbox.Enabled, state.Config.Sandbox.Image)
			if test.container != "" {
				sandbox += `,"container":` + test.container
			}
			store := hostStateStore(t, f)
			path := filepath.Join(store.dir, state.ID+".json")
			hostWrite(t, path, stateTestSandboxJSON(t, state, sandbox+"}"))
			states, err := store.load()
			if err != nil || len(states) != 1 {
				t.Fatalf("load legacy state: %+v, %v", states, err)
			}
			if !reflect.DeepEqual(states[0], state) {
				t.Fatalf("legacy normalization changed workspace: %+v, want %+v", states[0], state)
			}
			if err := store.save(states[0]); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			want, err := json.Marshal(state.Config.Sandbox)
			if err != nil || !strings.Contains(string(data), `"sandbox":`+string(want)) || strings.Contains(string(data), `"runtime"`) {
				t.Fatalf("saved obsolete sandbox shape: %s, %v", data, err)
			}
		})
	}
}

func TestStateRejectsLegacyBackendBeforeCleanup(t *testing.T) {
	for _, backend := range []string{"docker", "unknown", "podman --remote"} {
		t.Run(backend, func(t *testing.T) {
			f := newHostFixture(t, "sandbox: {enabled: true}\n")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			state := f.load(t, "topic")
			path := filepath.Join(f.state, state.RepoID, state.ID+".json")
			data := stateTestSandboxJSON(t, state, fmt.Sprintf(`{"enabled":true,"image":%q,"container":{"runtime":%q}}`, state.Config.Sandbox.Image, backend))
			hostWrite(t, path, data)
			engine := &sandboxTestEngine{}
			f.app.Sandbox = &Containers{Runner: engine, HomeDir: f.home, StateDir: f.state}
			for _, command := range []string{"open", "close", "merge", "remove"} {
				f.events = nil
				if err := f.run(t, command, "topic"); err == nil || !strings.Contains(err.Error(), "unsupported previous sandbox backend") || !strings.Contains(err.Error(), backend) {
					t.Fatalf("%s legacy backend error = %v", command, err)
				}
				if len(engine.calls) != 0 || len(f.events) != 0 {
					t.Fatalf("legacy backend reached cleanup: %+v, %v", engine.calls, f.events)
				}
			}
			if _, err := os.Stat(state.Path); err != nil {
				t.Fatal("legacy backend rejection removed worktree", err)
			}
			if current, err := os.ReadFile(path); err != nil || string(current) != data {
				t.Fatalf("legacy backend rejection rewrote state: %s, %v", current, err)
			}
		})
	}
}

func TestStateLegacySandboxDecodingRemainsStrict(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	store := hostStateStore(t, f)
	path := filepath.Join(store.dir, state.ID+".json")
	for _, test := range []struct{ name, sandbox, suffix, message string }{
		{"sandbox unknown", `{"enabled":false,"image":"localhost/test","other":true}`, "", `unknown field "other"`},
		{"legacy unknown", `{"enabled":true,"image":"localhost/test","container":{"runtime":"podman","other":true}}`, "", `unknown field "other"`},
		{"disabled legacy unknown", `{"enabled":false,"image":"localhost/test","container":{"runtime":"docker","other":true}}`, "", `unknown field "other"`},
		{"direct runtime", `{"enabled":false,"image":"localhost/test","runtime":"podman"}`, "", `unknown field "runtime"`},
		{"invalid legacy type", `{"enabled":false,"image":"localhost/test","container":{"runtime":1}}`, "", "cannot unmarshal"},
		{"trailing document", `{"enabled":true,"image":"localhost/test","container":{"runtime":"podman"}}`, "{}", "trailing data"},
		{"trailing garbage", `{"enabled":true,"image":"localhost/test","container":{"runtime":"podman"}}`, "garbage", "trailing data"},
	} {
		t.Run(test.name, func(t *testing.T) {
			hostWrite(t, path, stateTestSandboxJSON(t, state, test.sandbox)+test.suffix)
			if _, err := store.load(); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("strict legacy state error = %v, want %q", err, test.message)
			}
		})
	}
	data := stateTestSandboxJSON(t, state, `{"enabled":false,"image":"localhost/test","container":{"runtime":"podman"}}`)
	for _, key := range []string{"id", "config", "panes", "image"} {
		t.Run("unknown at "+key, func(t *testing.T) {
			hostWrite(t, path, strings.Replace(data, `"`+key+`":`, `"unexpected":`, 1))
			if _, err := store.load(); err == nil || !strings.Contains(err.Error(), `unknown field "unexpected"`) {
				t.Fatalf("accepted unknown state field: %v", err)
			}
		})
	}
}

func TestStateLegacyPodmanKeepsContainerIdentity(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	w.Container = "cli-workmux-" + w.ID
	plan, err := c.mountPlan(t.Context(), w, engine.image, true)
	if err != nil {
		t.Fatal(err)
	}
	legacyPlan := plan
	legacyPlan.Fingerprint = ""
	legacyPlan.Mounts = slices.Clone(plan.Mounts)
	for i := range legacyPlan.Mounts {
		if legacyPlan.Mounts[i].Snapshot {
			legacyPlan.Mounts[i].Source = legacyPlan.Mounts[i].Target
		}
	}
	data, err := json.Marshal(struct {
		Policy, Image, Engine, State string
		UID, GID                     int
		Plan                         sandboxMountPlan
	}{"2", engine.image, "podman", c.StateDir, os.Getuid(), os.Getgid(), legacyPlan})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := plan.snapshots(true); err != nil {
		t.Fatal(err)
	}
	if err := c.endpoint(w, sandboxEngine{Endpoint: sandboxTestEndpoint()}, true); err != nil {
		t.Fatal(err)
	}
	found := &sandboxInspection{ID: strings.Repeat("a", 64), Name: "/" + w.Container, Image: engine.image}
	found.State.Running = true
	found.Config.Labels = make(map[string]string)
	for key, value := range map[string]string{
		"owner": "cli-workmux", "policy": "2", "workspace": w.ID, "repository": w.RepoID,
		"root": w.Root, "common": w.CommonDir, "path": w.Path, "runtime": "podman", "image": w.Config.Sandbox.Image,
		"endpoint": sandboxTestEndpoint(), "image-id": engine.image, "mounts": fingerprint,
	} {
		found.Config.Labels[sandboxLabel+key] = value
	}
	engine.container = found
	path := filepath.Join(c.StateDir, "legacy.json")
	hostWrite(t, path, stateTestSandboxJSON(t, workspaceState{Workspace: w}, fmt.Sprintf(`{"enabled":true,"image":%q,"container":{"runtime":"podman"}}`, w.Config.Sandbox.Image)))
	state, err := readState(path)
	if err != nil {
		t.Fatal(err)
	}
	engine.calls = nil
	if err := c.Ensure(t.Context(), state.Workspace); err == nil || !strings.Contains(err.Error(), "preserved") {
		t.Fatalf("legacy migration must preserve container data: %v", err)
	}
	if engine.container != found {
		t.Fatal("replaced legacy Podman container")
	}
	for _, call := range sandboxEngineCalls(engine) {
		if args := sandboxTestCallArgs(call); args[0] == "create" || args[0] == "start" {
			t.Fatalf("legacy Podman container was recreated or restarted: %v", args)
		}
	}
	argv, err := c.PaneCommand(t.Context(), state.Workspace, "opencode")
	if err == nil || len(argv) != 0 || engine.container != found {
		t.Fatalf("legacy migration must not launch or delete a container: %q, %v", argv, err)
	}
	if err := c.Stop(t.Context(), state.Workspace); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(t.Context(), state.Workspace); err != nil {
		t.Fatal(err)
	}
}

func TestHostDefaultDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "/not-the-config-dir")
	app := App{HomeDir: home}
	if err := app.defaults(); err != nil {
		t.Fatal(err)
	}
	if app.StateDir != filepath.Join(home, ".local", "state", "cli", "workmux") || app.ConfigDir != filepath.Join(home, ".config", "cli", "workmux") {
		t.Fatalf("wrong default directories: %+v", app)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "custom"))
	app = App{HomeDir: home}
	if err := app.defaults(); err != nil || app.StateDir != filepath.Join(home, "custom", "cli", "workmux") {
		t.Fatalf("XDG state resolution: %+v, %v", app, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if err := (&App{HomeDir: home}).defaults(); err == nil {
		t.Fatal("accepted relative XDG state home")
	}
}

func TestStateLegacyPaneSnapshotDoesNotBlockFreshConfiguration(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	state.Config.Panes = []Pane{{Size: 10, Focus: true}, {Focus: true}}
	state.Layout = "deleted-layout"
	store := hostStateStore(t, f)
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
}

func TestStateLegacyImplicitZeroSizeDoesNotBecomeCurrentConfig(t *testing.T) {
	f := newHostFixture(t, "panes: [{}, {split: horizontal}]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	store := hostStateStore(t, f)
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	config := saved["config"].(map[string]any)
	config["panes"] = []any{map[string]any{"size": 0}, map[string]any{"size": 0}}
	data, err = json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	hostWrite(t, filepath.Join(store.dir, state.ID+".json"), string(data))
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal(err)
	}
	for _, pane := range f.mux.panes {
		if pane.SizeSpecified() {
			t.Fatal("old implicit size zero leaked into fresh panes")
		}
	}
}

func TestLegacyContainerExplicitKeepBranchMigration(t *testing.T) {
	f := newHostFixture(t, "sandbox: {enabled: true}\npanes: [{command: opencode}]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	state.Container, state.SandboxUsed = "cli-workmux-"+state.ID, false
	store := hostStateStore(t, f)
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "legacy-backup")
	hostWrite(t, backup, "user backed up container-local data\n")
	head := hostGit(t, state.Path, "rev-parse", "HEAD")
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal(err)
	}
	f.sandbox.ensureErr = fmt.Errorf("legacy container preserved; explicit migration required")
	if err := f.run(t, "open", "topic"); err == nil {
		t.Fatal("legacy container was silently migrated")
	}
	if !f.sandbox.present || slices.Contains(f.events, "sandbox:remove") {
		t.Fatal("failed migration deleted legacy resources")
	}
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal(err)
	}
	if hostGit(t, f.root, "rev-parse", "topic") != head {
		t.Fatal("explicit migration lost branch history")
	}
	f.sandbox.ensureErr = nil
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Container != "" || !state.SandboxUsed {
		t.Fatalf("recreated workspace did not select ephemeral sandbox: %+v", state)
	}
	if data, err := os.ReadFile(backup); err != nil || string(data) != "user backed up container-local data\n" {
		t.Fatalf("external backup changed: %q, %v", data, err)
	}
}
