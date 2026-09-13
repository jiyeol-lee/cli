package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const sandboxTestInfo = `{"Host":{"DatabaseBackend":"sqlite","Security":{"Rootless":true}},"Store":{"GraphRoot":"/tmp/cli-workmux-engine/store","RunRoot":"/tmp/cli-workmux-engine/run","GraphDriverName":"overlay","ConfigFile":"/tmp/cli-workmux-engine/storage.conf"}}`

type sandboxTestEngine struct {
	calls                                  []Process
	rawCalls                               []Process
	container                              *sandboxInspection
	sessions                               []*sandboxInspection
	image, info, inspection, listing, home string
	fail                                   map[string]error
	disappear                              bool
}

func sandboxTestCallArgs(call Process) []string { return call.Args }

func sandboxTestPodmanProcess(p Process) Process {
	if p.Name == "/usr/bin/env" {
		if index := slices.Index(p.Args, "podman"); index >= 0 {
			p.Name, p.Args = "podman", p.Args[index+1:]
		}
	}
	return p
}

func sandboxTestEndpoint() string {
	scope := []string{"podman", strconv.Itoa(os.Getuid()), "/tmp/cli-workmux-engine/store", "/tmp/cli-workmux-engine/run", "overlay", "/tmp/cli-workmux-engine/storage.conf", "sqlite", "false"}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(scope, "\x00"))))
}

func (engine *sandboxTestEngine) Run(ctx context.Context, p Process) ([]byte, error) {
	engine.rawCalls = append(engine.rawCalls, p)
	p = sandboxTestPodmanProcess(p)
	engine.calls = append(engine.calls, p)
	if p.Name == "git" {
		p.Env = append(p.Env, "HOME="+engine.home, "XDG_CONFIG_HOME="+filepath.Join(engine.home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		return (ExecRunner{}).Run(ctx, p)
	}
	if p.Name != "podman" || p.Dir != "/" || len(p.Args) == 0 {
		return nil, fmt.Errorf("unexpected engine process: %+v", p)
	}
	key := p.Args[0]
	if key == "container" || key == "image" {
		key += " " + p.Args[1]
	}
	if engine.disappear && (key == "stop" || key == "rm") {
		engine.sessions = nil
		return nil, fmt.Errorf("session already removed")
	}
	if err := engine.fail[key]; err != nil {
		return nil, err
	}
	all := slices.Clone(engine.sessions)
	if engine.container != nil {
		all = append(all, engine.container)
	}
	switch key {
	case "info":
		return []byte(engine.info), nil
	case "image inspect":
		return json.Marshal([]map[string]string{{"Id": engine.image}})
	case "container inspect":
		if engine.inspection != "" {
			return []byte(engine.inspection), nil
		}
		for _, found := range all {
			if strings.TrimPrefix(found.Name, "/") == p.Args[2] {
				return json.Marshal([]*sandboxInspection{found})
			}
		}
		return nil, fmt.Errorf("no such container")
	case "container ls":
		if engine.listing != "" {
			return []byte(engine.listing), nil
		}
		var names []string
		for _, found := range all {
			match := found.State.Running || slices.Contains(p.Args, "--all")
			for i, arg := range p.Args {
				if arg != "--filter" {
					continue
				}
				filter := p.Args[i+1]
				if assignment, ok := strings.CutPrefix(filter, "label="); ok {
					label, value, _ := strings.Cut(assignment, "=")
					match = match && found.Config.Labels[label] == value
				} else if strings.HasPrefix(filter, "name=") {
					match = match && strings.Contains(filter, strings.TrimPrefix(found.Name, "/"))
				}
			}
			if match {
				names = append(names, strings.TrimPrefix(found.Name, "/"))
			}
		}
		return []byte(strings.Join(names, "\n")), nil
	case "stop", "rm":
		for _, found := range all {
			if found.ID != p.Args[len(p.Args)-1] {
				continue
			}
			found.State.Running = false
			if found == engine.container {
				if key == "rm" {
					engine.container = nil
				}
			} else {
				engine.sessions = slices.DeleteFunc(engine.sessions, func(candidate *sandboxInspection) bool { return candidate == found })
			}
			return nil, nil
		}
		return nil, fmt.Errorf("action must address inspected container ID")
	case "run":
		if p.Stdin != nil && p.Stdout != nil {
			_, err := io.Copy(p.Stdout, p.Stdin)
			return nil, err
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected test process: %s %v", p.Name, p.Args)
	}
}

func sandboxTestGit(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	out, err := (ExecRunner{}).Run(t.Context(), Process{Name: "git", Args: args, Dir: dir, CleanGitEnv: true, Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}})
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return out
}

func sandboxTestWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func sandboxTestFixture(t *testing.T) (*Containers, Workspace, *sandboxTestEngine) {
	t.Helper()
	base := t.TempDir()
	root, worktree, home := filepath.Join(base, "main"), filepath.Join(base, "linked worktree"), filepath.Join(base, "fake-home")
	for _, dir := range []string{root, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sandboxTestWrite(t, filepath.Join(home, ".config", "opencode", ".gitignore"), "")
	sandboxTestGit(t, root, "init", "-q", "--template=", "--initial-branch=main")
	sandboxTestGit(t, root, "config", "user.name", "Sandbox Test")
	sandboxTestGit(t, root, "config", "user.email", "sandbox@example.invalid")
	sandboxTestWrite(t, filepath.Join(root, "tracked"), "base\n")
	sandboxTestGit(t, root, "add", "tracked")
	sandboxTestGit(t, root, "-c", "commit.gpgsign=false", "commit", "-qm", "base")
	sandboxTestGit(t, root, "worktree", "add", "-qb", "topic", worktree)
	engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo, fail: make(map[string]error), home: home}
	c := &Containers{Runner: engine, HomeDir: home, StateDir: filepath.Join(base, "private-state"), Getenv: func(string) string { return "" }}
	w := Workspace{ID: identity(filepath.Join(root, ".git"), worktree), RepoID: identity(filepath.Join(root, ".git")), Root: root, CommonDir: filepath.Join(root, ".git"), Path: worktree, Branch: "topic", Handle: "topic", Stage: "ready", Config: Config{Sandbox: SandboxConfig{Enabled: true, Image: "localhost/cli-workmux:test"}}}
	return c, w, engine
}

func sandboxEngineCalls(engine *sandboxTestEngine) []Process {
	var calls []Process
	for _, call := range engine.calls {
		if call.Name != "git" {
			calls = append(calls, call)
		}
	}
	return calls
}

func sandboxTestNonroot(t *testing.T) {
	t.Helper()
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Skip("integration fixture requires a nonroot host UID:GID")
	}
}

func sandboxTestSession(t *testing.T, engine *sandboxTestEngine, argv []string) *sandboxInspection {
	t.Helper()
	found := &sandboxInspection{ID: fmt.Sprintf("%064x", len(engine.sessions)+1), Image: engine.image}
	found.State.Running = true
	found.Config.Labels = make(map[string]string)
	for i, arg := range argv {
		switch arg {
		case "--name":
			found.Name = argv[i+1]
		case "--label":
			key, value, _ := strings.Cut(argv[i+1], "=")
			found.Config.Labels[key] = value
		}
	}
	engine.sessions = append(engine.sessions, found)
	return found
}

func sandboxTestLegacy(t *testing.T, c *Containers, w *Workspace, engine *sandboxTestEngine) *sandboxInspection {
	t.Helper()
	w.Container = "cli-workmux-legacy-test"
	found := &sandboxInspection{ID: strings.Repeat("a", 64), Name: w.Container, Image: engine.image}
	found.State.Running = true
	found.Config.Image = w.Config.Sandbox.Image
	found.Config.Labels = make(map[string]string)
	for key, value := range map[string]string{"owner": "cli-workmux", "policy": "2", "workspace": w.ID, "repository": w.RepoID, "root": w.Root, "common": w.CommonDir, "path": w.Path, "runtime": "podman", "image": w.Config.Sandbox.Image, "image-id": engine.image, "mounts": strings.Repeat("c", 64), "endpoint": sandboxTestEndpoint()} {
		found.Config.Labels[sandboxLabel+key] = value
	}
	engine.container = found
	if err := c.endpoint(*w, sandboxEngine{Endpoint: sandboxTestEndpoint()}, true); err != nil {
		t.Fatal(err)
	}
	return found
}

func TestContainersCheck(t *testing.T) {
	for _, test := range []struct {
		name, failure, image string
		disabled, wantErr    bool
	}{
		{name: "Podman"}, {name: "disabled", disabled: true},
		{name: "missing custom image", failure: "image inspect", wantErr: true},
		{name: "bad image identity", image: "sha256:short", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			if test.image != "" {
				engine.image = test.image
			}
			if test.failure != "" {
				engine.fail[test.failure] = fmt.Errorf("unavailable")
			}
			w.Config.Sandbox.Enabled = !test.disabled
			if err := c.Check(t.Context(), w.Config.Sandbox); (err != nil) != test.wantErr {
				t.Fatalf("Check = %v", err)
			}
			if test.disabled && len(engine.calls) != 0 {
				t.Fatal("disabled sandbox invoked engine")
			}
			for _, call := range engine.calls {
				if !slices.Equal(call.Args, []string{"image", "inspect", w.Config.Sandbox.Image}) {
					t.Fatalf("unexpected check: %+v", call)
				}
			}
		})
	}
}

func TestContainersEphemeralPane(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	if err := c.Ensure(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
		t.Fatalf("preflight prepared mounts: %v", err)
	}
	command := `opencode "quoted arg"; printf '%s' '$literal'`
	first, err := c.PaneCommand(t.Context(), w, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.PaneCommand(t.Context(), w, command)
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{first, second} {
		process := sandboxTestPodmanProcess(Process{Name: argv[0], Args: argv[1:]})
		if argv[0] != "/usr/bin/env" || process.Name != "podman" || !slices.Equal(process.Args[:3], []string{"run", "--rm", "-it"}) || !slices.Equal(argv[len(argv)-4:], []string{engine.image, "sh", "-c", command}) {
			t.Fatalf("pane argv = %q", argv)
		}
		for _, part := range [][]string{{"--userns=keep-id"}, {"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}, {"--workdir", w.Path}} {
			i := slices.Index(argv, part[0])
			if i < 0 || !slices.Equal(argv[i:i+len(part)], part) {
				t.Fatalf("missing %q in %q", part, argv)
			}
		}
	}
	if first[slices.Index(first, "--name")+1] == second[slices.Index(second, "--name")+1] {
		t.Fatal("panes share a container name")
	}
	for _, call := range sandboxEngineCalls(engine) {
		if call.Args[0] != "image" {
			t.Fatalf("preparing a pane started a persistent container: %+v", call)
		}
	}
	// A new pane takes a fresh snapshot rather than freezing the first pane's config.
	sandboxTestGit(t, w.Root, "config", "user.name", "Changed")
	if _, err := c.PaneCommand(t.Context(), w, command); err != nil {
		t.Fatalf("new pane rejected changed config: %v", err)
	}
}

func TestContainersPrepareFailsClosed(t *testing.T) {
	for _, change := range []string{"image", "pointer", "disabled", "legacy", "canceled", "command"} {
		t.Run(change, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			command := "opencode"
			switch change {
			case "image":
				engine.fail["image inspect"] = fmt.Errorf("missing image")
			case "pointer":
				sandboxTestWrite(t, filepath.Join(w.Path, ".git"), "invalid")
			case "disabled":
				w.Config.Sandbox.Enabled = false
			case "legacy":
				w.Container = "legacy"
			case "canceled":
				cancel()
			case "command":
				command += "\x00"
			}
			argv, err := c.PaneCommand(ctx, w, command)
			if err == nil || len(argv) != 0 {
				t.Fatalf("prepare returned host-executable fallback: %q, %v", argv, err)
			}
		})
	}
}

func TestContainersExecFinite(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("input")
	env := []string{"WM_BRANCH=topic $(false)", "VALUE=one=two\nthree", "_NUMBER1=1"}
	if err := c.Exec(t.Context(), w, "exit 7", env, stdin, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	last := engine.calls[len(engine.calls)-1]
	if !slices.Equal(last.Args[:3], []string{"run", "--rm", "-i"}) || !slices.Equal(last.Args[len(last.Args)-3:], []string{"sh", "-c", "exit 7"}) || last.Stdin != stdin || last.Stdout != &stdout || last.Stderr != &stderr || stdout.String() != "input" {
		t.Fatalf("Exec process = %+v", last)
	}
	for _, entry := range env {
		if !slices.Contains(last.Args, entry) {
			t.Fatalf("missing environment %q", entry)
		}
	}
	failure := fmt.Errorf("finite process failed")
	engine.fail["run"] = failure
	if err := c.Exec(t.Context(), w, "false", nil, nil, nil, nil); !errors.Is(err, failure) {
		t.Fatalf("lost underlying status: %v", err)
	}
	for _, entry := range []string{"SECRET", "=empty", "--privileged=1", "1KEY=value", "A-B=value", "A\nB=value", "A=value\x00"} {
		engine.calls = nil
		if err := c.Exec(t.Context(), w, "opencode", []string{entry}, nil, nil, nil); err == nil || len(engine.calls) != 0 {
			t.Fatalf("invalid env accepted: %q", entry)
		}
	}
}

func TestContainersConfiguredPodmanRouting(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	for _, key := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "PODMAN_CONNECTIONS_CONF", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_STORAGE_CONF", "STORAGE_DRIVER", "STORAGE_OPTS", "DOCKER_HOST"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "user-selected-routing")
			if err := c.Ensure(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			argv, err := c.PaneCommand(t.Context(), w, "opencode")
			if err != nil || argv[0] != "/usr/bin/env" || !slices.Contains(argv, "podman") || slices.Contains(argv, "--remote=false") {
				t.Fatalf("routing refused or overridden: %q %v", argv, err)
			}
			for _, call := range sandboxEngineCalls(engine) {
				if call.Name != "podman" || len(call.Env) != 0 {
					t.Fatalf("runtime environment overridden: %+v", call)
				}
			}
			last := engine.rawCalls[0]
			if last.Name != "/usr/bin/env" || !slices.Contains(last.Args, "podman") {
				t.Fatalf("preflight did not use structured environment prefix: %+v", last)
			}
		})
	}
}

func TestContainersOwnedSessions(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	for range 2 {
		argv, err := c.PaneCommand(t.Context(), w, "opencode")
		if err != nil {
			t.Fatal(err)
		}
		sandboxTestSession(t, engine, argv)
	}
	foreign := *engine.sessions[0]
	foreign.ID, foreign.Name = "foreign", "unrelated"
	foreign.Config.Labels = map[string]string{sandboxLabel + "owner": "somebody-else"}
	engine.sessions = append(engine.sessions, &foreign)
	if present, err := c.Exists(t.Context(), w); err != nil || !present {
		t.Fatalf("Exists = %v, %v", present, err)
	}
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if len(engine.sessions) != 1 || engine.sessions[0] != &foreign || !foreign.State.Running {
		t.Fatal("stop did not remove only both owned sessions")
	}
	stops := 0
	for _, call := range engine.calls {
		if call.Name == "podman" && len(call.Args) > 0 && call.Args[0] == "stop" {
			stops++
			if len(call.Args) != 4 || !slices.Equal(call.Args[:3], []string{"stop", "-t", "0"}) {
				t.Fatalf("sandbox stop must use upstream's zero-second timeout: %v", call.Args)
			}
		}
	}
	if stops != 2 {
		t.Fatalf("stopped %d sessions, want 2", stops)
	}
	if present, err := c.Exists(t.Context(), w); err != nil || present {
		t.Fatalf("Exists after --rm = %v, %v", present, err)
	}
	if err := c.Remove(t.Context(), w); err != nil {
		t.Fatal(err)
	}
}

func TestContainersValidateEveryOwnershipLabel(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	argv, err := c.PaneCommand(t.Context(), w, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	found := sandboxTestSession(t, engine, argv)
	engine.listing = found.Name
	for key, value := range found.Config.Labels {
		if key == sandboxLabel+"image" {
			continue
		}
		t.Run(key, func(t *testing.T) {
			found.Config.Labels[key] = "foreign"
			defer func() { found.Config.Labels[key] = value }()
			engine.calls = nil
			for _, action := range []func(context.Context, Workspace) error{c.Stop, c.Remove} {
				if err := action(t.Context(), w); err == nil {
					t.Fatal("accepted mismatched ownership")
				}
			}
			for _, call := range sandboxEngineCalls(engine) {
				if call.Args[0] == "stop" || call.Args[0] == "rm" {
					t.Fatalf("acted on unowned resource: %v", call)
				}
			}
		})
	}
}

func TestContainersLegacyPreservation(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	found := sandboxTestLegacy(t, c, &w, engine)
	w.Config.Sandbox.Image = "localhost/new-configured-image"
	for _, action := range []func(context.Context, Workspace) error{c.Ensure, func(ctx context.Context, w Workspace) error { _, err := c.PaneCommand(ctx, w, "opencode"); return err }} {
		if err := action(t.Context(), w); err == nil || !strings.Contains(err.Error(), "legacy persistent sandbox") {
			t.Fatalf("no migration instruction: %v", err)
		}
		if engine.container != found || !found.State.Running {
			t.Fatal("legacy container modified")
		}
	}
	if err := c.Stop(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if engine.container != found || found.State.Running {
		t.Fatal("close deleted legacy data or did not stop")
	}
	if present, err := c.Exists(t.Context(), w); err != nil || !present {
		t.Fatalf("stopped legacy forgotten: %v, %v", present, err)
	}
	if err := c.Remove(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	if engine.container != nil {
		t.Fatal("explicit removal retained legacy resource")
	}
}

func TestContainersLegacyEndpointProtection(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	sandboxTestLegacy(t, c, &w, engine)
	engine.info = strings.Replace(engine.info, "/tmp/cli-workmux-engine/store", "/tmp/different-store", 1)
	engine.container = nil
	if _, err := c.Exists(t.Context(), w); err == nil || !strings.Contains(err.Error(), "legacy sandbox engine identity changed") {
		t.Fatalf("orphaned legacy data: %v", err)
	}
}

func TestContainersMissingVersusUnavailable(t *testing.T) {
	for _, failure := range []string{"container ls", "container inspect"} {
		t.Run(failure, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			engine.listing = "cli-workmux-present"
			engine.fail[failure] = fmt.Errorf("engine unavailable")
			if present, err := c.Exists(t.Context(), w); err == nil || present {
				t.Fatalf("unavailable treated as absent: %v, %v", present, err)
			}
		})
	}
}

func TestContainersProcessesSurviveDeletedCallerDirectory(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	deleted := filepath.Join(t.TempDir(), "cwd")
	if err := os.Mkdir(deleted, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deleted)
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(t.Context(), w, "opencode", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context(), w); err != nil {
		t.Fatal(err)
	}
	for _, call := range engine.calls {
		if call.Dir != "/" {
			t.Fatalf("process inherited deleted cwd: %+v", call)
		}
	}
}

func TestContainersCleanupRacesAndFailures(t *testing.T) {
	for _, action := range []string{"stop", "rm"} {
		for _, disappeared := range []bool{false, true} {
			t.Run(action+" disappeared="+strconv.FormatBool(disappeared), func(t *testing.T) {
				c, w, engine := sandboxTestFixture(t)
				for range 2 {
					argv, err := c.PaneCommand(t.Context(), w, "opencode")
					if err != nil {
						t.Fatal(err)
					}
					sandboxTestSession(t, engine, argv)
				}
				engine.disappear = disappeared
				failure := fmt.Errorf("engine action failed")
				engine.fail[action] = failure
				var err error
				if action == "stop" {
					err = c.Stop(t.Context(), w)
				} else {
					err = c.Remove(t.Context(), w)
				}
				if disappeared && err != nil || !disappeared && !errors.Is(err, failure) {
					t.Fatalf("cleanup result: %v", err)
				}
				attempts := 0
				for _, call := range sandboxEngineCalls(engine) {
					if call.Args[0] == action {
						attempts++
					}
				}
				if attempts != 2 {
					t.Fatalf("cleanup did not attempt every owned session: %d", attempts)
				}
			})
		}
	}
}

func sandboxTestPodmanBinary(t *testing.T, dir, image, body string) {
	t.Helper()
	path := filepath.Join(dir, "podman")
	script := "#!/bin/sh\n"
	if image != "" {
		script += "if [ \"$1\" = image ]; then\n  printf '%s' '[{\"Id\":\"" + image + "\"}]'\n  exit 0\nfi\n"
	}
	sandboxTestWrite(t, path, script+body+"\n")
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestContainersPaneCapturesNativeEngineEnvironment(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	base := filepath.Dir(w.Root)
	binA, binB := filepath.Join(base, "engine-a"), filepath.Join(base, "engine-b")
	sandboxTestPodmanBinary(t, binA, "", "printf 'wrong backend A\\n' >&2\nexit 42")
	sandboxTestPodmanBinary(t, binB, engine.image, "exec /usr/bin/env -0")
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: base}
	selected := make(map[string]string)
	for _, key := range sandboxEngineEnvKeys {
		value := filepath.Join(base, "caller-b", key) + " with 'quotes' $literal; = value"
		if key == "PATH" {
			value = binB + ":/usr/bin:/bin"
		}
		if key == "CONTAINER_CONNECTION" {
			value = "podman-machine-b"
		}
		if key == "CONTAINERS_CONF_OVERRIDE" {
			value = ""
		}
		t.Setenv(key, value)
		selected[key] = value
	}
	if err := os.Unsetenv("CONTAINER_HOST"); err != nil {
		t.Fatal(err)
	}
	delete(selected, "CONTAINER_HOST")
	argv, err := c.PaneCommand(t.Context(), w, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range sandboxEngineEnvKeys {
		t.Setenv(key, "late-server-a")
	}
	t.Setenv("PATH", binA+":/usr/bin:/bin")
	out, err := c.Runner.Run(t.Context(), Process{Name: argv[0], Args: argv[1:], Dir: "/"})
	if err != nil {
		t.Fatalf("prepared command switched backends: %v", err)
	}
	actual := make(map[string]string)
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if key, value, ok := strings.Cut(string(entry), "="); ok {
			actual[key] = value
		}
	}
	for _, key := range sandboxEngineEnvKeys {
		want, present := selected[key]
		got, exists := actual[key]
		if got != want || exists != present {
			t.Fatalf("selector %s = %q, present=%v; want %q, present=%v", key, got, exists, want, present)
		}
	}
	if actual["HOME"] == c.HomeDir {
		t.Fatal("fake OpenCode home replaced native engine home")
	}
	if got, err := c.PaneCommand(t.Context(), w, "opencode"); err == nil || len(got) != 0 || !strings.Contains(err.Error(), "wrong backend A") {
		t.Fatalf("unavailable caller backend passed preflight: %q, %v", got, err)
	}
	sandboxTestPodmanBinary(t, binB, "", "printf 'selected backend B unavailable\\n' >&2\nexit 41")
	marker := filepath.Join(base, "forbidden-host-fallback")
	var stderr bytes.Buffer
	_, err = c.Runner.Run(t.Context(), Process{Name: argv[0], Args: argv[1:], Dir: "/", Stdin: strings.NewReader("touch " + marker + "\n"), Stderr: &stderr})
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 41 || !strings.Contains(stderr.String(), "selected backend B unavailable") {
		t.Fatalf("backend error hidden or launch redirected: %v, %s", err, &stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("failed launch created a host fallback: %v", err)
	}
}

func TestContainersPaneRestoresCallerEngineInTmux(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	base := filepath.Dir(w.Root)
	binA, binB := filepath.Join(base, "engine-a"), filepath.Join(base, "engine-b")
	marker := filepath.Join(base, "selected-engine")
	sandboxTestPodmanBinary(t, binA, "", "printf 'wrong backend A\\n' >&2\nexit 42")
	sandboxTestPodmanBinary(t, binB, engine.image, `printf '%s\n' "$CONTAINER_CONNECTION" "$HOME" "${CONTAINER_HOST-unset}" > `+strconv.Quote(marker))
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: base}
	t.Setenv("CONTAINER_CONNECTION", "machine-a")
	t.Setenv("CONTAINER_HOST", "ssh://server-a.invalid")
	t.Setenv("PATH", binA+":/usr/bin:/bin")
	ctx, runner, _, session := isolatedTmux(t)
	callerHome := filepath.Join(base, "native-caller-home")
	t.Setenv("HOME", callerHome)
	t.Setenv("CONTAINER_CONNECTION", "machine-b")
	if err := os.Unsetenv("CONTAINER_HOST"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binB+":/usr/bin:/bin")
	argv, err := c.PaneCommand(ctx, w, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINER_CONNECTION", "late-machine-a")
	t.Setenv("PATH", binA+":/usr/bin:/bin")
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-g", "remain-on-exit", "on"}}); err != nil {
		t.Fatal(err)
	}
	// Exercise the complete argv in the server's environment without the PTY input queue.
	args := []string{"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", session + ":", "-c", w.Path, "--", "/bin/sh", "-c", nativeShellCommand(argv, "/bin/sh")}
	created, err := runner.Run(ctx, Process{Name: "tmux", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	window := strings.TrimSpace(string(created))
	want := "machine-b\n" + callerHome + "\nunset\n"
	deadline := time.Now().Add(2 * time.Second)
	for {
		out, err := os.ReadFile(marker)
		if err == nil && string(out) == want {
			break
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			capture, captureErr := runner.Run(ctx, Process{Name: "tmux", Args: []string{"capture-pane", "-p", "-t", window}})
			t.Fatalf("tmux launch did not preserve caller B: %q, %v; capture %v:\n%s", out, err, captureErr, capture)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type sandboxRoutingTestRunner struct {
	engines      map[string]*sandboxTestEngine
	afterInspect func()
}

func (runner sandboxRoutingTestRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	connection := os.Getenv("CONTAINER_CONNECTION")
	if p.Name == "/usr/bin/env" {
		connection = ""
		for _, arg := range p.Args {
			if arg == "podman" {
				break
			}
			if value, ok := strings.CutPrefix(arg, "CONTAINER_CONNECTION="); ok {
				connection = value
			}
		}
	}
	engine := runner.engines[connection]
	if engine == nil {
		return nil, fmt.Errorf("unknown test connection %q", connection)
	}
	out, err := engine.Run(ctx, p)
	process := sandboxTestPodmanProcess(p)
	if slices.Equal(process.Args[:2], []string{"container", "inspect"}) && runner.afterInspect != nil {
		runner.afterInspect()
	}
	return out, err
}

func TestContainersCleanupKeepsInspectedConnection(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run("remove="+strconv.FormatBool(remove), func(t *testing.T) {
			c, w, engineB := sandboxTestFixture(t)
			t.Setenv("CONTAINER_CONNECTION", "machine-b")
			argv, err := c.PaneCommand(t.Context(), w, "opencode")
			if err != nil {
				t.Fatal(err)
			}
			owned := sandboxTestSession(t, engineB, argv)
			foreign := *owned
			foreign.Config.Labels = map[string]string{sandboxLabel + "owner": "another-tool"}
			engineA := &sandboxTestEngine{sessions: []*sandboxInspection{&foreign}}
			c.Runner = sandboxRoutingTestRunner{engines: map[string]*sandboxTestEngine{"machine-a": engineA, "machine-b": engineB}, afterInspect: func() { t.Setenv("CONTAINER_CONNECTION", "machine-a") }}
			if remove {
				err = c.Remove(t.Context(), w)
			} else {
				err = c.Stop(t.Context(), w)
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(engineB.sessions) != 0 || len(engineA.sessions) != 1 || !foreign.State.Running {
				t.Fatal("cleanup switched connection after validating ownership")
			}
			if err := c.Remove(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			if len(engineA.sessions) != 1 {
				t.Fatal("future connection selection deleted an unrelated container")
			}
		})
	}
}
