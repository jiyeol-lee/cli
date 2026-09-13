package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

const sandboxTestInfo = `{"Host":{"DatabaseBackend":"sqlite","Security":{"Rootless":true}},"Store":{"GraphRoot":"/tmp/cli-workmux-engine/store","RunRoot":"/tmp/cli-workmux-engine/run","GraphDriverName":"overlay","ConfigFile":"/tmp/cli-workmux-engine/storage.conf"}}`

type sandboxTestEngine struct {
	calls      []Process
	container  *sandboxInspection
	image      string
	info       string
	fail       map[string]error
	inspection string
	listing    string
	home       string
}

func sandboxTestEngineArgs(args ...string) []string {
	return append([]string{"--remote=false"}, args...)
}

func sandboxTestCallArgs(call Process) []string {
	return call.Args[1:]
}

func sandboxTestEndpoint() string {
	scope := []string{"podman", strconv.Itoa(os.Getuid()), "/tmp/cli-workmux-engine/store", "/tmp/cli-workmux-engine/run", "overlay", "/tmp/cli-workmux-engine/storage.conf", "sqlite", "false"}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(scope, "\x00"))))
}

func (engine *sandboxTestEngine) Run(ctx context.Context, p Process) ([]byte, error) {
	engine.calls = append(engine.calls, p)
	if p.Name == "git" {
		p.Env = append(p.Env, "HOME="+engine.home, "XDG_CONFIG_HOME="+filepath.Join(engine.home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		return (ExecRunner{}).Run(ctx, p)
	}
	if p.Dir != "/" {
		return nil, fmt.Errorf("engine process must not inherit caller cwd: %q", p.Dir)
	}
	prefix := sandboxTestEngineArgs()
	if p.Name != "podman" || len(p.Args) <= len(prefix) || !slices.Equal(p.Args[:len(prefix)], prefix) {
		return nil, fmt.Errorf("engine endpoint was not pinned: %s %v", p.Name, p.Args)
	}
	p.Args = p.Args[len(prefix):]
	key := p.Args[0]
	if key == "container" || key == "image" {
		key += " " + p.Args[1]
	}
	if err := engine.fail[key]; err != nil {
		return nil, err
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
		if engine.container == nil {
			return nil, fmt.Errorf("no such container")
		}
		return json.Marshal([]*sandboxInspection{engine.container})
	case "container ls":
		if engine.listing != "" {
			return []byte(engine.listing), nil
		}
		if engine.container == nil {
			return nil, nil
		}
		return []byte(engine.container.Name + "\n"), nil
	case "create":
		found := &sandboxInspection{ID: strings.Repeat("a", 64), Image: engine.image}
		found.Config.Labels = make(map[string]string)
		for i, arg := range p.Args {
			switch arg {
			case "--name":
				found.Name = "/" + p.Args[i+1]
			case "--label":
				key, value, _ := strings.Cut(p.Args[i+1], "=")
				found.Config.Labels[key] = value
			case "--entrypoint":
				found.Config.Image = p.Args[i+2]
			}
		}
		engine.container = found
		return []byte(found.ID + "\n"), nil
	case "start", "stop", "rm":
		if engine.container == nil || p.Args[len(p.Args)-1] != engine.container.ID {
			return nil, fmt.Errorf("action must address inspected container ID")
		}
		if key == "rm" {
			engine.container = nil
		} else {
			engine.container.State.Running = key == "start"
		}
		return nil, nil
	case "exec":
		if p.Stdout != nil && p.Stdin != nil {
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
	out, err := (ExecRunner{}).Run(context.Background(), Process{
		Name: "git", Args: args, Dir: dir, CleanGitEnv: true,
		Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"},
	})
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
	root := filepath.Join(base, "main")
	worktree := filepath.Join(base, "linked worktree")
	home := filepath.Join(base, "fake-home")
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
	w := Workspace{
		ID: "sandbox-test", RepoID: "repository-test", Root: root, CommonDir: filepath.Join(root, ".git"),
		Path: worktree, Branch: "topic", Handle: "topic", Container: "cli-workmux-test", Stage: "ready",
		Config: Config{Sandbox: SandboxConfig{Enabled: true, Image: "localhost/cli-workmux:test"}},
	}
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
		t.Skip("container lifecycle requires a nonroot host UID:GID")
	}
}

func TestContainersCheck(t *testing.T) {
	sandboxTestNonroot(t)
	for _, test := range []struct {
		name, info, failure string
		disabled            bool
		wantErr             bool
	}{
		{name: "local rootless podman"},
		{name: "disabled", disabled: true},
		{name: "engine failure", failure: "info", wantErr: true},
		{name: "image missing", failure: "image inspect", wantErr: true},
		{name: "malformed info", info: "broken", wantErr: true},
		{name: "remote podman", info: `{"host":{"serviceIsRemote":true}}`, wantErr: true},
		{name: "rootful podman", info: strings.Replace(sandboxTestInfo, `"Rootless":true`, `"Rootless":false`, 1), wantErr: true},
		{name: "missing driver", info: strings.Replace(sandboxTestInfo, `"overlay"`, `""`, 1), wantErr: true},
		{name: "relative storage", info: strings.Replace(sandboxTestInfo, `"/tmp/cli-workmux-engine/store"`, `"relative"`, 1), wantErr: true},
		{name: "missing storage identity", info: `{"Host":{"Security":{"Rootless":true}}}`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo, fail: make(map[string]error)}
			if test.info != "" {
				engine.info = test.info
			}
			if test.failure != "" {
				engine.fail[test.failure] = fmt.Errorf("unavailable")
			}
			home := t.TempDir()
			sandboxTestWrite(t, filepath.Join(home, ".config", "opencode", ".gitignore"), "")
			c := &Containers{Runner: engine, HomeDir: home, Getenv: func(string) string { return "" }}
			config := SandboxConfig{Enabled: !test.disabled, Image: "localhost/test:local"}
			err := c.Check(context.Background(), config)
			if (err != nil) != test.wantErr {
				t.Fatalf("Check error = %v", err)
			}
			if test.disabled && len(engine.calls) != 0 {
				t.Fatal("disabled sandbox invoked engine")
			}
			for _, call := range engine.calls {
				if call.Name != "podman" || !call.CleanGitEnv || call.Dir != "/" {
					t.Fatalf("unexpected process: %+v", call)
				}
				if !slices.Equal(call.Args, sandboxTestEngineArgs("info", "--format", "{{json .}}")) && !slices.Equal(call.Args, sandboxTestEngineArgs("image", "inspect", config.Image)) {
					t.Fatalf("unexpected check args: %v", call.Args)
				}
			}
		})
	}
}

func TestContainersLifecycle(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	plan, err := c.mountPlan(context.Background(), w, engine.image, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	calls := sandboxEngineCalls(engine)
	expected := [][]string{
		{"info", "--format", "{{json .}}"}, {"image", "inspect", w.Config.Sandbox.Image},
		{"info", "--format", "{{json .}}"},
		{"container", "inspect", w.Container}, {"container", "ls", "--all", "--filter", "name=^/?cli-workmux-test$", "--format", "{{.Names}}"},
		nil, {"container", "inspect", w.Container}, {"start", engine.container.ID},
	}
	createIndex := 5
	if len(calls) != len(expected) {
		t.Fatalf("engine calls = %+v", calls)
	}
	for i, call := range calls {
		want := sandboxTestEngineArgs(expected[i]...)
		if call.Name != "podman" || i != createIndex && !slices.Equal(call.Args, want) {
			t.Fatalf("call %d = %s %v, want %v", i, call.Name, call.Args, expected[i])
		}
	}
	wantCreate := []string{"create", "--name", w.Container, "--pull=never", "--init", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--cap-drop=ALL", "--security-opt=no-new-privileges", "--userns=keep-id"}
	labels := map[string]string{
		"common": w.CommonDir, "image": w.Config.Sandbox.Image, "image-id": engine.image, "mounts": plan.Fingerprint,
		"owner": "cli-workmux", "path": w.Path, "policy": "2", "repository": w.RepoID, "root": w.Root, "runtime": "podman", "workspace": w.ID,
		"endpoint": sandboxTestEndpoint(),
	}
	for _, key := range []string{"common", "endpoint", "image", "image-id", "mounts", "owner", "path", "policy", "repository", "root", "runtime", "workspace"} {
		wantCreate = append(wantCreate, "--label", sandboxLabel+key+"="+labels[key])
	}
	for _, mount := range plan.Mounts {
		value := "type=bind,source=" + mount.Source + ",target=" + mount.Target
		if mount.ReadOnly {
			value += ",readonly"
		}
		wantCreate = append(wantCreate, "--mount", value)
	}
	wantCreate = append(wantCreate, "--workdir", w.Path)
	for _, entry := range plan.Env {
		wantCreate = append(wantCreate, "--env", entry)
	}
	wantCreate = append(wantCreate, "--entrypoint", "/usr/bin/sleep", engine.image, "infinity")
	wantCreate = sandboxTestEngineArgs(wantCreate...)
	if !slices.Equal(calls[createIndex].Args, wantCreate) {
		t.Fatalf("create args\ngot  %q\nwant %q", calls[createIndex].Args, wantCreate)
	}
	engine.calls = nil
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if calls := sandboxEngineCalls(engine); len(calls) != 4 {
		t.Fatalf("reuse must not create or start: %+v", calls)
	}
	id := engine.container.ID
	for _, action := range []struct {
		run  func(context.Context, Workspace) error
		args []string
	}{{c.Stop, []string{"stop", id}}, {c.Stop, nil}, {c.Ensure, []string{"start", id}}, {c.Remove, []string{"rm", "--force", id}}, {c.Remove, nil}, {c.Stop, nil}} {
		engine.calls = nil
		if err := action.run(context.Background(), w); err != nil {
			t.Fatal(err)
		}
		var mutations [][]string
		for _, call := range sandboxEngineCalls(engine) {
			args := sandboxTestCallArgs(call)
			if slices.Contains([]string{"start", "stop", "rm", "create"}, args[0]) {
				mutations = append(mutations, args)
			}
		}
		if action.args == nil && len(mutations) != 0 || action.args != nil && (len(mutations) != 1 || !slices.Equal(mutations[0], action.args)) {
			t.Fatalf("action args = %v, want %v", mutations, action.args)
		}
	}
}

func TestContainersMissingVersusUnavailable(t *testing.T) {
	sandboxTestNonroot(t)
	for _, test := range []struct {
		name, inspection, listing string
		inspectFail, listFail     bool
		wantErr                   bool
	}{
		{name: "missing"},
		{name: "engine unavailable", inspectFail: true, listFail: true, wantErr: true},
		{name: "inspect failed but name exists", inspectFail: true, listing: "cli-workmux-test\n", wantErr: true},
		{name: "invalid json", inspection: "not json", wantErr: true},
		{name: "empty inspect json", inspection: "[]", wantErr: true},
		{name: "wrong name", inspection: `[{"Id":"abc","Name":"another"}]`, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			engine.inspection, engine.listing = test.inspection, test.listing
			if test.inspectFail {
				engine.fail["container inspect"] = fmt.Errorf("engine connection refused")
			}
			if test.listFail {
				engine.fail["container ls"] = fmt.Errorf("engine connection refused")
			}
			for _, action := range []func(context.Context, Workspace) error{c.Stop, c.Remove} {
				if err := action(context.Background(), w); (err != nil) != test.wantErr {
					t.Fatalf("action error = %v", err)
				}
			}
		})
	}
}

func TestContainersRefuseForeignLabels(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	for key, value := range engine.container.Config.Labels {
		t.Run(key, func(t *testing.T) {
			engine.container.Config.Labels[key] = "foreign"
			defer func() { engine.container.Config.Labels[key] = value }()
			engine.calls = nil
			for _, action := range []func(context.Context, Workspace) error{c.Ensure, c.Stop, c.Remove} {
				if err := action(context.Background(), w); err == nil {
					t.Fatal("foreign ownership accepted")
				}
			}
			if err := c.Exec(context.Background(), w, "opencode", nil, nil, nil, nil); err == nil {
				t.Fatal("foreign exec accepted")
			}
			if got := c.PaneCommand(w, "opencode"); !slices.Equal(got, []string{"/usr/bin/false"}) {
				t.Fatalf("foreign pane command accepted: %v", got)
			}
			for _, call := range sandboxEngineCalls(engine) {
				if slices.Contains([]string{"start", "stop", "rm", "create", "exec"}, sandboxTestCallArgs(call)[0]) {
					t.Fatalf("acted on foreign container: %v", call.Args)
				}
			}
		})
	}
}

func TestContainersRefuseStaleProtection(t *testing.T) {
	sandboxTestNonroot(t)
	for _, change := range []string{"config contents", "config inode", "image", "new credential directory", "missing snapshot", "snapshot contents"} {
		t.Run(change, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			if err := c.Ensure(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			plan, err := c.mountPlan(context.Background(), w, engine.image, false)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot string
			for _, mount := range plan.Mounts {
				if mount.Snapshot {
					snapshot = mount.Source
					break
				}
			}
			original, err := os.ReadFile(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(w.CommonDir, "config")
			switch change {
			case "config contents":
				sandboxTestGit(t, w.Root, "config", "user.name", "Changed Name")
			case "config inode":
				data, err := os.ReadFile(config)
				if err != nil {
					t.Fatal(err)
				}
				sandboxTestWrite(t, config+".replacement", string(data))
				if err := os.Rename(config+".replacement", config); err != nil {
					t.Fatal(err)
				}
			case "image":
				engine.image = "sha256:" + strings.Repeat("c", 64)
			case "new credential directory":
				data := filepath.Join(c.HomeDir, "new-data")
				if err := os.MkdirAll(filepath.Join(data, "opencode"), 0700); err != nil {
					t.Fatal(err)
				}
				c.Getenv = func(key string) string {
					if key == "XDG_DATA_HOME" {
						return data
					}
					return ""
				}
			case "missing snapshot":
				if err := os.Remove(snapshot); err != nil {
					t.Fatal(err)
				}
			case "snapshot contents":
				sandboxTestWrite(t, snapshot, "changed snapshot")
			}
			engine.calls = nil
			if err := c.Ensure(context.Background(), w); err == nil {
				t.Fatal("stale sandbox was reused")
			}
			for _, call := range sandboxEngineCalls(engine) {
				if args := sandboxTestCallArgs(call); args[0] == "create" || args[0] == "start" {
					t.Fatalf("stale sandbox silently restarted: %v", call.Args)
				}
			}
			if change != "missing snapshot" && change != "snapshot contents" {
				now, err := os.ReadFile(snapshot)
				if err != nil || !bytes.Equal(now, original) {
					t.Fatalf("running snapshot was modified: %v", err)
				}
			}
			if err := c.Remove(context.Background(), w); err != nil {
				t.Fatalf("cannot remove stale but owned container: %v", err)
			}
		})
	}
}

func TestContainersExecAndPaneArguments(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	command := `opencode "quoted arg"; printf '%s' '$literal'`
	var stdout, stderr bytes.Buffer
	stdin := strings.NewReader("input")
	env := []string{"WM_BRANCH=topic $(false)", "VALUE=one=two\nthree", "_NUMBER1=1"}
	if err := c.Exec(context.Background(), w, command, env, stdin, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	last := engine.calls[len(engine.calls)-1]
	want := sandboxTestEngineArgs("exec", "-i", "--workdir", w.Path, "--env", env[0], "--env", env[1], "--env", env[2], engine.container.ID, "bash", "-c", command)
	if !slices.Equal(last.Args, want) || last.Stdin != stdin || last.Stdout != &stdout || last.Stderr != &stderr || stdout.String() != "input" {
		t.Fatalf("exec process = %+v; want %v", last, want)
	}
	if err := c.Exec(context.Background(), w, "opencode", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	last = engine.calls[len(engine.calls)-1]
	want = sandboxTestEngineArgs("exec", "--workdir", w.Path, engine.container.ID, "bash", "-c", "opencode")
	if !slices.Equal(last.Args, want) {
		t.Fatalf("noninteractive exec = %v", last.Args)
	}
	for _, command := range []string{"", "opencode", `printf '%s' 'a b'`} {
		want := []string{"/usr/bin/env", "--chdir=/"}
		for _, key := range sandboxEngineEnvKeys {
			want = append(want, "--unset="+key)
		}
		for _, key := range sandboxEngineEnvKeys {
			if value := os.Getenv(key); value != "" {
				want = append(want, key+"="+value)
			}
		}
		want = append(want, "podman", "--remote=false", "exec", "-it", "--workdir", w.Path, engine.container.ID, "bash")
		if command == "" {
			want = append(want, "-i")
		} else {
			want = append(want, "-c", command)
		}
		if got := c.PaneCommand(w, command); !reflect.DeepEqual(got, want) {
			t.Fatalf("pane command = %v, want %v", got, want)
		}
	}
	for _, env := range []string{"SECRET", "=empty", "--privileged=1", "1KEY=value", "A-B=value", "A\nB=value", "A=value\x00"} {
		engine.calls = nil
		if err := c.Exec(context.Background(), w, "opencode", []string{env}, nil, nil, nil); err == nil || len(engine.calls) != 0 {
			t.Fatalf("invalid env accepted or engine invoked: %q", env)
		}
	}
}

func TestContainersActionFailure(t *testing.T) {
	sandboxTestNonroot(t)
	for _, failure := range []string{"create", "start", "stop", "rm", "exec"} {
		t.Run(failure, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			if failure != "create" && failure != "start" {
				if err := c.Ensure(context.Background(), w); err != nil {
					t.Fatal(err)
				}
			}
			engine.fail[failure] = fmt.Errorf("engine unavailable")
			action := c.Ensure
			switch failure {
			case "stop":
				action = c.Stop
			case "rm":
				action = c.Remove
			case "exec":
				action = func(ctx context.Context, w Workspace) error { return c.Exec(ctx, w, "opencode", nil, nil, nil, nil) }
			}
			if err := action(context.Background(), w); err == nil || !strings.Contains(err.Error(), "engine unavailable") {
				t.Fatalf("action failure = %v", err)
			}
		})
	}
}

func TestContainersCleanupWithoutWorktree(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, _ := sandboxTestFixture(t)
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(w.Root); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if got := c.PaneCommand(w, ""); !slices.Equal(got, []string{"/usr/bin/false"}) {
		t.Fatalf("missing container could start a host shell: %v", got)
	}
}

func TestContainersRejectInvalidImageIdentity(t *testing.T) {
	sandboxTestNonroot(t)
	for _, image := range []string{"", "--privileged", "sha256:short", strings.Repeat("b", 64) + "\n"} {
		engine := &sandboxTestEngine{image: image, info: sandboxTestInfo}
		home := t.TempDir()
		sandboxTestWrite(t, filepath.Join(home, ".config", "opencode", ".gitignore"), "")
		c := &Containers{Runner: engine, HomeDir: home, Getenv: func(string) string { return "" }}
		if err := c.Check(context.Background(), SandboxConfig{Enabled: true, Image: "localhost/test:local"}); err == nil {
			t.Fatalf("invalid image identity accepted: %q", image)
		}
	}
}

func TestContainersRejectEndpointOverridesOnEveryAction(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "PODMAN_CONNECTIONS_CONF", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_STORAGE_CONF", "STORAGE_DRIVER", "STORAGE_OPTS"} {
		t.Run(key, func(t *testing.T) {
			c.Getenv = func(name string) string {
				if name == key {
					return "different-engine"
				}
				return ""
			}
			for _, action := range []func(context.Context, Workspace) error{c.Ensure, c.Stop, c.Remove} {
				engine.calls = nil
				if err := action(context.Background(), w); err == nil || !strings.Contains(err.Error(), key) || len(engine.calls) != 0 {
					t.Fatalf("override reached an engine or was treated as absent: %v, calls=%v", err, engine.calls)
				}
			}
			if err := c.Exec(context.Background(), w, "opencode", nil, nil, nil, nil); err == nil {
				t.Fatal("exec accepted endpoint override")
			}
			if args := c.PaneCommand(w, "opencode"); !slices.Equal(args, []string{"/usr/bin/false"}) || len(engine.calls) != 0 {
				t.Fatalf("pane accepted endpoint override: %v", args)
			}
			if engine.container == nil || !engine.container.State.Running {
				t.Fatal("endpoint rejection modified the original container")
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal("endpoint rejection removed worktree", err)
			}
		})
	}
}

func TestContainersRejectChangedEndpointBeforeMissingLookup(t *testing.T) {
	sandboxTestNonroot(t)
	for _, change := range []struct{ old, new string }{
		{"/tmp/cli-workmux-engine/store", "/tmp/different-engine/store"},
		{"/tmp/cli-workmux-engine/run", "/tmp/different-engine/run"},
		{"overlay", "vfs"},
		{"/tmp/cli-workmux-engine/storage.conf", "/tmp/different-engine/storage.conf"},
		{"sqlite", "boltdb"},
		{`"Store":{`, `"Store":{"TransientStore":true,`},
	} {
		t.Run(change.old, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			if err := c.Ensure(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			engine.info = strings.Replace(engine.info, change.old, change.new, 1)
			engine.container = nil
			for _, action := range []func(context.Context, Workspace) error{c.Stop, c.Remove} {
				engine.calls = nil
				if err := action(context.Background(), w); err == nil || !strings.Contains(err.Error(), "engine identity changed") {
					t.Fatalf("different endpoint treated as missing: %v", err)
				}
				for _, call := range sandboxEngineCalls(engine) {
					if args := sandboxTestCallArgs(call); args[0] != "info" {
						t.Fatalf("queried container in a different endpoint: %v", call.Args)
					}
				}
			}
		})
	}
}

func TestContainersIgnoreDockerEnvironment(t *testing.T) {
	sandboxTestNonroot(t)
	for _, value := range []string{"", "unrelated-docker-setting"} {
		t.Run(value, func(t *testing.T) {
			for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH"} {
				t.Setenv(key, value)
			}
			c, w, engine := sandboxTestFixture(t)
			c.Getenv = func(key string) string {
				if strings.HasPrefix(key, "DOCKER_") {
					return value
				}
				return ""
			}
			if err := c.Check(t.Context(), w.Config.Sandbox); err != nil {
				t.Fatal(err)
			}
			if err := c.Exec(t.Context(), w, "opencode", nil, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
			pane := c.PaneCommand(w, "")
			if !slices.Contains(pane, "podman") || !slices.Contains(pane, "--remote=false") {
				t.Fatalf("pane did not use local Podman: %v", pane)
			}
			for _, arg := range pane {
				if strings.Contains(arg, "DOCKER_") {
					t.Fatalf("pane manages unrelated Docker settings: %v", pane)
				}
			}
			for _, action := range []func(context.Context, Workspace) error{c.Ensure, c.Stop, c.Remove} {
				if err := action(t.Context(), w); err != nil {
					t.Fatal(err)
				}
			}
			for _, call := range sandboxEngineCalls(engine) {
				if call.Name != "podman" || call.Args[0] != "--remote=false" {
					t.Fatalf("invoked another backend: %+v", call)
				}
			}
		})
	}
}

func TestContainersRecheckWritableAliasesBeforeExecAndStart(t *testing.T) {
	sandboxTestNonroot(t)
	for _, location := range []string{"worktree", "opencode", "admin", "objects", "refs", "logs", "rr-cache"} {
		t.Run(location, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			if err := os.MkdirAll(filepath.Join(w.CommonDir, "rr-cache"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := c.Ensure(context.Background(), w); err != nil {
				t.Fatal(err)
			}
			identity, err := discoverSandboxGit(w.Path, w.CommonDir)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(w.CommonDir, location)
			switch location {
			case "worktree":
				dir = w.Path
			case "opencode":
				dir = filepath.Join(c.HomeDir, ".local", "share", "opencode", "nested")
			case "admin":
				dir = identity.Admin
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(filepath.Dir(w.Root), "outside-sentinel")
			sandboxTestWrite(t, sentinel, "outside")
			alias := filepath.Join(dir, "preexisting-alias")
			if err := os.Link(sentinel, alias); err != nil {
				t.Fatal(err)
			}
			engine.calls = nil
			if err := c.Exec(context.Background(), w, "printf changed > preexisting-alias", nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "--no-hardlinks") {
				t.Fatalf("exec did not reject a writable alias: %v", err)
			}
			if got := c.PaneCommand(w, "opencode"); !slices.Equal(got, []string{"/usr/bin/false"}) {
				t.Fatalf("pane accepted a writable alias: %v", got)
			}
			engine.container.State.Running = false
			if err := c.Ensure(context.Background(), w); err == nil || !strings.Contains(err.Error(), "hard links") {
				t.Fatalf("start accepted a writable alias: %v", err)
			}
			for _, call := range sandboxEngineCalls(engine) {
				if args := sandboxTestCallArgs(call); args[0] == "exec" || args[0] == "start" || args[0] == "create" {
					t.Fatalf("acted with unsafe writable aliases: %v", args)
				}
			}
			first, err := os.Stat(sentinel)
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.Stat(alias)
			if err != nil || !os.SameFile(first, second) {
				t.Fatalf("CLI silently replaced the existing hardlink: %v", err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "outside" {
				t.Fatalf("outside sentinel changed: %q %v", data, err)
			}
			if err := c.Remove(context.Background(), w); err != nil {
				t.Fatalf("unsafe source should not prevent owned cleanup: %v", err)
			}
		})
	}
}

func TestContainersProcessesSurviveDeletedCallerDirectory(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	deleted := filepath.Join(t.TempDir(), "cwd")
	if err := os.Mkdir(deleted, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(deleted)
	if err := os.Remove(deleted); err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := c.Exec(context.Background(), w, "opencode", nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	for _, call := range engine.calls {
		if call.Dir != "/" {
			t.Fatalf("process inherited deleted cwd: %+v", call)
		}
	}
}

func TestContainersRejectExplicitEmptyEngineConfig(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	if err := c.Ensure(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTAINERS_CONF", "")
	engine.calls = nil
	if err := c.Stop(context.Background(), w); err == nil || !strings.Contains(err.Error(), "CONTAINERS_CONF") || len(engine.calls) != 0 {
		t.Fatalf("an empty config override is not the same as unset: %v, calls=%v", err, engine.calls)
	}
}

func TestContainersOpenCodePreflight(t *testing.T) {
	sandboxTestNonroot(t)
	for _, test := range []struct {
		name     string
		xdg      bool
		disabled bool
		wantErr  bool
	}{
		{name: "missing directory", wantErr: true},
		{name: "missing file", wantErr: true},
		{name: "empty regular file"},
		{name: "existing contents"},
		{name: "XDG regular file", xdg: true},
		{name: "XDG missing file", xdg: true, wantErr: true},
		{name: "symlink", wantErr: true},
		{name: "hardlink", wantErr: true},
		{name: "directory", wantErr: true},
		{name: "unreadable", wantErr: true},
		{name: "pipe", wantErr: true},
		{name: "config directory symlink", wantErr: true},
		{name: "disabled", disabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			base := filepath.Join(home, ".config")
			values := make(map[string]string)
			if test.xdg {
				// An initialized default directory must not hide an uninitialized XDG override.
				sandboxTestWrite(t, filepath.Join(base, "opencode", ".gitignore"), "default")
				base = filepath.Join(home, "custom config")
				values["XDG_CONFIG_HOME"] = base
			}
			dir := filepath.Join(base, "opencode")
			path := filepath.Join(dir, ".gitignore")
			if test.name != "missing directory" && !test.disabled {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch test.name {
			case "empty regular file", "XDG regular file":
				sandboxTestWrite(t, path, "")
			case "existing contents":
				sandboxTestWrite(t, path, "# User-maintained content must not change.\n")
			case "symlink", "hardlink":
				target := filepath.Join(home, "outside")
				sandboxTestWrite(t, target, "outside")
				link := os.Symlink
				if test.name == "hardlink" {
					link = os.Link
				}
				if err := link(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				sandboxTestWrite(t, path, "")
				if err := os.Chmod(path, 0000); err != nil {
					t.Fatal(err)
				}
			case "pipe":
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "config directory symlink":
				target := filepath.Join(home, "outside-config")
				sandboxTestWrite(t, filepath.Join(target, ".gitignore"), "")
				if err := os.Remove(dir); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, dir); err != nil {
					t.Fatal(err)
				}
			}
			before, beforeErr := os.Lstat(path)
			engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo}
			c := &Containers{Runner: engine, HomeDir: home, Getenv: func(key string) string { return values[key] }}
			err := c.Check(context.Background(), SandboxConfig{Enabled: !test.disabled, Image: "localhost/test:local"})
			if (err != nil) != test.wantErr {
				t.Fatalf("OpenCode preflight error = %v", err)
			}
			if test.wantErr {
				for _, text := range []string{path, dir, "readable regular .gitignore", "initialize OpenCode once on the host", "then retry"} {
					if !strings.Contains(err.Error(), text) {
						t.Fatalf("preflight lacks actionable path/setup details: %v", err)
					}
				}
			}
			if (test.wantErr || test.disabled) && len(engine.calls) != 0 {
				t.Fatalf("preflight invoked an engine or host initialization: %+v", engine.calls)
			}
			if !test.wantErr && !test.disabled && len(engine.calls) != 2 {
				t.Fatalf("initialized config did not continue to the image check: %+v", engine.calls)
			}
			after, afterErr := os.Lstat(path)
			if os.IsNotExist(beforeErr) {
				if !os.IsNotExist(afterErr) {
					t.Fatalf("preflight created missing metadata: %v", afterErr)
				}
			} else if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("preflight modified existing metadata: before=%v, after=%v", beforeErr, afterErr)
			}
			if test.name == "missing directory" || test.disabled {
				if _, err := os.Lstat(dir); !os.IsNotExist(err) {
					t.Fatalf("preflight created the config directory: %v", err)
				}
			}
			if test.name == "existing contents" {
				content, err := os.ReadFile(path)
				if err != nil || string(content) != "# User-maintained content must not change.\n" {
					t.Fatalf("preflight rewrote metadata: %q %v", content, err)
				}
			}
		})
	}
}

func TestContainersRevalidateOpenCodeMetadata(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	path := filepath.Join(c.HomeDir, ".config", "opencode", ".gitignore")
	if err := c.Check(context.Background(), w.Config.Sandbox); err != nil {
		t.Fatal(err)
	}
	for _, running := range []bool{false, true} {
		if running {
			sandboxTestWrite(t, path, "")
			if err := c.Ensure(context.Background(), w); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		engine.calls = nil
		if err := c.Ensure(context.Background(), w); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("Ensure did not revalidate OpenCode metadata: %v", err)
		}
		if _, err := c.mountPlan(context.Background(), w, engine.image, !running); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("mount planning did not revalidate OpenCode metadata: %v", err)
		}
		if err := c.Exec(context.Background(), w, "opencode", nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("exec did not revalidate OpenCode metadata: %v", err)
		}
		if len(engine.calls) != 0 {
			t.Fatalf("missing metadata was checked after engine operations: %+v", engine.calls)
		}
		if !running {
			if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
				t.Fatalf("failed preflight provisioned private state: %v", err)
			}
		} else if engine.container == nil || !engine.container.State.Running {
			t.Fatal("failed revalidation stopped or replaced the running container")
		}
	}
	if err := c.Stop(context.Background(), w); err != nil {
		t.Fatalf("missing OpenCode metadata must not prevent owned cleanup: %v", err)
	}
	if err := c.Remove(context.Background(), w); err != nil {
		t.Fatal(err)
	}
}

func TestContainersAddPreflightBeforeGitMutation(t *testing.T) {
	f := newHostFixture(t, "sandbox: {enabled: true}\n")
	engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo, home: f.home}
	f.app.Sandbox = &Containers{Runner: engine, HomeDir: f.home, StateDir: f.state}
	metadata := filepath.Join(f.home, "xdgconfig", "opencode", ".gitignore")
	beforeRefs := hostGit(t, f.root, "show-ref")
	beforeTrees := hostGit(t, f.root, "worktree", "list", "--porcelain")
	beforeStatus := hostGit(t, f.root, "status", "--porcelain")
	if err := f.run(t, "add", "needs-opencode-metadata", "-b"); err == nil || !strings.Contains(err.Error(), metadata) {
		t.Fatalf("add did not fail at the real sandbox preflight: %v", err)
	}
	if len(engine.calls) != 0 {
		t.Fatalf("add started the image check before metadata preflight: %+v", engine.calls)
	}
	for _, event := range f.events {
		if strings.HasPrefix(event, "git:") || strings.HasPrefix(event, "hook:") || event == "mux:create" {
			t.Fatalf("add mutated resources after failed sandbox preflight: %q", f.events)
		}
	}
	if hostGit(t, f.root, "show-ref") != beforeRefs || hostGit(t, f.root, "worktree", "list", "--porcelain") != beforeTrees || hostGit(t, f.root, "status", "--porcelain") != beforeStatus {
		t.Fatal("failed sandbox preflight changed Git refs, worktrees or tracked files")
	}
	path := workspacePath(f.root, "needs-opencode-metadata")
	common := filepath.Join(f.root, ".git")
	for _, path := range []string{path, metadata, filepath.Join(f.state, identity(common), identity(common, path)+".json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("failed preflight provisioned %s: %v", path, err)
		}
	}
}
