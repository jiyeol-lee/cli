package workmux

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"testing"
)

func TestHostHooksPassCommandAndEnvironmentWithoutInterpolation(t *testing.T) {
	var events []string
	runner := &hostTestRunner{events: &events}
	stdin := bytes.NewBufferString("input")
	var stdout, stderr bytes.Buffer
	w := tmuxTestWorkspace()
	w.Branch, w.MergeTarget = "feature/topic", "release"
	app := App{Runner: runner, Stdin: stdin, Stdout: &stdout, Stderr: &stderr, ConfigDir: "/external/workmux"}
	command := "printf '%s' \"$WM_BRANCH_NAME\"; echo 'literal $value'"
	if err := app.hooks(context.Background(), w, "pre_merge", []string{command}); err != nil {
		t.Fatal(err)
	}
	if len(runner.processes) != 1 {
		t.Fatalf("process count = %d", len(runner.processes))
	}
	p := runner.processes[0]
	if p.Name != "bash" || !reflect.DeepEqual(p.Args, []string{"-c", command}) || p.Dir != w.Path || !p.CleanGitEnv || !p.ProcessGroup {
		t.Fatalf("wrong hook process: %+v", p)
	}
	if p.Stdin != stdin || p.Stdout != &stdout || p.Stderr != &stderr {
		t.Fatal("hook did not use injected I/O")
	}
	for _, value := range []string{"WM_BRANCH_NAME=feature/topic", "WM_PROJECT_ROOT=" + w.Root, "WM_WORKTREE_PATH=" + w.Path, "WM_TARGET_BRANCH=release", "WM_CONFIG_DIR=/external/workmux", "WM_HANDLE=" + w.Handle, "WORKMUX_HANDLE=" + w.Handle} {
		if !slices.Contains(p.Env, value) {
			t.Fatalf("missing hook env %s", value)
		}
	}
}

func TestHostHooksStopAtFirstFailure(t *testing.T) {
	var events []string
	runner := &hostTestRunner{events: &events, hook: func(Process) error { return fmt.Errorf("failed") }}
	app := App{Runner: runner}
	if err := app.hooks(context.Background(), tmuxTestWorkspace(), "pre_remove", []string{"first", "second"}); err == nil {
		t.Fatal("expected hook error")
	}
	if !reflect.DeepEqual(events, []string{"hook:first"}) {
		t.Fatalf("ran after failing hook: %q", events)
	}
}

func TestHostSandboxNeverFallsBackForHooksOrPanes(t *testing.T) {
	w := tmuxTestWorkspace()
	w.Config.Sandbox.Enabled = true
	app := App{}
	if err := app.hooks(context.Background(), w, "pre_merge", []string{"true"}); err == nil {
		t.Fatal("missing sandbox did not block hooks")
	}
	if _, err := app.paneCommands(w, []Pane{{Command: "true"}}); err == nil {
		t.Fatal("missing sandbox did not block panes")
	}
	w.Config.Sandbox.Enabled = false
	commands, err := app.paneCommands(w, []Pane{{}, {Command: "echo ok"}})
	if err != nil || !reflect.DeepEqual(commands, [][]string{nil, {"bash", "-c", "echo ok"}}) {
		t.Fatalf("host panes = %q, %v", commands, err)
	}
}

func TestHostHooksUseBashAndUpstreamEnvironment(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	w.Branch, w.MergeTarget = "feature/topic", "main"
	var stdout bytes.Buffer
	app := App{Runner: ExecRunner{}, Stdout: &stdout, ConfigDir: "/external/workmux"}
	command := `[[ "$WM_BRANCH_NAME" == feature/topic ]] && [[ "$WM_TARGET_BRANCH" == main ]] && printf '%s' "$WM_CONFIG_DIR"`
	if err := app.hooks(context.Background(), w, "pre_merge", []string{command}); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != app.ConfigDir {
		t.Fatalf("hook output = %q", stdout.String())
	}
}
