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
		args []string
		want Command
	}{
		{nil, Command{Help: true}},
		{[]string{"help", "add"}, Command{Kind: "add", Help: true}},
		{[]string{"add", "topic", "--base=main", "-l", "dev"}, Command{Kind: "add", Name: "topic", Base: "main", Layout: "dev", Names: []string{"topic"}}},
		{[]string{"merge", "--into", "release"}, Command{Kind: "merge", Into: "release"}},
		{[]string{"merge", "topic", "--into=release"}, Command{Kind: "merge", Name: "topic", Names: []string{"topic"}, Into: "release"}},
		{[]string{"merge"}, Command{Kind: "merge"}},
		{[]string{"merge", "topic"}, Command{Kind: "merge", Name: "topic", Names: []string{"topic"}}},
		{[]string{"merge", "--help"}, Command{Kind: "merge", Help: true}},
		{[]string{"merge", "-h"}, Command{Kind: "merge", Help: true}},
		{[]string{"rm", "one", "two"}, Command{Kind: "remove", Name: "one", Names: []string{"one", "two"}}},
		{[]string{"open", "--", "one", "two"}, Command{Kind: "open", Name: "one", Names: []string{"one", "two"}}},
		{[]string{"close"}, Command{Kind: "close"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			got, err := ParseCommand(test.args)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
	for _, args := range [][]string{{"add"}, {"open"}, {"merge", "one", "two"}, {"close", "one", "two"}, {"add", "topic", "--base"}, {"add", "topic", "--layout="}, {"add", "topic", "--layout=x", "-l", "x"}, {"merge", "--into"}, {"merge", "--into="}, {"merge", "--into=main", "--into=release"}, {"add", "one\ntwo"}, {"open", "--", "-bad"}, {"merge", "topic", "--unknown"}, {"remove", "topic", "--layout=review"}, {"unknown"}} {
		if _, err := ParseCommand(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
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
	for len(args) > 0 {
		switch {
		case len(args) >= 2 && args[0] == "-c":
			args = args[2:]
		case strings.HasPrefix(args[0], "--config-env="):
			args = args[1:]
		default:
			return args
		}
	}
	return args
}

func (runner *hostTestRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	runner.processes = append(runner.processes, p)
	if p.Name == "git" {
		args := hostGitArgs(p)
		if len(args) > 0 && (args[0] == "merge" || args[0] == "update-ref" || args[0] == "worktree" && len(args) > 1 && args[1] != "list") {
			event := "git:" + strings.Join(args[:min(2, len(args))], " ")
			if args[0] == "merge" {
				event = "git:merge"
			}
			*runner.events = append(*runner.events, event)
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
	findErr                                     error
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
	return mux.window, mux.findErr
}
func (mux *hostTestMux) Create(_ context.Context, _ string, w Workspace, panes []Pane, commands [][]string) (string, error) {
	*mux.events = append(*mux.events, "mux:create")
	if mux.onCreate != nil {
		mux.onCreate(w)
	}
	mux.panes, mux.commands, mux.window = panes, commands, "@7"
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
	if mux.window == "" {
		return false, nil
	}
	if window.Token != mux.cleanupToken {
		return false, fmt.Errorf("stale captured window")
	}
	return true, nil
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
	existsErr                           error
	present                             bool
	exec                                func(Workspace, string, []string) error
}

func (sandbox *hostTestSandbox) Check(context.Context, SandboxConfig) error {
	*sandbox.events = append(*sandbox.events, "sandbox:check")
	return sandbox.checkErr
}
func (sandbox *hostTestSandbox) Ensure(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:ensure")
	if sandbox.ensureErr == nil {
		sandbox.present = true
	}
	return sandbox.ensureErr
}
func (sandbox *hostTestSandbox) Exists(context.Context, Workspace) (bool, error) {
	*sandbox.events = append(*sandbox.events, "sandbox:exists")
	return sandbox.present, sandbox.existsErr
}
func (sandbox *hostTestSandbox) Exec(_ context.Context, w Workspace, command string, env []string, _ io.Reader, _, _ io.Writer) error {
	*sandbox.events = append(*sandbox.events, "sandbox:hook:"+command)
	if sandbox.exec != nil {
		return sandbox.exec(w, command, env)
	}
	return nil
}
func (sandbox *hostTestSandbox) Stop(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:stop")
	return sandbox.stopErr
}
func (sandbox *hostTestSandbox) Remove(context.Context, Workspace) error {
	*sandbox.events = append(*sandbox.events, "sandbox:remove")
	if sandbox.rmErr == nil {
		sandbox.present = false
	}
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
	f.app = App{Runner: f.runner, Mux: f.mux, Sandbox: f.sandbox, HomeDir: f.home, StateDir: f.state, ConfigDir: f.config, Getwd: func() (string, error) { return f.root, nil }, Stdout: &f.stdout}
	return f
}

func (f *hostFixture) run(t *testing.T, args ...string) error {
	t.Helper()
	command, err := ParseCommand(args)
	if err != nil {
		t.Fatal(err)
	}
	return f.app.Run(t.Context(), command)
}
func (f *hostFixture) load(t *testing.T, name string) workspaceState {
	t.Helper()
	path, common := workspacePath(f.root, workspaceSlug(name)), filepath.Join(f.root, ".git")
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

func TestAddExistingAndOpenNeverReprovision(t *testing.T) {
	const config = `post_create: [create-hook]
files:
  copy: [.env]
panes:
  - command: default-shell
    focus: true
layouts:
  review:
    panes:
      - command: review-diff
        focus: true
      - command: review-tests
        split: vertical
  alternate:
    panes:
      - command: alternate-editor
      - command: alternate-shell
        split: horizontal
        focus: true
`
	alternate := []Pane{{Command: "alternate-editor"}, {Command: "alternate-shell", Split: "horizontal", Focus: true}}
	review := []Pane{{Command: "review-diff", Focus: true}, {Command: "review-tests", Split: "vertical"}}
	defaults := []Pane{{Command: "default-shell", Focus: true}}
	for _, test := range []struct {
		name, kind, window, layout string
		panes                      []Pane
	}{
		{"add live", "add", "live", "", alternate},
		{"add review live", "add", "live", "review", alternate},
		{"add review closed", "add", "closed", "review", review},
		{"add review missing", "add", "missing", "review", review},
		{"open live", "open", "live", "", alternate},
		{"open closed", "open", "closed", "", defaults},
		{"open missing", "open", "missing", "", defaults},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, config)
			hostWrite(t, filepath.Join(f.root, ".env"), "original")
			if err := f.run(t, "add", "Feature/Auth_OAuth", "--layout", "alternate"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "Feature/Auth_OAuth")
			if w.Path != filepath.Join(filepath.Dir(f.root), "worktrees__test repo", "feature-auth-oauth") || w.Layout != "alternate" {
				t.Fatalf("wrong path or layout: %+v", w)
			}
			if !slices.Contains(f.events, "hook:create-hook") || !slices.Contains(f.events, "git:worktree add") {
				t.Fatalf("initial provisioning missing: %q", f.events)
			}
			if !reflect.DeepEqual(f.mux.panes, alternate) || !reflect.DeepEqual(f.mux.commands, [][]string{{"alternate-editor"}, {"alternate-shell"}}) {
				t.Fatalf("initial panes = %+v, commands = %q", f.mux.panes, f.mux.commands)
			}
			checkFile(t, filepath.Join(w.Path, ".env"), "original")
			hostWrite(t, filepath.Join(f.root, ".env"), "updated source")
			hostWrite(t, filepath.Join(w.Path, ".env"), "workspace")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "local edit")
			hostWrite(t, filepath.Join(w.Path, "untracked"), "keep")
			switch test.window {
			case "closed":
				if err := f.run(t, "close", w.Handle); err != nil {
					t.Fatal(err)
				}
				if f.mux.window != "" || f.load(t, w.Branch).Stage != "closed" {
					t.Fatal("close did not close the workspace window")
				}
			case "missing":
				f.mux.window = ""
			}
			f.events = nil
			args := []string{test.kind, w.Branch}
			if test.layout != "" {
				args = append(args, "--layout", test.layout)
			}
			if err := f.run(t, args...); err != nil {
				t.Fatal(err)
			}
			wantEvents := []string{"mux:session", "mux:find"}
			if test.window != "live" {
				wantEvents = append(wantEvents, "mux:create")
			}
			wantEvents = append(wantEvents, "mux:focus")
			if !slices.Equal(f.events, wantEvents) {
				t.Fatalf("events = %q, want %q", f.events, wantEvents)
			}
			commands := make([][]string, len(test.panes))
			for i, pane := range test.panes {
				commands[i] = []string{pane.Command}
			}
			if !reflect.DeepEqual(f.mux.panes, test.panes) || !reflect.DeepEqual(f.mux.commands, commands) {
				t.Fatalf("panes = %+v, commands = %q; want %+v, %q", f.mux.panes, f.mux.commands, test.panes, commands)
			}
			if f.mux.window == "" || f.load(t, w.Branch).Path != w.Path {
				t.Fatal("existing checkout or window lost")
			}
			checkFile(t, filepath.Join(f.root, ".env"), "updated source")
			checkFile(t, filepath.Join(w.Path, ".env"), "workspace")
			checkFile(t, filepath.Join(w.Path, "tracked"), "local edit")
			checkFile(t, filepath.Join(w.Path, "untracked"), "keep")
			if err := f.run(t, "add", "main"); err == nil {
				t.Fatal("main checkout adopted")
			}
		})
	}
}

func TestAddExistingPlainWorktreeRetainsActualPath(t *testing.T) {
	f := newHostFixture(t, "post_create: [must-not-run]\n")
	path := filepath.Join(t.TempDir(), "external")
	hostGit(t, f.root, "worktree", "add", "-b", "topic", path)
	f.runner.hook = func(Process) error { t.Fatal("existing worktree hook ran"); return nil }
	if err := f.run(t, "add", "topic"); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(f.root, ".git")
	w, err := readState(filepath.Join(f.state, identity(common), identity(common, path)+".json"))
	if err != nil || w.Path != path {
		t.Fatalf("plain worktree relocated: %+v %v", w, err)
	}
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("actual path not removed")
	}
}

func TestAddBroadFilesPreservesGitIdentityBeforeHooks(t *testing.T) {
	for _, pattern := range []string{".", "**", "*", "absolute root", "absolute glob"} {
		t.Run(pattern, func(t *testing.T) {
			f := newHostFixture(t, "")
			selected := pattern
			if pattern == "absolute root" {
				selected = f.root
			} else if pattern == "absolute glob" {
				selected = filepath.Join(f.root, "**")
			}
			hostWrite(t, filepath.Join(f.config, "config.yaml"), "files: {copy: ['"+selected+"']}\npost_create:\n  - |\n    set -eu\n    git symbolic-ref --short HEAD > hook-branch\n    git rev-parse --absolute-git-dir > hook-admin\n")
			hostWrite(t, filepath.Join(f.root, ".env"), "private configuration\n")
			mainHead := hostGit(t, f.root, "rev-parse", "refs/heads/main")
			metadata := make(map[string][]byte)
			for _, name := range []string{"HEAD", "config", "index"} {
				data, err := os.ReadFile(filepath.Join(f.root, ".git", name))
				if err != nil {
					t.Fatal(err)
				}
				metadata[name] = data
			}
			called := false
			f.runner.hook = func(p Process) error {
				called = true
				if got := hostGit(t, p.Dir, "symbolic-ref", "--short", "HEAD"); got != "feature/topic" {
					t.Fatalf("hook checkout branch = %q", got)
				}
				_, err := (ExecRunner{}).Run(t.Context(), p)
				return err
			}
			if err := f.run(t, "add", "feature/topic"); err != nil {
				t.Fatal(err)
			}
			if !called {
				t.Fatal("post_create hook did not run")
			}
			w := f.load(t, "feature/topic")
			admin := hostGit(t, w.Path, "rev-parse", "--absolute-git-dir")
			if filepath.Dir(admin) != filepath.Join(f.root, ".git", "worktrees") {
				t.Fatalf("linked administration directory = %q", admin)
			}
			checkFile(t, filepath.Join(w.Path, ".git"), "gitdir: "+admin+"\n")
			checkFile(t, filepath.Join(w.Path, "hook-branch"), "feature/topic\n")
			checkFile(t, filepath.Join(w.Path, "hook-admin"), admin+"\n")
			checkFile(t, filepath.Join(w.Path, ".env"), "private configuration\n")
			for name, before := range metadata {
				after, err := os.ReadFile(filepath.Join(f.root, ".git", name))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("main %s changed: %v", name, err)
				}
			}
			if hostGit(t, f.root, "rev-parse", "refs/heads/main") != mainHead {
				t.Fatal("main branch changed")
			}
			if _, err := (gitHost{runner: f.runner}).discover(t.Context(), w.Path); err != nil {
				t.Fatalf("provisioned checkout is invalid: %v", err)
			}
		})
	}
}

func TestAddValidatesProvisionedWorktreeBeforeHooks(t *testing.T) {
	f := newHostFixture(t, "files: {copy: [.env]}\npost_create: [must-not-run]\n")
	hostWrite(t, filepath.Join(f.root, ".env"), "provisioned\n")
	path := workspacePath(f.root, "topic")
	changed := false
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if !changed && len(args) >= 2 && args[0] == "worktree" && args[1] == "list" {
			if _, err := os.Stat(filepath.Join(path, ".env")); err == nil {
				hostWrite(t, filepath.Join(path, ".git"), "gitdir: "+filepath.Join(f.root, ".git")+"\n")
				changed = true
			}
		}
		return nil
	}
	f.runner.hook = func(Process) error {
		t.Fatal("post_create ran before worktree validation")
		return nil
	}
	if err := f.run(t, "add", "topic"); err == nil {
		t.Fatal("invalid provisioned checkout accepted")
	}
	if !changed {
		t.Fatal("worktree validation was not reached after provisioning")
	}
	if f.mux.window != "" {
		t.Fatal("invalid checkout opened a tmux window")
	}
	checkFile(t, filepath.Join(path, ".env"), "provisioned\n")
	if w := f.load(t, "topic"); w.Stage == "ready" || w.Stage == "removed" {
		t.Fatalf("invalid checkout stage = %q", w.Stage)
	}
}

func TestSelectedPaneGeometryPrecedesProvisioning(t *testing.T) {
	for _, kind := range []string{"add", "open", "existing add"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "panes: [{}]\n")
			if kind != "add" {
				if err := f.run(t, "add", "topic"); err != nil {
					t.Fatal(err)
				}
			}
			hostWrite(t, filepath.Join(f.root, ".env"), "source")
			hostWrite(t, filepath.Join(f.config, "config.yaml"), "panes: [{}, {}]\nfiles: {copy: [.env]}\npost_create: [must-not-run]\n")
			f.events = nil
			command := "add"
			if kind == "open" {
				command = "open"
			}
			if err := f.run(t, command, "topic"); err == nil || !strings.Contains(err.Error(), "split") {
				t.Fatalf("bad geometry accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(workspacePath(f.root, "topic"), ".env")); !os.IsNotExist(err) {
				t.Fatal("files copied before geometry validation")
			}
			for _, event := range f.events {
				if strings.HasPrefix(event, "git:") || strings.HasPrefix(event, "hook:") || event == "mux:create" || event == "mux:focus" {
					t.Fatalf("geometry failure mutated lifecycle: %q", f.events)
				}
			}
		})
	}
}

func TestHostAddUsesCurrentHeadAndReusesLocalBranches(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "first"); err != nil {
		t.Fatal(err)
	}
	first := f.load(t, "first")
	hostWrite(t, filepath.Join(first.Path, "tracked"), "first")
	hostGit(t, first.Path, "commit", "-am", "first")
	head := hostGit(t, first.Path, "rev-parse", "HEAD")
	f.app.Getwd = func() (string, error) { return first.Path, nil }
	if err := f.run(t, "add", "second"); err != nil {
		t.Fatal(err)
	}
	if got := hostGit(t, f.load(t, "second").Path, "rev-parse", "HEAD"); got != head {
		t.Fatalf("new branch HEAD = %s, want %s", got, head)
	}
	hostGit(t, f.root, "branch", "existing", "main")
	if err := f.run(t, "add", "existing"); err != nil {
		t.Fatal(err)
	}
	if f.load(t, "existing").CreatedBranch {
		t.Fatal("existing local branch marked created")
	}
}

func TestHostAddRejectsSlugCollisionAndUnownedPath(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "feature/topic"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "add", "feature-topic"); err == nil {
		t.Fatal("accepted colliding branch handle")
	}
	path := workspacePath(f.root, "unowned")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	hostWrite(t, filepath.Join(path, "important"), "keep")
	if err := f.run(t, "add", "unowned"); err == nil {
		t.Fatal("accepted unowned destination")
	}
	checkFile(t, filepath.Join(path, "important"), "keep")
}

func TestHostPreflightAndPartialAdd(t *testing.T) {
	for _, test := range []struct {
		name, config, stage string
		setup               func(*hostFixture)
	}{
		{"tmux", "", "", func(f *hostFixture) { f.mux.sessionErr = fmt.Errorf("not in tmux") }},
		{"worktree", "", "planned", func(f *hostFixture) {
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 1 && args[0] == "worktree" && args[1] == "add" {
					if f.load(t, "topic").Stage != "planned" {
						t.Fatal("missing write-ahead add state")
					}
					return fmt.Errorf("injected add failure")
				}
				return nil
			}
		}},
		{"hook", "post_create: [fail]\n", "sandbox", func(f *hostFixture) { f.runner.hook = func(Process) error { return fmt.Errorf("hook failed") } }},
		{"panes", "", "ready", func(f *hostFixture) { f.mux.createErr = fmt.Errorf("split failed") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, test.config)
			test.setup(f)
			if err := f.run(t, "add", "topic"); err == nil {
				t.Fatal("injected failure succeeded")
			}
			if test.stage == "" {
				if slices.Contains(f.events, "git:worktree add") {
					t.Fatal("Git mutated before preflight")
				}
				return
			}
			w := f.load(t, "topic")
			if w.Stage != test.stage {
				t.Fatalf("stage %s, want %s", w.Stage, test.stage)
			}
			if test.stage != "planned" {
				if _, err := os.Stat(w.Path); err != nil {
					t.Fatal("partial worktree lost", err)
				}
			}
		})
	}
}

func TestHostAddLayoutAndOpenFreshPanes(t *testing.T) {
	f := newHostFixture(t, "post_create: [created]\nlayouts:\n  dev:\n    panes:\n      - command: editor\n      - command: tests\n        split: vertical\n")
	if err := f.run(t, "add", "topic", "-l", "dev"); err != nil {
		t.Fatal(err)
	}
	if f.load(t, "topic").Layout != "dev" || len(f.mux.panes) != 2 || !slices.Contains(f.events, "mux:focus") {
		t.Fatal("layout or focus lost")
	}
	hostEventBefore(t, f.events, "hook:created", "mux:create")
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal(err)
	}
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "post_create: [must-not-run]\npanes: [{command: new}]\n")
	f.events = nil
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal(err)
	}
	if len(f.mux.panes) != 1 || f.mux.panes[0].Command != "new" || slices.Contains(f.events, "hook:must-not-run") {
		t.Fatal("open reused saved panes or ran setup")
	}
}

func TestMergeHooksSurviveCleanupWorkerHandoff(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [merge-check]\npre_remove: [remove-check]\n")
	policyCommit(t, f)
	f.mux.caller = true
	var launch CleanupLaunch
	f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, captured CleanupLaunch) error { launch = captured; return nil })
	if err := f.run(t, "merge", "topic"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	if state.Removal == nil || state.Removal.Job == nil || !state.Removal.HookDone || !slices.Equal(state.Config.PreMerge, []string{"merge-check"}) || !slices.Equal(state.Config.PreRemove, []string{"remove-check"}) {
		t.Fatal("handoff lost configured hooks or their completion checkpoint")
	}
	hostEventBefore(t, f.events, "hook:merge-check", "git:merge")
	hostEventBefore(t, f.events, "git:merge", "hook:remove-check")
	hostEventBefore(t, f.events, "hook:remove-check", "mux:capture")
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "pre_merge: [replacement-merge]\npre_remove: [replacement-remove]\n")
	f.runner.hook = func(Process) error {
		t.Fatal("worker reran lifecycle hooks after their completion checkpoint")
		return nil
	}
	snapshotChecked := false
	f.mux.onClose = func(w Workspace) {
		snapshotChecked = true
		if !slices.Equal(w.Config.PreMerge, state.Config.PreMerge) || !slices.Equal(w.Config.PreRemove, state.Config.PreRemove) {
			t.Fatal("worker reloaded hooks instead of preserving the handoff snapshot")
		}
	}
	state.Removal.Job.Status = "queued"
	savePolicyState(t, f, state)
	if err := f.app.RunCleanup(t.Context(), launch.Command, cleanupReadyFunc(func(data []byte) (int, error) {
		state.Removal.Job.Status = "armed"
		savePolicyState(t, f, state)
		return len(data), nil
	})); err != nil {
		t.Fatal(err)
	}
	if !snapshotChecked || f.load(t, "topic").Stage != "removed" {
		t.Fatal("worker failed to close the window and complete cleanup")
	}
}

func TestMergeRunsConfiguredHooks(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [merge-check]\npre_remove: [remove-check]\n")
	policyCommit(t, f)
	if err := f.run(t, "merge", "topic"); err != nil {
		t.Fatal(err)
	}
	hostEventBefore(t, f.events, "hook:merge-check", "git:merge")
	hostEventBefore(t, f.events, "git:merge", "hook:remove-check")
	hostEventBefore(t, f.events, "hook:remove-check", "git:worktree remove")
	if f.load(t, "topic").Stage != "removed" {
		t.Fatal("successful merge did not clean up the workspace")
	}
}
