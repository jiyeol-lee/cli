package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jiyeol-lee/cli/internal/workmux"
)

func TestPaneSignalRunsDeferredCleanup(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal os.Signal
		code   int
	}{
		{name: "hangup", signal: syscall.SIGHUP, code: 129},
		{name: "interrupt", signal: os.Interrupt, code: 130},
		{name: "terminate", signal: syscall.SIGTERM, code: 143},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			manual := startPaneSignalProcess(t, filepath.Join(dir, "manual"), "")
			pane := startPaneSignalProcess(t, filepath.Join(dir, "pane"), test.name)
			if err := pane.cmd.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := pane.cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != test.code {
				t.Fatalf("pane exit = %v, want code %d\n%s", err, test.code, &pane.output)
			}
			if data, err := os.ReadFile(pane.cleanup); err != nil || string(data) != "cleaned\n" {
				t.Fatalf("pane deferred cleanup = %q, %v", data, err)
			}
			if _, err := os.Stat(manual.cleanup); !os.IsNotExist(err) {
				t.Fatalf("pane signal triggered manual cleanup: %v", err)
			}
			if _, err := io.WriteString(manual.input, "ping\n"); err != nil {
				t.Fatal(err)
			}
			manual.expect(t, "alive\n")
			if _, err := io.WriteString(manual.input, "exit\n"); err != nil {
				t.Fatal(err)
			}
			if err := manual.cmd.Wait(); err != nil {
				t.Fatalf("manual run = %v\n%s", err, &manual.output)
			}
			if data, err := os.ReadFile(manual.cleanup); err != nil || string(data) != "cleaned\n" {
				t.Fatalf("manual deferred cleanup = %q, %v", data, err)
			}
		})
	}
}

type paneSignalProcess struct {
	cmd     *exec.Cmd
	output  bytes.Buffer
	ready   *os.File
	reader  *bufio.Reader
	input   *os.File
	cleanup string
}

func startPaneSignalProcess(t *testing.T, cleanup, signal string) *paneSignalProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readyRead.Close() })
	defer readyWrite.Close()
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inputWrite.Close() })
	defer inputRead.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	p := &paneSignalProcess{ready: readyRead, reader: bufio.NewReader(readyRead), input: inputWrite, cleanup: cleanup}
	p.cmd = exec.CommandContext(ctx, executable, "-test.run=^TestPaneSignalProcess$")
	p.cmd.Env = append(os.Environ(), "CLI_TEST_PANE_SIGNAL_CLEANUP="+cleanup, "CLI_TEST_PANE_SIGNAL="+signal)
	p.cmd.ExtraFiles = []*os.File{readyWrite, inputRead}
	p.cmd.Stdout, p.cmd.Stderr = &p.output, &p.output
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			_ = p.cmd.Wait()
		}
	})
	if err := readyWrite.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inputRead.Close(); err != nil {
		t.Fatal(err)
	}
	p.expect(t, "ready\n")
	return p
}

func (p *paneSignalProcess) expect(t *testing.T, want string) {
	t.Helper()
	if err := p.ready.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	line, err := p.reader.ReadString('\n')
	if err != nil || line != want {
		t.Fatalf("subprocess response = %q, %v, want %q", line, err, want)
	}
}

func TestPaneSignalProcess(t *testing.T) {
	cleanup := os.Getenv("CLI_TEST_PANE_SIGNAL_CLEANUP")
	if cleanup == "" {
		t.Skip("pane-signal subprocess")
	}
	ctx, stop := commandSignalContext()
	defer stop()
	ready := os.NewFile(3, "ready")
	defer ready.Close()
	input := os.NewFile(4, "input")
	defer input.Close()
	commands := make(chan string)
	go func() {
		defer close(commands)
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			select {
			case commands <- scanner.Text():
			case <-ctx.Done():
				return
			}
		}
	}()
	err := func() (result error) {
		defer func() {
			result = errors.Join(result, os.WriteFile(cleanup, []byte("cleaned\n"), 0600))
		}()
		if _, err := io.WriteString(ready, "ready\n"); err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			case command, ok := <-commands:
				if !ok {
					return fmt.Errorf("subprocess control pipe closed")
				}
				switch command {
				case "ping":
					if ctx.Err() != nil {
						return context.Cause(ctx)
					}
					if _, err := io.WriteString(ready, "alive\n"); err != nil {
						return err
					}
				case "exit":
					return nil
				default:
					return fmt.Errorf("unexpected subprocess command %q", command)
				}
			}
		}
	}()
	want := map[string]error{"hangup": workmux.ErrHangup, "interrupt": workmux.ErrInterrupt, "terminate": workmux.ErrTerminate}[os.Getenv("CLI_TEST_PANE_SIGNAL")]
	if want == nil {
		if err != nil || ctx.Err() != nil {
			t.Fatalf("manual run was canceled: %v, %v", err, context.Cause(ctx))
		}
		return
	}
	if !errors.Is(err, want) || !errors.Is(err, context.Canceled) {
		t.Fatalf("signal cause = %v, want %v", err, want)
	}
	os.Exit(commandExitCode([]string{"workmux", "sandbox", "run"}, err))
}

func TestTmuxWindowCloseRunsDeferredCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("isolated tmux integration test")
	}
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	if _, err := os.Stat("/bin/bash"); err != nil {
		t.Skip("Bash is not installed")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	socket := filepath.Join(dir, "tmux.sock")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	runTmux := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, tmux, append([]string{"-S", socket, "-f", "/dev/null"}, args...)...)
		cmd.Env = append(os.Environ(), "TMUX=", "TMUX_PANE=", "HOME="+dir, "SHELL=/bin/bash")
		return cmd.CombinedOutput()
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = runTmux(cleanup, "kill-server")
	})
	startWindow := func(name string, first bool) (string, int) {
		t.Helper()
		prefix := filepath.Join(dir, name)
		args := []string{"new-window", "-d", "-t", "pane-signal-test:", "-n", name, "-P", "-F", "#{window_id}\t#{pane_pid}"}
		if first {
			args = []string{"new-session", "-d", "-s", "pane-signal-test", "-n", name, "-P", "-F", "#{window_id}\t#{pane_pid}"}
		}
		// Keep Bash waiting on a foreground job rather than replacing it with the helper.
		command := `"$1" -test.run=^TestTmuxPaneSignalProcess$; status=$?; exit "$status"`
		args = append(args, "--", "/usr/bin/env", "CLI_TEST_TMUX_SIGNAL_PREFIX="+prefix,
			"/bin/bash", "--noprofile", "--norc", "-i", "-c", command, "pane-signal-test", executable)
		out, err := runTmux(ctx, args...)
		if err != nil {
			t.Fatalf("start %s window: %v\n%s", name, err, out)
		}
		window, shell, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
		if !ok || !strings.HasPrefix(window, "@") {
			t.Fatalf("window identity = %q", out)
		}
		shellPID, err := strconv.Atoi(shell)
		if err != nil {
			t.Fatal(err)
		}
		ready := waitTmuxSignalFile(t, ctx, prefix+".ready", func(data string) bool {
			return len(strings.Fields(data)) == 2 && strings.HasSuffix(data, "\n")
		})
		var pid, parent int
		if _, err := fmt.Sscanf(ready, "%d %d", &pid, &parent); err != nil || pid == shellPID || parent != shellPID {
			t.Fatalf("helper identity = %q, shell PID = %d, error = %v; want a Bash foreground child", ready, shellPID, err)
		}
		return window, pid
	}
	manualWindow, manualPID := startWindow("manual", true)
	paneWindow, _ := startWindow("pane", false)
	if out, err := runTmux(ctx, "kill-window", "-t", paneWindow); err != nil {
		t.Fatalf("close pane window: %v\n%s", err, out)
	}
	want := workmux.ErrHangup.Error() + "\n129\n"
	waitTmuxSignalFile(t, ctx, filepath.Join(dir, "pane.cleanup"), func(data string) bool { return data == want })
	if _, err := os.Stat(filepath.Join(dir, "manual.cleanup")); !os.IsNotExist(err) {
		t.Fatalf("closing pane window triggered manual cleanup: %v", err)
	}
	if err := syscall.Kill(manualPID, 0); err != nil {
		t.Fatalf("manual helper is no longer running: %v", err)
	}
	if out, err := runTmux(ctx, "list-panes", "-t", manualWindow, "-F", "#{pane_dead}"); err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Fatalf("manual window is no longer live: %q, %v", out, err)
	}
	if out, err := runTmux(ctx, "kill-window", "-t", manualWindow); err != nil {
		t.Fatalf("close manual window: %v\n%s", err, out)
	}
	waitTmuxSignalFile(t, ctx, filepath.Join(dir, "manual.cleanup"), func(data string) bool { return data == want })
}

func waitTmuxSignalFile(t *testing.T, ctx context.Context, path string, matches func(string) bool) string {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && matches(string(data)) {
			return string(data)
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read subprocess marker %s: %v", path, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for subprocess marker %s: %q, %v, %v", path, data, err, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestTmuxPaneSignalProcess(t *testing.T) {
	prefix := os.Getenv("CLI_TEST_TMUX_SIGNAL_PREFIX")
	if prefix == "" {
		t.Skip("tmux pane-signal subprocess")
	}
	ctx, stop := commandSignalContext()
	defer stop()
	args := []string{"workmux", "sandbox", "run"}
	err := func() (result error) {
		defer func() {
			cause := context.Cause(ctx)
			marker := fmt.Sprintf("%v\n%d\n", cause, commandExitCode(args, cause))
			result = errors.Join(result, os.WriteFile(prefix+".cleanup", []byte(marker), 0600))
		}()
		ready := fmt.Sprintf("%d %d\n", os.Getpid(), os.Getppid())
		if err := os.WriteFile(prefix+".ready", []byte(ready), 0600); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-time.After(25 * time.Second):
			return fmt.Errorf("tmux pane did not receive hangup")
		}
	}()
	if !errors.Is(err, workmux.ErrHangup) {
		t.Fatalf("window-close cause = %v, want %v", err, workmux.ErrHangup)
	}
	os.Exit(commandExitCode(args, err))
}
