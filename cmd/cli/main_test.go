package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jiyeol-lee/cli/internal/gcal"
	"github.com/jiyeol-lee/cli/internal/memory"
	"github.com/jiyeol-lee/cli/internal/workmux"
	"github.com/jiyeol-lee/cli/internal/xdg"
)

func TestInvalidGcalInvocationsDoNotConstructOAuthDependencies(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "missing", args: []string{"gcal"}, wantErr: true},
		{name: "unknown", args: []string{"gcal", "remove"}, wantErr: true},
		{name: "invalid option", args: []string{"gcal", "list", "--json"}, wantErr: true},
		{name: "extra argument", args: []string{"gcal", "soon", "--text", "extra"}, wantErr: true},
		{name: "help", args: []string{"gcal", "--help"}},
		{name: "command help", args: []string{"gcal", "list", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var out bytes.Buffer
			deps := dependencies{
				stdout: &out,
				newCalendar: func(context.Context, xdg.Dirs) (gcal.Calendar, error) {
					called = true
					return gcal.Calendar{}, nil
				},
			}
			err := runWithDependencies(context.Background(), tt.args, deps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if called {
				t.Fatal("calendar dependency was constructed")
			}
		})
	}
}

func TestInvalidCommandsDoNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown app", args: []string{"typo"}, want: `unknown app "typo"`},
		{name: "missing voca command", args: []string{"voca"}, want: "usage: cli voca"},
		{name: "invalid voca command", args: []string{"voca", "typo"}, want: `unknown voca command "typo"`},
		{name: "missing add phrase", args: []string{"voca", "add"}, want: "usage: cli voca add <phrase>"},
		{name: "missing delete phrase", args: []string{"voca", "delete"}, want: "usage: cli voca delete <phrase>"},
		{name: "extra list argument", args: []string{"voca", "list", "extra"}, want: "usage: cli voca list"},
		{name: "extra story argument", args: []string{"voca", "story", "extra"}, want: "usage: cli voca story"},
		{name: "extra news argument", args: []string{"voca", "news", "extra"}, want: "usage: cli voca news"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithDependencies(context.Background(), tt.args, dependencies{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestGcalHelpDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	called := false
	var out bytes.Buffer
	deps := dependencies{
		stdout: &out,
		newCalendar: func(context.Context, xdg.Dirs) (gcal.Calendar, error) {
			called = true
			return gcal.Calendar{}, nil
		},
	}
	if err := runWithDependencies(context.Background(), []string{"gcal", "--help"}, deps); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("calendar dependency was constructed")
	}
	if got, want := out.String(), gcal.Usage+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestInvalidMemoryCommandsDoNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"memory", "remove"}, want: `unknown memory command "remove"`},
		{name: "bad read scope", args: []string{"memory", "read", "--scope", "local"}, want: "invalid memory scope"},
		{name: "missing write options", args: []string{"memory", "write", "note"}, want: "usage: cli memory write"},
		{name: "bad archive id", args: []string{"memory", "archive", "zero", "--scope", "global"}, want: "positive integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithDependencies(context.Background(), tt.args, dependencies{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMemoryHelpDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	for _, args := range [][]string{{"memory"}, {"memory", "--help"}, {"memory", "-h"}} {
		var out bytes.Buffer
		if err := runWithDependencies(context.Background(), args, dependencies{stdout: &out}); err != nil {
			t.Fatal(err)
		}
		if got, want := out.String(), memory.Usage+"\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	}
}

type fixedDirectoryResolver struct {
	directory string
}

func (r fixedDirectoryResolver) Resolve(context.Context) (string, error) {
	return r.directory, nil
}

func TestMemoryDirectoryDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	var out bytes.Buffer
	deps := dependencies{
		directoryResolver: fixedDirectoryResolver{directory: "/main/worktree"},
		stdout:            &out,
	}
	if err := runWithDependencies(context.Background(), []string{"memory", "directory"}, deps); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "/main/worktree\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestWorkmuxParsingDoesNotConstructDependencies(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("HOME", "")
	for _, tt := range []struct {
		args    []string
		wantErr bool
	}{
		{args: []string{"workmux"}},
		{args: []string{"workmux", "--help"}},
		{args: []string{"workmux", "add", "--help"}},
		{args: []string{"workmux", "help", "remove"}},
		{args: []string{"workmux", "add"}, wantErr: true},
		{args: []string{"workmux", "list"}, wantErr: true},
		{args: []string{"workmux", "merge", "--force"}, wantErr: true},
		{args: []string{"workmux", "remove", "one", "two"}, wantErr: true},
		{args: []string{"workmux", "_cleanup"}, wantErr: true},
		{args: []string{"workmux", "_cleanup", "--help"}, wantErr: true},
	} {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			deps := dependencies{stdout: &out, newWorkmux: func() (workmux.App, error) {
				t.Fatal("workmux dependencies constructed for help or invalid arguments")
				return workmux.App{}, nil
			}}
			err := runWithDependencies(context.Background(), tt.args, deps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && out.String() != workmux.Usage {
				t.Fatalf("stdout = %q, want workmux usage", out.String())
			}
		})
	}
}

type failingWorkmuxRunner struct {
	called bool
}

func (r *failingWorkmuxRunner) Run(context.Context, workmux.Process) ([]byte, error) {
	r.called = true
	return nil, fmt.Errorf("test Git unavailable")
}

func TestWorkmuxDispatchDoesNotOpenDatabase(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "relative-and-invalid")
	home := t.TempDir()
	// Filesystem discovery validates .git before invoking Git.
	repo := filepath.Join(home, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n\tbare = false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &failingWorkmuxRunner{}
	deps := dependencies{newWorkmux: func() (workmux.App, error) {
		return workmux.App{
			Runner: runner, HomeDir: home, StateDir: filepath.Join(home, "state"),
			Getwd: func() (string, error) { return repo, nil },
		}, nil
	}}
	err := runWithDependencies(context.Background(), []string{"workmux", "close", "topic"}, deps)
	if err == nil || !strings.Contains(err.Error(), "test Git unavailable") || !runner.called {
		t.Fatalf("error = %v, Git called = %v; want workmux dispatch without database resolution", err, runner.called)
	}
}

func TestWorkmuxDependencyPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "other-config"))
	t.Setenv("XDG_STATE_HOME", "")
	var out bytes.Buffer
	app, err := newWorkmuxApp(nil, &out, &out)
	if err != nil {
		t.Fatal(err)
	}
	if app.ConfigDir != filepath.Join(home, ".config", "cli", "workmux") || app.StateDir != filepath.Join(home, ".local", "state", "cli", "workmux") {
		t.Fatalf("config = %s, state = %s", app.ConfigDir, app.StateDir)
	}
	container, ok := app.Sandbox.(*workmux.Containers)
	if !ok || container.StateDir != app.StateDir || container.HomeDir != home {
		t.Fatalf("sandbox paths do not match app: %#v", app.Sandbox)
	}
	if _, ok := app.Spawner.(workmux.ExecCleanupSpawner); !ok {
		t.Fatalf("detached cleanup spawner is not wired: %#v", app.Spawner)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "custom-state"))
	app, err = newWorkmuxApp(nil, &out, &out)
	if err != nil || app.StateDir != filepath.Join(home, "custom-state", "cli", "workmux") {
		t.Fatalf("state = %s, error = %v", app.StateDir, err)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if _, err := newWorkmuxApp(nil, &out, &out); err == nil {
		t.Fatal("accepted relative state directory")
	}
}

func TestPrivateWorkmuxCleanupRejectsMalformedInputBeforeDependencies(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "relative")
	t.Setenv("CLI_WORKMUX_CLEANUP_PROTOCOL", "")
	id := strings.Repeat("a", 32)
	for _, args := range [][]string{
		{"workmux", "_cleanup"},
		{"workmux", "_cleanup", id, id},
		{"workmux", "_cleanup", id, id, "../escape"},
		{"workmux", "_cleanup", id, id, strings.ToUpper(id)},
		{"workmux", "_cleanup", id, id, id, "extra"},
		{"workmux", "_cleanup", id, id, id},
	} {
		deps := dependencies{
			newWorkmux: func() (workmux.App, error) {
				t.Fatal("private dispatch used public dependency resolution")
				return workmux.App{}, nil
			},
			newCleanup: func(workmux.CleanupPaths) workmux.App {
				t.Fatal("malformed or unlaunched private invocation constructed dependencies")
				return workmux.App{}
			},
		}
		if err := runWithDependencies(context.Background(), args, deps); err == nil {
			t.Fatalf("accepted malformed private command %q", args)
		}
	}
	if strings.Contains(workmux.Usage, "_cleanup") {
		t.Fatal("private worker dispatch leaked into public help")
	}
}

type mainCleanupMux struct{}

func (mainCleanupMux) Session(context.Context) (string, error)                 { return "$0", nil }
func (mainCleanupMux) Server(context.Context) (string, error)                  { return "/test/tmux.sock", nil }
func (mainCleanupMux) Find(context.Context, workmux.Workspace) (string, error) { return "", nil }
func (mainCleanupMux) Create(context.Context, string, workmux.Workspace, []workmux.Pane, [][]string) (string, error) {
	return "@1", nil
}
func (mainCleanupMux) Focus(context.Context, workmux.Workspace, string) error { return nil }
func (mainCleanupMux) Close(context.Context, workmux.Workspace) error         { return nil }
func (mainCleanupMux) Capture(_ context.Context, _ workmux.Workspace, token string) (workmux.CleanupWindow, error) {
	return workmux.CleanupWindow{ID: "@1", Token: token, Socket: "/test/tmux.sock", Caller: true}, nil
}
func (mainCleanupMux) CloseCaptured(context.Context, workmux.Workspace, workmux.CleanupWindow) error {
	return nil
}
func (mainCleanupMux) CapturedExists(context.Context, workmux.Workspace, workmux.CleanupWindow) (bool, error) {
	return false, nil
}

type mainCleanupSpawn func(context.Context, workmux.CleanupLaunch) error

func (spawn mainCleanupSpawn) Start(ctx context.Context, launch workmux.CleanupLaunch) error {
	return spawn(ctx, launch)
}

func mainCleanupGit(t *testing.T, root string, args ...string) {
	t.Helper()
	options := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "commit.gpgSign=false",
		"-c", "user.name=Workmux Test", "-c", "user.email=workmux@example.invalid"}
	cmd := exec.Command("git", append(options, args...)...)
	cmd.Dir = root
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("temporary Git fixture: %v\n%s", err, out)
	}
}

func TestPrivateWorkmuxDispatchUsesCapturedPaths(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	home := filepath.Join(base, "home")
	for _, path := range []string{root, home} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	mainCleanupGit(t, root, "init", "--initial-branch=main")
	mainCleanupGit(t, root, "commit", "--allow-empty", "-m", "initial")
	paths := workmux.CleanupPaths{HomeDir: home, StateDir: filepath.Join(base, "state"), ConfigDir: filepath.Join(base, "config")}
	app := workmuxApp(paths, nil, io.Discard, io.Discard)
	app.Getwd = func() (string, error) { return root, nil }
	app.Mux = mainCleanupMux{}
	var command workmux.CleanupCommand
	app.Spawner = mainCleanupSpawn(func(_ context.Context, launch workmux.CleanupLaunch) error {
		command = launch.Command
		return nil
	})
	if err := app.Run(context.Background(), workmux.Command{Kind: "add", Name: "topic", Background: true}); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), workmux.Command{Kind: "remove", Name: "topic", KeepBranch: true}); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(paths.StateDir, command.RepoID, command.ID+".json")
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&state); err != nil {
		t.Fatal(err)
	}
	job := state["removal"].(map[string]any)["job"].(map[string]any)
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(statePath), "lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	writeStatus := func(status string) {
		t.Helper()
		job["status"] = status
		data, err := json.Marshal(state)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(statePath+".next", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(statePath+".next", statePath); err != nil {
			t.Fatal(err)
		}
	}
	writeStatus("queued")
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readyRead.Close()
	defer readyWrite.Close()
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer inputRead.Close()
	defer inputWrite.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestPrivateWorkmuxDispatchProcess$")
	cmd.Dir = root
	expectedPaths, err := json.Marshal(paths)
	if err != nil {
		t.Fatal(err)
	}
	expectedCommand, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	cmd.Env = append(os.Environ(), "CLI_TEST_PRIVATE_DISPATCH=1", "CLI_WORKMUX_CLEANUP_PROTOCOL=1",
		"CLI_TEST_CLEANUP_PATHS="+string(expectedPaths), "CLI_TEST_CLEANUP_COMMAND="+string(expectedCommand),
		"HOME=", "XDG_STATE_HOME=relative", "XDG_DATA_HOME=relative", "XDG_CONFIG_HOME=relative")
	cmd.ExtraFiles = []*os.File{readyWrite, inputRead}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	readyWrite.Close()
	inputRead.Close()
	if err := json.NewEncoder(inputWrite).Encode(paths); err != nil {
		t.Fatal(err)
	}
	inputWrite.Close()
	if err := readyRead.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(readyRead).ReadString('\n')
	if err != nil || line != "ready "+command.Token+"\n" {
		t.Fatalf("production dispatch did not acknowledge the protocol: %q, %v", line, err)
	}
	writeStatus("armed")
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("successful private dispatch failed with invalid ambient paths: %v\n%s", err, output.String())
	}
	data, err = os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var completed workmux.Workspace
	if err := json.Unmarshal(data, &completed); err != nil || completed.Stage != "removed" {
		t.Fatalf("private dispatch did not finish cleanup: %+v, %v", completed, err)
	}
	if _, err := os.Stat(completed.Path); !os.IsNotExist(err) {
		t.Fatalf("private dispatch left the temporary worktree: %v", err)
	}
}

func TestPrivateWorkmuxDispatchProcess(t *testing.T) {
	if os.Getenv("CLI_TEST_PRIVATE_DISPATCH") != "1" {
		t.Skip("private-dispatch subprocess")
	}
	var expectedPaths workmux.CleanupPaths
	var command workmux.CleanupCommand
	if err := json.Unmarshal([]byte(os.Getenv("CLI_TEST_CLEANUP_PATHS")), &expectedPaths); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(os.Getenv("CLI_TEST_CLEANUP_COMMAND")), &command); err != nil {
		t.Fatal(err)
	}
	called := false
	deps := dependencies{
		newWorkmux: func() (workmux.App, error) {
			t.Fatal("private dispatch resolved public HOME/XDG dependencies")
			return workmux.App{}, nil
		},
		newCleanup: func(paths workmux.CleanupPaths) workmux.App {
			called = true
			if paths != expectedPaths || os.Getenv("HOME") != "" || os.Getenv("XDG_STATE_HOME") != "relative" || os.Getenv("XDG_DATA_HOME") != "relative" {
				t.Fatalf("private dispatch did not use captured paths: %+v", paths)
			}
			app := workmuxApp(paths, nil, io.Discard, io.Discard)
			container := app.Sandbox.(*workmux.Containers)
			if app.HomeDir != paths.HomeDir || app.StateDir != paths.StateDir || app.ConfigDir != paths.ConfigDir || container.HomeDir != paths.HomeDir || container.StateDir != paths.StateDir {
				t.Fatal("private factory wiring did not preserve captured paths")
			}
			app.Mux = mainCleanupMux{}
			return app
		},
	}
	args := []string{"workmux", "_cleanup", command.RepoID, command.ID, command.Token}
	if err := runWithDependencies(context.Background(), args, deps); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("private dispatch did not construct its captured-path dependencies")
	}
}
