package workmux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestExecRunnerCancellationWithInheritedPipes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	for _, group := range []bool{false, true} {
		t.Run(strconv.FormatBool(group), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(t.TempDir(), "child")
			done := make(chan error, 1)
			go func() {
				_, err := (ExecRunner{}).Run(ctx, Process{
					Name: "bash", Args: []string{"-c", `sleep 30 & child=$!; printf '%s' "$child" > "$1"; wait "$child"`, "--", path},
					Dir: "/", ProcessGroup: group,
				})
				done <- err
			}()
			var pid int
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				data, err := os.ReadFile(path)
				if err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
					if pid > 0 {
						break
					}
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("hook did not start its child")
			}
			// The non-group case intentionally leaves a child holding the output pipes.
			t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation error = %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation waited for an inherited output pipe")
			}
			if group {
				// Linux may retain a killed child as a zombie until its adopter reaps it.
				if err := syscall.Kill(pid, 0); err == nil {
					status, readErr := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
					_, rest, ok := strings.Cut(string(status), ") ")
					if readErr == nil && ok && !strings.HasPrefix(rest, "Z ") {
						t.Fatal("host hook child remained running after group cancellation")
					}
				}
			}
		})
	}
}

func TestExecRunnerTerminalInput(t *testing.T) {
	if directory := os.Getenv("WORKMUX_TEST_TERMINAL_INPUT"); directory != "" {
		w := tmuxTestWorkspace()
		w.Path = directory
		var stdout bytes.Buffer
		app := App{Runner: ExecRunner{}, Stdin: os.Stdin, Stdout: &stdout, Stderr: os.Stderr}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		command := `printf ready > "$WM_WORKTREE_PATH/ready"; read -r answer; printf '%s' "$answer"`
		if err := app.hooks(ctx, w, "pre_remove", []string{command}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "answer"), stdout.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	ctx, runner, _, session := isolatedTmux(t)
	directory := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{
		"new-window", "-d", "-P", "-F", "#{pane_id}", "-t", session + ":", "--",
		"env", "WORKMUX_TEST_TERMINAL_INPUT=" + directory, executable, "-test.run=^TestExecRunnerTerminalInput$",
	}})
	if err != nil {
		t.Fatal(err)
	}
	pane := strings.TrimSpace(string(out))
	if !tmuxID(pane, '%') {
		t.Fatalf("invalid terminal helper pane %q", pane)
	}
	waitHostFile(t, ctx, filepath.Join(directory, "ready"))
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", pane, "terminal-input-ok", "Enter"}}); err != nil {
		t.Fatal(err)
	}
	waitHostFile(t, ctx, filepath.Join(directory, "answer"))
	answer, err := os.ReadFile(filepath.Join(directory, "answer"))
	if err != nil || string(answer) != "terminal-input-ok" {
		t.Fatalf("host hook terminal read = %q, %v", answer, err)
	}
}
