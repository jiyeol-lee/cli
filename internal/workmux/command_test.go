package workmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want Command
	}{
		{"empty help", nil, Command{Help: true}},
		{"global help", []string{"--help"}, Command{Help: true}},
		{"command help", []string{"help", "add"}, Command{Kind: "add", Help: true}},
		{"add help", []string{"add", "-h"}, Command{Kind: "add", Help: true}},
		{"add", []string{"add", "feature/topic", "--base", "HEAD~1", "-l", "dev", "-b"}, Command{Kind: "add", Name: "feature/topic", Base: "HEAD~1", Layout: "dev", Background: true}},
		{"equals", []string{"add", "--layout=dev", "--base=main", "topic"}, Command{Kind: "add", Name: "topic", Base: "main", Layout: "dev"}},
		{"merge", []string{"merge", "--into", "release", "--keep", "topic"}, Command{Kind: "merge", Name: "topic", Into: "release", Keep: true}},
		{"remove", []string{"remove", "--keep-branch", "topic", "--force"}, Command{Kind: "remove", Name: "topic", KeepBranch: true, Force: true}},
		{"end options", []string{"open", "--", "topic"}, Command{Kind: "open", Name: "topic"}},
		{"implicit close", []string{"close"}, Command{Kind: "close"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseCommand(test.args)
			if err != nil || got != test.want {
				t.Fatalf("ParseCommand(%q) = %+v, %v; want %+v", test.args, got, err, test.want)
			}
		})
	}
	for _, args := range [][]string{
		{"add"}, {"unknown"}, {"help", "unknown"}, {"help", "add", "extra"},
		{"add", "one", "two"}, {"merge", "one", "two"}, {"open", "--keep"},
		{"close", "--force"}, {"merge", "--force"}, {"remove", "--keep"},
		{"add", "topic", "--base"}, {"add", "topic", "--base", "--background"},
		{"add", "topic", "--layout="}, {"add", "topic", "--background=true"},
		{"add", "topic", "-b", "--background"}, {"merge", "--into", "-bad"},
		{"add", "--", "-bad"}, {"add", "one\ntwo"}, {"add", ""},
		{"add", "topic", "--base", "@{-1}"}, {"add", "topic", "--wat"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := ParseCommand(args); err == nil || !strings.HasPrefix(err.Error(), "usage: cli workmux") {
				t.Fatalf("expected usage error for %q, got %v", args, err)
			}
		})
	}
}

type hostTestRunner struct {
	processes []Process
	events    *[]string
	hook      func(Process) error
	git       func(Process) error
}

func hostGitArgs(p Process) []string {
	args := p.Args
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args
}

func (runner *hostTestRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	runner.processes = append(runner.processes, p)
	if p.Name == "git" {
		args := hostGitArgs(p)
		if len(args) > 0 && (args[0] == "merge" || args[0] == "update-ref" || args[0] == "worktree" && len(args) > 1 && args[1] != "list") {
			*runner.events = append(*runner.events, "git:"+strings.Join(args[:min(2, len(args))], " "))
		}
		if runner.git != nil {
			if err := runner.git(p); err != nil {
				return nil, err
			}
		}
		return (ExecRunner{}).Run(ctx, p)
	}
	if p.Name == "bash" {
		*runner.events = append(*runner.events, "hook:"+p.Args[1])
		if runner.hook != nil {
			return nil, runner.hook(p)
		}
		return nil, nil
	}
	return nil, fmt.Errorf("unexpected process %s", p.Name)
}

type hostTestMux struct {
	caller                                      bool
	cleanupToken                                string
	onCapturedClose                             func(Workspace, CleanupWindow) error
	onExists                                    func(Workspace, CleanupWindow) (bool, error)
	events                                      *[]string
	window                                      string
	sessionErr, createErr, closeErr, quiesceErr error
	panes                                       []Pane
	commands                                    [][]string
	onCreate, onClose                           func(Workspace)
}

func (mux *hostTestMux) Session(context.Context) (string, error) {
	*mux.events = append(*mux.events, "mux:session")
	return "$1", mux.sessionErr
}

func (mux *hostTestMux) Server(context.Context) (string, error) { return "/fake/tmux.sock", nil }

func (mux *hostTestMux) Find(context.Context, Workspace) (string, error) {
	*mux.events = append(*mux.events, "mux:find")
	return mux.window, nil
}

func (mux *hostTestMux) Create(_ context.Context, _ string, w Workspace, panes []Pane, commands [][]string) (string, error) {
	*mux.events = append(*mux.events, "mux:create")
	if mux.onCreate != nil {
		mux.onCreate(w)
	}
	mux.panes, mux.commands = panes, commands
	mux.window = "@7"
	return mux.window, mux.createErr
}

func (mux *hostTestMux) Focus(context.Context, Workspace, string) error {
	*mux.events = append(*mux.events, "mux:focus")
	return nil
}

func (mux *hostTestMux) Quiesce(context.Context, Workspace) error {
	*mux.events = append(*mux.events, "mux:quiesce")
	return mux.quiesceErr
}

func (mux *hostTestMux) Capture(_ context.Context, _ Workspace, token string) (CleanupWindow, error) {
	*mux.events = append(*mux.events, "mux:capture")
	mux.cleanupToken = token
	window := CleanupWindow{ID: mux.window, Token: token, Caller: mux.caller}
	if window.ID != "" {
		window.Socket = "/fake/tmux.sock"
	}
	return window, mux.quiesceErr
}

func (mux *hostTestMux) CloseCaptured(ctx context.Context, w Workspace, window CleanupWindow) error {
	if mux.onCapturedClose != nil {
		return mux.onCapturedClose(w, window)
	}
	return mux.Close(ctx, w)
}

func (mux *hostTestMux) CapturedExists(_ context.Context, w Workspace, window CleanupWindow) (bool, error) {
	*mux.events = append(*mux.events, "mux:exists")
	if mux.onExists != nil {
		return mux.onExists(w, window)
	}
	if window.Token != mux.cleanupToken {
		return false, fmt.Errorf("stale captured window")
	}
	return mux.window != "", nil
}

func (mux *hostTestMux) Close(_ context.Context, w Workspace) error {
	*mux.events = append(*mux.events, "mux:close")
	if mux.onClose != nil {
		mux.onClose(w)
	}
	if mux.closeErr == nil {
		mux.window = ""
	}
	return mux.closeErr
}

type hostTestSandbox struct {
	events                              *[]string
	checkErr, ensureErr, stopErr, rmErr error
	exec                                func(Workspace, string, []string) error
}

func (sandbox *hostTestSandbox) Check(context.Context, SandboxConfig) error {
	*sandbox.events = append(*sandbox.events, "sandbox:check")
	return sandbox.checkErr
}

func (sandbox *hostTestSandbox) Ensure(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:ensure")
	return sandbox.ensureErr
}

func (sandbox *hostTestSandbox) Exec(_ context.Context, w Workspace, command string, env []string, _ io.Reader, _, _ io.Writer) error {
	*sandbox.events = append(*sandbox.events, "sandbox:hook:"+command)
	if sandbox.exec != nil {
		return sandbox.exec(w, command, env)
	}
	return nil
}

func (*hostTestSandbox) PaneCommand(w Workspace, command string) []string {
	return []string{"podman", "exec", w.Container, "sh", "-lc", command}
}

func (sandbox *hostTestSandbox) Stop(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:stop")
	return sandbox.stopErr
}

func (sandbox *hostTestSandbox) Remove(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:remove")
	return sandbox.rmErr
}

type hostFixture struct {
	root, home, config, state string
	events                    []string
	runner                    *hostTestRunner
	mux                       *hostTestMux
	sandbox                   *hostTestSandbox
	app                       App
	stdout                    bytes.Buffer
}

func hostGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	protected := []string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-c", "core.fsmonitor=false"}
	cmd := exec.Command("git", append(protected, args...)...)
	cmd.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hostWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func newHostFixture(t *testing.T, config string) *hostFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &hostFixture{root: filepath.Join(base, "test repo"), home: filepath.Join(base, "home"), config: filepath.Join(base, "config"), state: filepath.Join(base, "state")}
	for _, path := range []string{f.root, f.home, f.config} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(f.home, "xdgconfig"))
	hostGit(t, f.root, "init", "--initial-branch=main")
	hostGit(t, f.root, "config", "user.name", "Workmux Test")
	hostGit(t, f.root, "config", "user.email", "workmux@example.invalid")
	hostWrite(t, filepath.Join(f.root, "tracked"), "initial\n")
	hostWrite(t, filepath.Join(f.root, ".gitignore"), ".env\nignored/\n")
	hostGit(t, f.root, "add", "tracked", ".gitignore")
	hostGit(t, f.root, "commit", "-m", "initial")
	if config != "" {
		hostWrite(t, filepath.Join(f.config, "config.yaml"), config)
	}
	f.runner = &hostTestRunner{events: &f.events}
	f.mux = &hostTestMux{events: &f.events}
	f.sandbox = &hostTestSandbox{events: &f.events}
	f.app = App{Runner: f.runner, Mux: f.mux, Sandbox: f.sandbox, HomeDir: f.home, StateDir: f.state,
		ConfigDir: f.config, Getwd: func() (string, error) { return f.root, nil }, Stdout: &f.stdout}
	return f
}

func (f *hostFixture) run(t *testing.T, args ...string) error {
	t.Helper()
	command, err := ParseCommand(args)
	if err != nil {
		t.Fatal(err)
	}
	return f.app.Run(context.Background(), command)
}

func (f *hostFixture) load(t *testing.T, name string) workspaceState {
	t.Helper()
	path := workspacePath(f.root, strings.ReplaceAll(name, "/", "-"))
	common := filepath.Join(f.root, ".git")
	state, err := readState(filepath.Join(f.state, identity(common), identity(common, path)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func hostEventBefore(t *testing.T, events []string, before, after string) {
	t.Helper()
	a, b := slices.Index(events, before), slices.Index(events, after)
	if a == -1 || b == -1 || a >= b {
		t.Fatalf("expected %s before %s in %q", before, after, events)
	}
}

func TestHostAddOpenCloseUseSavedLayout(t *testing.T) {
	f := newHostFixture(t, "post_create: [created]\nlayouts:\n  dev:\n    panes:\n      - command: editor\n      - command: tests\n        split: vertical\n")
	f.mux.onCreate = func(w Workspace) {
		if state := f.load(t, w.Branch); state.Stage != "ready" {
			t.Fatalf("state must precede mux mutation: %+v", state)
		}
	}
	if err := f.run(t, "add", "feature/topic", "-l", "dev", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "feature/topic")
	if w.Path != workspacePath(f.root, "feature-topic") || w.Branch != "feature/topic" || !w.CreatedBranch || w.Layout != "dev" {
		t.Fatalf("wrong workspace: %+v", w)
	}
	if slices.Contains(f.events, "mux:focus") {
		t.Fatal("background add focused a window")
	}
	hostEventBefore(t, f.events, "mux:session", "git:worktree add")
	hostEventBefore(t, f.events, "hook:created", "mux:create")
	if err := f.run(t, "open", "feature-topic"); err != nil {
		t.Fatal(err)
	}
	f.mux.onClose = func(w Workspace) {
		state := f.load(t, w.Branch)
		if state.Stage != "closed" {
			t.Fatalf("close did not finalize state: %s", state.Stage)
		}
		repo, err := (gitHost{runner: f.runner}).discover(context.Background(), f.root)
		if err != nil {
			t.Fatal(err)
		}
		store, err := lockState(f.state, repo)
		if err != nil {
			t.Fatalf("lock must be released before killing caller: %v", err)
		}
		if err := store.unlock(); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(t, "close", "feature-topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "close", "feature-topic"); err != nil {
		t.Fatal(err)
	}
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "post_create: [changed]\npanes: [{command: new}]\n")
	if err := f.run(t, "open", "feature/topic"); err != nil {
		t.Fatal(err)
	}
	if len(f.mux.panes) != 2 || f.mux.panes[0].Command != "editor" {
		t.Fatalf("open did not use saved layout: %+v", f.mux.panes)
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return !strings.HasPrefix(event, "hook:") })); count != 1 {
		t.Fatalf("open/close reran provisioning hooks: %q", f.events)
	}
}

func TestHostAddUsesCurrentHeadAndReusesLocalBranches(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "first", "-b"); err != nil {
		t.Fatal(err)
	}
	first := f.load(t, "first")
	hostWrite(t, filepath.Join(first.Path, "tracked"), "first\n")
	hostGit(t, first.Path, "commit", "-am", "first")
	head := hostGit(t, first.Path, "rev-parse", "HEAD")
	f.app.Getwd = func() (string, error) { return first.Path, nil }
	if err := f.run(t, "add", "second", "-b"); err != nil {
		t.Fatal(err)
	}
	if got := hostGit(t, f.load(t, "second").Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("new branch started at %s, want current HEAD %s", got, head)
	}
	hostGit(t, f.root, "branch", "existing", "main")
	if err := f.run(t, "add", "existing", "-b"); err != nil {
		t.Fatal(err)
	}
	if f.load(t, "existing").CreatedBranch {
		t.Fatal("existing branch marked as created")
	}
	if err := f.run(t, "add", "main", "-b"); err == nil {
		t.Fatal("accepted branch checked out in main worktree")
	}
}

func TestHostAddRejectsSlugCollisionAndUnownedPath(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "feature/topic", "-b"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "add", "feature-topic", "-b"); err == nil {
		t.Fatal("accepted colliding slug")
	}
	path := workspacePath(f.root, "unowned")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "add", "unowned", "-b"); err == nil {
		t.Fatal("accepted existing unowned path")
	}
}

func TestHostPreflightAndPartialAdd(t *testing.T) {
	for _, test := range []struct {
		name, config, wantStage string
		setup                   func(*hostFixture)
	}{
		{"tmux", "", "", func(f *hostFixture) { f.mux.sessionErr = fmt.Errorf("not in tmux") }},
		{"sandbox preflight", "sandbox: {enabled: true}\n", "", func(f *hostFixture) { f.sandbox.checkErr = fmt.Errorf("missing image") }},
		{"worktree failure", "", "planned", func(f *hostFixture) {
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
					if f.load(t, "topic").Stage != "planned" {
						t.Fatal("missing write-ahead state")
					}
					return fmt.Errorf("injected add failure")
				}
				return nil
			}
		}},
		{"sandbox ensure", "sandbox: {enabled: true}\n", "files", func(f *hostFixture) { f.sandbox.ensureErr = fmt.Errorf("cannot start") }},
		{"hook", "post_create: [fail]\n", "sandbox", func(f *hostFixture) { f.runner.hook = func(Process) error { return fmt.Errorf("failed") } }},
		{"panes", "", "ready", func(f *hostFixture) { f.mux.createErr = fmt.Errorf("cannot split") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, test.config)
			test.setup(f)
			if err := f.run(t, "add", "topic", "-b"); err == nil {
				t.Fatal("expected failure")
			}
			if test.wantStage == "" {
				if slices.Contains(f.events, "git:worktree add") {
					t.Fatalf("mutated Git before preflight: %q", f.events)
				}
				return
			}
			state := f.load(t, "topic")
			if state.Stage != test.wantStage {
				t.Fatalf("stage = %s, want %s", state.Stage, test.wantStage)
			}
			if test.wantStage != "planned" {
				if _, err := os.Stat(state.Path); err != nil {
					t.Fatal("partial worktree was not preserved", err)
				}
			}
		})
	}
}

func TestHostNamesOnlyImplicitInsideManagedTree(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "open"); err == nil {
		t.Fatal("guessed a workspace from main")
	}
	state := f.load(t, "topic")
	dir := filepath.Join(state.Path, "subdir")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f.app.Getwd = func() (string, error) { return dir, nil }
	if err := f.run(t, "open"); err != nil {
		t.Fatal(err)
	}
}

func TestHostSandboxHooksAndChangedConfig(t *testing.T) {
	f := newHostFixture(t, "sandbox: {enabled: true}\npost_create: [created]\npre_merge: [mergecheck]\npre_remove: [removecheck]\n")
	f.sandbox.exec = func(w Workspace, command string, env []string) error {
		if !slices.Contains(env, "WM_WORKTREE_PATH="+w.Path) || !slices.Contains(env, "WM_BRANCH_NAME="+w.Branch) {
			t.Fatalf("missing hook environment: %q", env)
		}
		return nil
	}
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	hostEventBefore(t, f.events, "sandbox:check", "git:worktree add")
	hostEventBefore(t, f.events, "sandbox:ensure", "sandbox:hook:created")
	hostEventBefore(t, f.events, "sandbox:hook:created", "mux:create")
	if f.mux.commands[0][0] != "podman" {
		t.Fatalf("sandbox pane fell back to host: %q", f.mux.commands)
	}
	f.sandbox.ensureErr = fmt.Errorf("must not ensure an already open workspace")
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal("open should only focus its existing window", err)
	}
	f.sandbox.ensureErr = nil
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "sandbox: {enabled: false}\n")
	if err := f.run(t, "open", "topic"); err == nil || !strings.Contains(err.Error(), "configuration changed") {
		t.Fatalf("expected sandbox drift rejection, got %v", err)
	}
	if err := f.run(t, "merge", "topic"); err != nil {
		t.Fatal(err)
	}
	hostEventBefore(t, f.events, "sandbox:hook:removecheck", "sandbox:stop")
	hostEventBefore(t, f.events, "sandbox:stop", "git:worktree remove")
	hostEventBefore(t, f.events, "git:worktree remove", "sandbox:remove")
	hostEventBefore(t, f.events, "mux:close", "git:worktree remove")
	for _, event := range f.events {
		if strings.HasPrefix(event, "hook:") {
			t.Fatalf("sandbox hook escaped to host: %q", f.events)
		}
	}
}

func TestHostMergeKeepAndRetryCleanup(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [mergecheck]\npre_remove: [removecheck]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "topic\n")
	hostGit(t, w.Path, "commit", "-am", "topic")
	head := hostGit(t, w.Path, "rev-parse", "HEAD")
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.events, "hook:removecheck") || slices.Contains(f.events, "mux:close") {
		t.Fatalf("merge --keep cleaned up: %q", f.events)
	}
	state := f.load(t, "topic")
	if state.MergedCommit != head || state.MergeTarget != "main" || state.Stage != "merged" {
		t.Fatalf("missing successful merge record: %+v", state)
	}
	f.runner.hook = func(p Process) error {
		if p.Args[1] == "removecheck" {
			return fmt.Errorf("not yet")
		}
		return nil
	}
	if err := f.run(t, "merge", "topic"); err == nil {
		t.Fatal("expected hook failure")
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatal("failed hook removed worktree")
	}
	f.runner.hook = nil
	f.mux.onClose = func(w Workspace) {
		state := f.load(t, w.Branch)
		if state.Stage != "removing" || !state.Removal.HookDone || state.Removal.WorktreeRemoved {
			t.Fatalf("window must close after hooks and before deletion: %+v", state)
		}
	}
	f.app.Getwd = func() (string, error) { return w.Path, nil }
	if err := f.run(t, "merge"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" || !reflect.DeepEqual(state.Config, Config{}) {
		t.Fatalf("cleanup did not finalize and scrub state: %+v", state)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed: %v", err)
	}
	if refs := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); refs != "" {
		t.Fatalf("merged branch retained: %s", refs)
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "hook:mergecheck" })); count != 1 {
		t.Fatalf("merge retry reran pre_merge: %q", f.events)
	}
	f.app.Getwd = func() (string, error) { return f.root, nil }
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal("idempotent remove failed", err)
	}
}

func TestHostRemoveDirtyUnmergedAndKeepBranch(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [removecheck]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "dirty\n")
	if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
		t.Fatal("keep-branch permitted dirty removal")
	}
	hostGit(t, w.Path, "commit", "-am", "unmerged")
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("removed unmerged branch without force")
	}
	if err := f.run(t, "remove", "topic", "--force", "--keep-branch"); err != nil {
		t.Fatal(err)
	}
	if refs := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); refs != "refs/heads/topic" {
		t.Fatal("keep-branch deleted branch")
	}
	if !slices.Contains(f.events, "hook:removecheck") {
		t.Fatal("force skipped pre_remove")
	}
}

func TestHostIgnoredFilesPolicy(t *testing.T) {
	for _, test := range []struct {
		name    string
		changed bool
		unknown bool
	}{
		{"owned copy", false, false}, {"changed copy", true, false}, {"unknown ignored", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, "files: {copy: [.env]}\n")
			hostWrite(t, filepath.Join(f.root, ".env"), "secret=original\n")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			if test.changed {
				hostWrite(t, filepath.Join(w.Path, ".env"), "secret=changed\n")
			}
			if test.unknown {
				if err := os.Mkdir(filepath.Join(w.Path, "ignored"), 0700); err != nil {
					t.Fatal(err)
				}
				hostWrite(t, filepath.Join(w.Path, "ignored", "notes"), "keep me\n")
			}
			if err := f.run(t, "merge", "topic", "--keep"); err != nil {
				t.Fatal("ignored files blocked merge", err)
			}
			err := f.run(t, "remove", "topic")
			if test.changed || test.unknown {
				if err == nil || !strings.Contains(err.Error(), "ignored file") {
					t.Fatalf("expected ignored-file preservation, got %v", err)
				}
				if err := f.run(t, "remove", "topic", "--force"); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal("unchanged provisioned ignored file blocked cleanup", err)
			}
		})
	}
}

func TestHostMergeConflictAndRefChangesPreserveResources(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, *hostFixture, workspaceState)
	}{
		{"dirty target", func(t *testing.T, f *hostFixture, w workspaceState) {
			hostWrite(t, filepath.Join(f.root, "tracked"), "dirty\n")
		}},
		{"conflict", func(t *testing.T, f *hostFixture, w workspaceState) {
			hostWrite(t, filepath.Join(f.root, "tracked"), "main change\n")
			hostGit(t, f.root, "commit", "-am", "main change")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "topic change\n")
			hostGit(t, w.Path, "commit", "-am", "topic change")
		}},
		{"hook changes ref", func(t *testing.T, f *hostFixture, w workspaceState) {
			f.runner.hook = func(p Process) error {
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "hook changed ref")
				return nil
			}
		}},
		{"unfinished source", func(t *testing.T, f *hostFixture, w workspaceState) {
			path := hostGit(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "CHERRY_PICK_HEAD")
			hostWrite(t, path, hostGit(t, w.Path, "rev-parse", "HEAD")+"\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [check]\n")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			test.edit(t, f, w)
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("expected merge refusal")
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal("failed merge removed worktree", err)
			}
			if slices.Contains(f.events, "mux:close") || slices.Contains(f.events, "git:worktree remove") {
				t.Fatalf("failed merge removed resources: %q", f.events)
			}
		})
	}
}

func TestHostCleanupRetryAfterContainerFailure(t *testing.T) {
	f := newHostFixture(t, "sandbox: {enabled: true}\npre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.sandbox.rmErr = fmt.Errorf("engine offline")
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("expected container failure")
	}
	w := f.load(t, "topic")
	if w.Stage != "removing" || !w.Removal.WorktreeRemoved || !w.Removal.BranchDeleted || f.mux.window != "" {
		t.Fatalf("cleanup did not retain checkpoints after closing its window: %+v", w)
	}
	f.sandbox.rmErr = nil
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" {
		t.Fatal("retry did not finalize cleanup")
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "sandbox:hook:check" })); count != 1 {
		t.Fatalf("cleanup retry reran completed hook: %q", f.events)
	}
}

func TestHostConditionalBranchDeletePreservesChangedRef(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	initial := hostGit(t, f.root, "rev-parse", "main")
	hostGit(t, f.root, "commit", "--allow-empty", "-m", "later")
	later := hostGit(t, f.root, "rev-parse", "main")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 0 && args[0] == "update-ref" {
			if args[len(args)-1] != initial {
				t.Fatalf("missing expected old commit: %q", args)
			}
			hostGit(t, f.root, "update-ref", "refs/heads/topic", later, initial)
		}
		return nil
	}
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("deleted a concurrently changed ref")
	}
	if got := hostGit(t, f.root, "rev-parse", "topic"); got != later {
		t.Fatalf("changed branch lost: %s", got)
	}
	if f.mux.window != "" {
		t.Fatal("worktree was removed before its window closed")
	}
	f.runner.git = nil
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal("could not recover by preserving changed branch", err)
	}
}

func TestHostMergeRecoversUncertainSuccessfulResult(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "topic\n")
	hostGit(t, w.Path, "commit", "-am", "topic")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) != 0 && args[0] == "merge" {
			hostGit(t, p.Dir, args...)
			return fmt.Errorf("lost merge result after Git completed")
		}
		return nil
	}
	if err := f.run(t, "merge", "topic", "--keep"); err == nil {
		t.Fatal("expected uncertain merge result")
	}
	if state := f.load(t, "topic"); state.Stage != "merging" || state.PendingMergeCommit == "" || state.MergedCommit != "" {
		t.Fatalf("missing write-ahead merge record: %+v", state)
	}
	f.runner.git = nil
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Stage != "merged" || state.MergedCommit == "" || state.PendingMergeCommit != "" {
		t.Fatalf("merge was not recovered: %+v", state)
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "hook:check" })); count != 1 {
		t.Fatalf("uncertain merge retry reran hooks: %q", f.events)
	}
}

func TestHostMergeDoesNotAutostashConcurrentDirt(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "topic\n")
	hostGit(t, w.Path, "commit", "-am", "topic")
	hostGit(t, f.root, "config", "merge.autoStash", "true")
	hostGit(t, f.root, "config", "branch.main.mergeOptions", "--autostash")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 0 && args[0] == "merge" {
			if !slices.Contains(args, "--no-autostash") {
				t.Fatalf("merge did not disable configured autostash: %q", args)
			}
			hostWrite(t, filepath.Join(f.root, "tracked"), "concurrent edit\n")
		}
		return nil
	}
	if err := f.run(t, "merge", "topic"); err == nil {
		t.Fatal("merge overwrote concurrent dirt")
	}
	if refs := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/stash"); refs != "" {
		t.Fatal("merge created an automatic stash")
	}
	data, err := os.ReadFile(filepath.Join(f.root, "tracked"))
	if err != nil || string(data) != "concurrent edit\n" {
		t.Fatalf("concurrent work changed: %s, %v", data, err)
	}
}

func TestHostStopFailureNeverRemovesWorktreeOrWindow(t *testing.T) {
	f := newHostFixture(t, "sandbox: {enabled: true}\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.sandbox.stopErr = fmt.Errorf("cannot stop running writers")
	if err := f.run(t, "close", "topic"); err == nil {
		t.Fatal("close ignored stop failure")
	}
	if err := f.run(t, "remove", "topic", "--force"); err == nil {
		t.Fatal("force ignored stop failure")
	}
	if slices.Contains(f.events, "git:worktree remove") || slices.Contains(f.events, "mux:close") {
		t.Fatalf("stop failure removed resources: %q", f.events)
	}
	f.sandbox.stopErr = nil
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
}

func TestHostKeepBranchNeedsNoMergeTarget(t *testing.T) {
	for _, noTarget := range []bool{false, true} {
		t.Run(fmt.Sprintf("no target=%v", noTarget), func(t *testing.T) {
			f := newHostFixture(t, "")
			if noTarget {
				hostGit(t, f.root, "branch", "-m", "main", "trunk")
			}
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "unmerged\n")
			hostGit(t, w.Path, "commit", "-am", "unmerged")
			head := hostGit(t, w.Path, "rev-parse", "HEAD")
			f.runner.processes = nil
			if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
				t.Fatal(err)
			}
			if got := hostGit(t, f.root, "rev-parse", "topic"); got != head {
				t.Fatalf("retained branch changed: %s, want %s", got, head)
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if p.Name == "git" && len(args) > 0 && (args[0] == "merge-base" || args[0] == "symbolic-ref" || args[0] == "update-ref") {
					t.Fatalf("keep-branch checked a target or deleted a ref: %q", args)
				}
			}
		})
	}
}

func TestHostKeepBranchStillProtectsWorktreeData(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, workspaceState)
	}{
		{"dirty", func(t *testing.T, w workspaceState) { hostWrite(t, filepath.Join(w.Path, "tracked"), "dirty\n") }},
		{"untracked", func(t *testing.T, w workspaceState) { hostWrite(t, filepath.Join(w.Path, "untracked"), "keep\n") }},
		{"ignored", func(t *testing.T, w workspaceState) { hostWrite(t, filepath.Join(w.Path, ".env"), "keep\n") }},
		{"unfinished", func(t *testing.T, w workspaceState) {
			path := hostGit(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "CHERRY_PICK_HEAD")
			hostWrite(t, path, hostGit(t, w.Path, "rev-parse", "HEAD")+"\n")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, "")
			hostGit(t, f.root, "branch", "-m", "main", "trunk")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			test.edit(t, w)
			if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
				t.Fatal("keep-branch bypassed data protection")
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal("protected worktree disappeared", err)
			}
			if slices.Contains(f.events, "git:worktree remove") {
				t.Fatal("unsafe removal was attempted")
			}
		})
	}
}

func TestHostKeepBranchRetryAdoptsNewCommitAndRerunsHook(t *testing.T) {
	for _, completedHook := range []bool{false, true} {
		t.Run(fmt.Sprintf("previous hook completed=%v", completedHook), func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			hookCalls := 0
			f.runner.hook = func(Process) error {
				hookCalls++
				if !completedHook {
					return fmt.Errorf("fix the branch before retrying")
				}
				return nil
			}
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
					return fmt.Errorf("injected worktree removal failure")
				}
				return nil
			}
			if err := f.run(t, "remove", "topic"); err == nil {
				t.Fatal("expected failed cleanup")
			}
			if state := f.load(t, "topic"); state.Removal == nil || state.Removal.HookDone != completedHook {
				t.Fatalf("incorrect hook checkpoint: %+v", state.Removal)
			}
			hostWrite(t, filepath.Join(w.Path, "tracked"), "fix hook\n")
			hostGit(t, w.Path, "commit", "-am", "fix hook")
			head := hostGit(t, w.Path, "rev-parse", "HEAD")
			f.runner.git = nil
			f.runner.hook = func(Process) error {
				hookCalls++
				state := f.load(t, "topic")
				if state.Removal.Head != head || !state.Removal.KeepBranch || state.Removal.HookDone || state.Removal.WorktreeRemovalStarted {
					t.Fatalf("new head was not safely recorded before hook: %+v", state.Removal)
				}
				return nil
			}
			if err := f.run(t, "remove", "topic"); err == nil {
				t.Fatal("adopted new commit without explicit keep-branch")
			}
			if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
				t.Fatal(err)
			}
			if hookCalls != 2 {
				t.Fatalf("hook calls = %d, want 2", hookCalls)
			}
			if got := hostGit(t, f.root, "rev-parse", "topic"); got != head {
				t.Fatalf("new commit was lost: %s, want %s", got, head)
			}
			if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("worktree was not removed: %v", err)
			}
		})
	}
}

func TestHostRepeatedKeptMergesAndLaterCleanup(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [mergecheck]\npre_remove: [removecheck]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	var previous string
	for _, text := range []string{"first\n", "second\n"} {
		hostWrite(t, filepath.Join(w.Path, "tracked"), text)
		hostGit(t, w.Path, "commit", "-am", strings.TrimSpace(text))
		head := hostGit(t, w.Path, "rev-parse", "HEAD")
		if head == previous {
			t.Fatal("test did not create a new source commit")
		}
		if err := f.run(t, "merge", "topic", "--keep"); err != nil {
			t.Fatal(err)
		}
		state := f.load(t, "topic")
		if !state.MergeKept || state.MergedCommit != head || state.Removal != nil {
			t.Fatalf("incorrect retained merge record: %+v", state)
		}
		if got := hostGit(t, f.root, "rev-parse", "HEAD"); got != head {
			t.Fatalf("target was not merged: %s, want %s", got, head)
		}
		previous = head
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "hook:mergecheck" })); count != 2 {
		t.Fatalf("new kept merge did not rerun validation hooks: %q", f.events)
	}
	if slices.Contains(f.events, "hook:removecheck") {
		t.Fatal("kept merge ran cleanup hook")
	}
	if err := f.run(t, "merge", "topic"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" || state.MergedCommit != previous || state.MergeKept {
		t.Fatalf("later cleanup lost completion record: %+v", state)
	}
}

func TestHostKeptMergeDoesNotResetPendingCleanup(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.runner.hook = func(Process) error { return fmt.Errorf("cleanup failed") }
	if err := f.run(t, "merge", "topic"); err == nil || !strings.Contains(err.Error(), "merge succeeded; cleanup incomplete") {
		t.Fatalf("missing successful-merge/failed-cleanup distinction: %v", err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "new work\n")
	hostGit(t, w.Path, "commit", "-am", "new work")
	if err := f.run(t, "merge", "topic", "--keep"); err == nil || !strings.Contains(err.Error(), "cleanup is already in progress") {
		t.Fatalf("kept merge reset pending cleanup: %v", err)
	}
	if state := f.load(t, "topic"); state.Removal.Head != w.Removal.Head || state.MergeKept {
		t.Fatalf("pending cleanup was changed: %+v", state)
	}
}

func TestHostMergeFinalCloseFailureReportsSuccessfulMerge(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.closeErr = fmt.Errorf("window server unavailable")
	if err := f.run(t, "merge", "topic"); err == nil || !strings.Contains(err.Error(), "merge succeeded; cleanup incomplete") {
		t.Fatalf("missing final-close cleanup error: %v", err)
	}
}

func TestHostShutdownIsRecheckedAfterHooksAndCanBeClosed(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.runner.hook = func(Process) error {
		f.mux.quiesceErr = fmt.Errorf("hook opened a new writer window")
		return nil
	}
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("cleanup did not recheck writer shutdown after the hook")
	}
	state := f.load(t, "topic")
	if state.Removal == nil || !state.Removal.HookDone || state.Removal.Stopped || state.Removal.WorktreeRemovalStarted {
		t.Fatalf("incorrect interrupted shutdown record: %+v", state.Removal)
	}
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal("pending host cleanup must permit explicit close", err)
	}
	f.mux.quiesceErr = nil
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "hook:check" })); count != 1 {
		t.Fatalf("completed hook was rerun after explicit close: %q", f.events)
	}
}

func TestHostUncertainWorktreeRemovalCheckpoint(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
			state := f.load(t, "topic")
			if !state.Removal.WorktreeRemovalStarted || state.Removal.WorktreeRemoved || !state.Removal.Stopped {
				t.Fatalf("missing removal intent before mutation: %+v", state.Removal)
			}
			hostGit(t, p.Dir, args...)
			return fmt.Errorf("lost result after successful worktree removal")
		}
		return nil
	}
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("expected uncertain removal result")
	}
	w := f.load(t, "topic")
	if w.Removal.WorktreeRemoved || !w.Removal.WorktreeRemovalStarted {
		t.Fatalf("uncertain removal was not retained: %+v", w.Removal)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("test did not remove worktree: %v", err)
	}
	f.runner.git = nil
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal("could not recover uncertain removal", err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" {
		t.Fatalf("recovery did not complete: %+v", state)
	}
	if count := len(slices.DeleteFunc(slices.Clone(f.events), func(event string) bool { return event != "hook:check" })); count != 1 {
		t.Fatalf("uncertain removal reran completed hook: %q", f.events)
	}
}

func TestHostMissingWorktreeBeforeRemovalCheckpointIsNotSuccess(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.runner.hook = func(Process) error { return fmt.Errorf("hook failed") }
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("expected failed hook")
	}
	w := f.load(t, "topic")
	hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	f.runner.hook = nil
	if err := f.run(t, "remove", "topic"); err == nil || !strings.Contains(err.Error(), "before the recorded Git removal step") {
		t.Fatalf("external deletion was treated as successful cleanup: %v", err)
	}
	if got := hostGit(t, f.root, "rev-parse", "topic"); got != w.Removal.Head {
		t.Fatal("branch was removed without a recorded Git removal attempt")
	}
}

func TestHostRemoveFromActualDeletedCwd(t *testing.T) {
	if os.Getenv("WORKMUX_TEST_DELETED_CWD") == "1" {
		var events []string
		runner := &hostTestRunner{events: &events, git: func(p Process) error {
			if !filepath.IsAbs(p.Dir) {
				return fmt.Errorf("Git used the caller cwd instead of an absolute directory")
			}
			return nil
		}}
		mux := &hostTestMux{events: &events}
		app := App{Runner: runner, Mux: mux, HomeDir: os.Getenv("WORKMUX_TEST_HOME"),
			StateDir: os.Getenv("WORKMUX_TEST_STATE"), ConfigDir: os.Getenv("WORKMUX_TEST_CONFIG")}
		if err := app.Run(context.Background(), Command{Kind: "remove", KeepBranch: true}); err != nil {
			t.Fatal(err)
		}
		if cwd, err := os.Getwd(); err == nil {
			t.Fatalf("caller cwd was not actually deleted: %s", cwd)
		}
		return
	}
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestHostRemoveFromActualDeletedCwd$")
	cmd.Dir = w.Path
	cmd.Env = append(os.Environ(), "WORKMUX_TEST_DELETED_CWD=1", "WORKMUX_TEST_HOME="+f.home,
		"WORKMUX_TEST_STATE="+f.state, "WORKMUX_TEST_CONFIG="+f.config)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cleanup from actual removed cwd failed: %v\n%s", err, out)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" {
		t.Fatalf("child did not finalize state: %+v", state)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("child did not delete worktree: %v", err)
	}
	if got := hostGit(t, f.root, "rev-parse", "topic"); got != w.InitialCommit {
		t.Fatal("child deleted retained branch")
	}
}
