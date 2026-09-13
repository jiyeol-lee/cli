package workmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "workmux" {
		if os.Args[2] == "_cleanup" {
			command, err := ParseCleanupCommand(os.Args[2:])
			if err != nil {
				os.Exit(2)
			}
			if path := os.Getenv("WORKMUX_TEST_WORKER_PID"); path != "" {
				if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
					os.Exit(2)
				}
			}
			switch os.Getenv("WORKMUX_TEST_WORKER_MODE") {
			case "bad-ready":
				file := os.NewFile(3, "ready")
				_, writeErr := file.WriteString("wrong acknowledgement\n")
				if err := errors.Join(writeErr, file.Close()); err != nil {
					os.Exit(2)
				}
				os.Exit(0)
			case "no-ready":
				time.Sleep(30 * time.Second)
				os.Exit(1)
			}
			err = RunCleanupProcess(context.Background(), command, func(paths CleanupPaths) App {
				app := cleanupProcessApp(paths)
				app.Runner = cleanupProofRunner{command: command, paths: paths}
				return app
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		paths := CleanupPaths{HomeDir: os.Getenv("WORKMUX_TEST_HOME"), StateDir: os.Getenv("WORKMUX_TEST_STATE"), ConfigDir: os.Getenv("WORKMUX_TEST_CONFIG")}
		command, err := ParseCommand(os.Args[2:])
		if err == nil {
			if gate := os.Getenv("WORKMUX_TEST_GATE"); gate != "" {
				deadline := time.Now().Add(15 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(gate); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			app := cleanupProcessApp(paths)
			app.Stdin, app.Stdout, app.Stderr = os.Stdin, os.Stdout, os.Stderr
			var outputFile *os.File
			if output := os.Getenv("WORKMUX_TEST_OUTPUT"); output != "" {
				file, openErr := os.OpenFile(output, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
				if openErr != nil {
					os.Exit(1)
				}
				app.Stdout = file
				outputFile = file
			}
			err = app.Run(context.Background(), command)
			if outputFile != nil {
				err = errors.Join(err, outputFile.Close())
			}
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if os.Getenv("WORKMUX_TEST_HOLD_PARENT") == "1" {
			time.Sleep(30 * time.Second)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func cleanupProcessApp(paths CleanupPaths) App {
	runner := ExecRunner{}
	return App{Runner: runner, Mux: Tmux{Runner: runner}, Spawner: ExecCleanupSpawner{},
		Sandbox: &Containers{Runner: runner, HomeDir: paths.HomeDir, StateDir: paths.StateDir},
		HomeDir: paths.HomeDir, StateDir: paths.StateDir, ConfigDir: paths.ConfigDir}
}

func freshTmuxWorkspace(w Workspace) Workspace {
	w.Window, w.Socket = "", ""
	w.ServerPID, w.SocketDevice, w.SocketInode = 0, 0, 0
	return w
}

type cleanupProof struct {
	PID                   int
	WindowGone            bool
	SessionLeader         bool
	NoControllingTerminal bool
	NullInputOutput       bool
	SurvivingCwd          bool
}

type cleanupProofRunner struct {
	command CleanupCommand
	paths   CleanupPaths
}

func (runner cleanupProofRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	args := hostGitArgs(p)
	if p.Name == "git" && len(args) > 1 && args[0] == "worktree" && args[1] == "remove" {
		path := filepath.Join(runner.paths.StateDir, runner.command.RepoID, runner.command.ID+".json")
		state, err := readState(path)
		if err != nil {
			return nil, err
		}
		present, err := (Tmux{Runner: ExecRunner{}}).CapturedExists(ctx, state.Workspace, state.Removal.Job.Window)
		if err != nil || present {
			return nil, fmt.Errorf("worker attempted deletion with source window present")
		}
		proof := cleanupProof{PID: os.Getpid(), WindowGone: true, SessionLeader: syscall.Getpgrp() == os.Getpid()}
		if tty, err := os.Open("/dev/tty"); err != nil {
			proof.NoControllingTerminal = true
		} else {
			_ = tty.Close()
		}
		zero, err0 := os.Stdin.Stat()
		one, err1 := os.Stdout.Stat()
		null, err2 := os.Stat(os.DevNull)
		proof.NullInputOutput = err0 == nil && err1 == nil && err2 == nil && os.SameFile(zero, null) && os.SameFile(one, null)
		cwd, err := os.Getwd()
		proof.SurvivingCwd = err == nil && cwd == state.Root && p.Dir == state.Root
		if output := os.Getenv("WORKMUX_TEST_PROOF"); output != "" {
			data, err := json.Marshal(proof)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(output, data, 0600); err != nil {
				return nil, err
			}
		}
	}
	return (ExecRunner{}).Run(ctx, p)
}

type cleanupSpawnFunc func(context.Context, CleanupLaunch) error

func (spawn cleanupSpawnFunc) Start(ctx context.Context, launch CleanupLaunch) error {
	return spawn(ctx, launch)
}

type cleanupReadyFunc func([]byte) (int, error)

func (ready cleanupReadyFunc) Write(data []byte) (int, error) { return ready(data) }

type cleanupReadyCloser struct {
	cleanupReadyFunc
	err error
}

func (ready cleanupReadyCloser) Close() error { return ready.err }

type cleanupRunFunc func(context.Context, Process) ([]byte, error)

func (run cleanupRunFunc) Run(ctx context.Context, p Process) ([]byte, error) {
	return run(ctx, p)
}

func queuedCleanup(t *testing.T, f *hostFixture) (*stateStore, workspaceState, CleanupCommand) {
	t.Helper()
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	state := f.load(t, "topic")
	source, err := (gitHost{runner: f.runner}).source(context.Background(), store.repo, state.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := captureCleanupIdentity(store.repo, source)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("c", 32)
	window, err := f.mux.Capture(context.Background(), state.Workspace, token)
	if err != nil {
		t.Fatal(err)
	}
	window.Caller = true
	state.Stage = "removing"
	state.Removal = &removalState{Head: source.Head, KeepBranch: true, HookDone: true, Identity: saved,
		Job: &cleanupJob{Token: token, Status: "queued", Deadline: time.Now().Add(cleanupLease).UnixNano(), Window: window}}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	return store, state, CleanupCommand{RepoID: state.RepoID, ID: state.ID, Token: token}
}

func armCleanup(t *testing.T, store *stateStore, state *workspaceState) {
	t.Helper()
	state.Removal.Job.Status = "armed"
	if err := store.save(*state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupExternalClosesAndPollsBeforeDeletion(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	checks := 0
	f.mux.onCapturedClose = func(w Workspace, window CleanupWindow) error {
		*f.mux.events = append(*f.mux.events, "mux:close")
		state := f.load(t, w.Branch)
		if !state.Removal.HookDone || state.Removal.WorktreeRemoved {
			t.Fatal("window close was not ordered after hooks and before deletion")
		}
		return nil
	}
	f.mux.onExists = func(Workspace, CleanupWindow) (bool, error) {
		checks++
		return checks < 3, nil
	}
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 1 && args[0] == "worktree" && args[1] == "remove" && checks < 3 {
			t.Fatal("worktree deletion preceded window disappearance")
		}
		return nil
	}
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal(err)
	}
	hostEventBefore(t, f.events, "hook:check", "mux:close")
	hostEventBefore(t, f.events, "mux:close", "git:worktree remove")
}

func TestCleanupWindowTimeoutAndCloseFailureAreRetriable(t *testing.T) {
	for _, failure := range []string{"timeout", "close"} {
		t.Run(failure, func(t *testing.T) {
			f := newHostFixture(t, "")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			f.app.cleanupTimeout = 50 * time.Millisecond
			f.mux.onCapturedClose = func(Workspace, CleanupWindow) error {
				if failure == "close" {
					return fmt.Errorf("SECRET engine stderr must not enter diagnostics")
				}
				return nil
			}
			if err := f.run(t, "remove", "topic", "--force"); err == nil {
				t.Fatal("expected shutdown failure")
			}
			state := f.load(t, "topic")
			oldToken := state.Removal.Job.Token
			if state.Stage == "removed" || state.Removal.WorktreeRemovalStarted || state.Removal.Job.Status != "failed" {
				t.Fatalf("premature removal/checkpoint: %+v", state.Removal)
			}
			data, err := os.ReadFile(filepath.Join(f.state, state.RepoID, state.ID+"."+oldToken+".cleanup.log"))
			if err != nil || bytes.Contains(data, []byte("SECRET")) {
				t.Fatalf("unsafe diagnostic output: %s, %v", data, err)
			}
			f.mux.onCapturedClose = nil
			if err := f.run(t, "remove", "topic"); err != nil {
				t.Fatal("shutdown failure could not be retried", err)
			}
			if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal.Job.Token == oldToken {
				t.Fatal("retry did not replace attempt token and complete")
			}
		})
	}
}

func TestCleanupSpawnFailureDoesNotCloseCaller(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, launch CleanupLaunch) error {
		state := f.load(t, "topic")
		if state.Removal.Job.Status != "queued" || state.Removal.Job.Token != launch.Command.Token || !state.Removal.HookDone {
			t.Fatal("spawn preceded its durable handoff")
		}
		return fmt.Errorf("cannot spawn")
	})
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("expected spawn failure")
	}
	if f.mux.window == "" || slices.Contains(f.events, "git:worktree remove") {
		t.Fatal("failed handoff closed the caller or removed data")
	}
	if state := f.load(t, "topic"); state.Removal.Job.Status != "failed" {
		t.Fatal("failed handoff was not retryable")
	}
}

func TestCleanupDiagnosticErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "diagnostic")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, path string
		flag       int
		want       error
	}{
		{"write", path, os.O_RDONLY, syscall.EBADF},
		{"sync", os.DevNull, os.O_WRONLY, syscall.EINVAL},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.OpenFile(test.path, test.flag, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := file.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := cleanupDiagnostic(file, "queued"); !errors.Is(err, test.want) || !strings.Contains(err.Error(), test.name+" cleanup diagnostic") {
				t.Fatalf("diagnostic error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestCleanupDiagnosticFailureDoesNotCancelScheduledJob(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, launch CleanupLaunch) error {
		if err := launch.Stderr.Close(); err != nil {
			t.Fatal(err)
		}
		return nil
	})
	if err := f.run(t, "remove", "topic", "--keep-branch"); !errors.Is(err, os.ErrClosed) || !strings.Contains(err.Error(), "write cleanup diagnostic") {
		t.Fatalf("lost diagnostic failure: %v", err)
	}
	state := f.load(t, "topic")
	if state.Removal.Job.Status != "armed" || state.Removal.WorktreeRemoved || f.mux.window == "" || !strings.Contains(f.stdout.String(), "cleanup scheduled") {
		t.Fatalf("diagnostic failure changed scheduled work: %+v, %s", state, f.stdout.String())
	}
	hostStateStore(t, f)
}

func TestCleanupSpawnFailurePreservesSecondaryErrors(t *testing.T) {
	for _, failure := range []string{"diagnostic", "checkpoint"} {
		t.Run(failure, func(t *testing.T) {
			f := newHostFixture(t, "")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			want := fmt.Errorf("spawn failed")
			f.mux.caller = true
			f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, launch CleanupLaunch) error {
				if failure == "diagnostic" {
					if err := launch.Stderr.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					lock := filepath.Join(f.state, launch.Command.RepoID, "lock")
					if err := os.Rename(lock, lock+"-old"); err != nil {
						t.Fatal(err)
					}
				}
				return want
			})
			err := f.run(t, "remove", "topic", "--keep-branch")
			if !errors.Is(err, want) || !strings.Contains(err.Error(), "cleanup "+failure) {
				t.Fatalf("lost primary or secondary failure: %v", err)
			}
			if failure == "diagnostic" && !errors.Is(err, os.ErrClosed) {
				t.Fatalf("lost diagnostic cause: %v", err)
			}
			if state := f.load(t, "topic"); state.Removal.WorktreeRemoved || f.mux.window == "" {
				t.Fatal("failed spawn removed resources")
			}
		})
	}
}

func TestCleanupConcreteSpawnerRejectsBadOrMissingHandshake(t *testing.T) {
	for _, mode := range []string{"bad-ready", "no-ready"} {
		t.Run(mode, func(t *testing.T) {
			f := newHostFixture(t, "")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			pidPath := filepath.Join(f.home, "worker-pid")
			t.Setenv("WORKMUX_TEST_WORKER_MODE", mode)
			t.Setenv("WORKMUX_TEST_WORKER_PID", pidPath)
			f.mux.caller = true
			f.app.Spawner = ExecCleanupSpawner{}
			ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
			defer cancel()
			err := f.app.Run(ctx, Command{Kind: "remove", Name: "topic", KeepBranch: true})
			if err == nil || !strings.Contains(err.Error(), "handshake failed") {
				t.Fatalf("invalid worker handshake succeeded: %v", err)
			}
			pidData, err := os.ReadFile(pidPath)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(pidData))
			if err != nil || syscall.Kill(pid, 0) != syscall.ESRCH {
				t.Fatalf("failed helper was not killed and reaped: %s, %v", pidData, err)
			}
			if state := f.load(t, "topic"); state.Removal.Job.Status != "failed" || state.Removal.WorktreeRemoved || f.mux.window == "" {
				t.Fatalf("failed handshake changed resources: %+v", state)
			}
		})
	}
}

func TestCleanupWorkerReadinessPrecedesLockHandoff(t *testing.T) {
	f := newHostFixture(t, "")
	store, state, command := queuedCleanup(t, f)
	ready := make(chan string, 1)
	closed := make(chan struct{}, 1)
	f.mux.onCapturedClose = func(w Workspace, window CleanupWindow) error {
		closed <- struct{}{}
		f.mux.window = ""
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- f.app.RunCleanup(context.Background(), command, cleanupReadyFunc(func(data []byte) (int, error) {
			ready <- string(data)
			return len(data), nil
		}))
	}()
	select {
	case message := <-ready:
		if message != "ready "+command.Token+"\n" {
			t.Fatal(message)
		}
	case <-time.After(time.Second):
		t.Fatal("worker waited for the parent lock before acknowledging readiness")
	}
	select {
	case <-closed:
		t.Fatal("worker closed window before parent released lock")
	case <-time.After(50 * time.Millisecond):
	}
	armCleanup(t, store, &state)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" {
		t.Fatal("worker did not finish after lock handoff")
	}
}

func TestCleanupWorkerReadinessCloseFailureDoesNotCancelArmedJob(t *testing.T) {
	f := newHostFixture(t, "")
	store, state, command := queuedCleanup(t, f)
	want := fmt.Errorf("readiness close failed")
	ready := cleanupReadyCloser{
		cleanupReadyFunc: func(data []byte) (int, error) {
			armCleanup(t, store, &state)
			return len(data), nil
		},
		err: want,
	}
	if err := f.app.RunCleanup(context.Background(), command, ready); !errors.Is(err, want) {
		t.Fatalf("readiness close error = %v, want %v", err, want)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal.Job.Status != "complete" {
		t.Fatalf("readiness close failure cancelled armed cleanup: %+v", state)
	}
	hostStateStore(t, f)
}

func TestCleanupExecutionOutlivesHandoffLease(t *testing.T) {
	f := newHostFixture(t, "")
	store, state, command := queuedCleanup(t, f)
	state.Removal.Job.Deadline = time.Now().Add(time.Second).UnixNano()
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	expiry := time.Unix(0, state.Removal.Job.Deadline)
	waited := false
	base := f.app.Runner
	f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
		if !waited {
			waited = true
			if current := f.load(t, "topic"); current.Removal.Job.Status != "running" {
				t.Fatal("execution began before claiming the handoff under lock")
			}
			if err := cleanupPause(ctx, time.Until(expiry)+50*time.Millisecond); err != nil {
				return nil, fmt.Errorf("handoff deadline leaked into execution: %w", err)
			}
		}
		return base.Run(ctx, p)
	})
	if err := f.app.RunCleanup(context.Background(), command, cleanupReadyFunc(func(data []byte) (int, error) {
		armCleanup(t, store, &state)
		return len(data), nil
	})); err != nil {
		t.Fatal(err)
	}
	if !waited || time.Now().Before(expiry) || f.load(t, "topic").Stage != "removed" {
		t.Fatal("cleanup did not complete beyond the handoff lease")
	}
}

func TestCleanupSchedulingOutputPrecedesArming(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(context.Context, CleanupLaunch) error { return nil })
	f.app.Stdout = cleanupReadyFunc(func([]byte) (int, error) {
		if state := f.load(t, "topic"); state.Removal.Job.Status != "queued" {
			t.Fatal("cleanup was armed before scheduling output succeeded")
		}
		return 0, syscall.EPIPE
	})
	if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
		t.Fatal("ignored scheduling output failure")
	}
	if state := f.load(t, "topic"); state.Removal.Job.Status != "failed" || state.Removal.WorktreeRemoved || f.mux.window == "" {
		t.Fatalf("output failure changed resources: %+v", state)
	}
}

func TestCleanupSocketLoss(t *testing.T) {
	for _, kind := range []string{"rename", "unlink", "rebound"} {
		t.Run(kind, func(t *testing.T) {
			ctx, runner, mux, session := isolatedTmux(t)
			f := newHostFixture(t, "")
			store, state, command := queuedCleanup(t, f)
			state.Socket = ""
			window, err := mux.Create(ctx, session, freshTmuxWorkspace(state.Workspace), []Pane{{}}, [][]string{{"sleep", "60"}})
			if err != nil {
				t.Fatal(err)
			}
			state.Window, state.Socket = window, runner.socket
			captured, err := mux.Capture(ctx, state.Workspace, command.Token)
			if err != nil {
				t.Fatal(err)
			}
			state.Removal.Job.Window = captured
			if err := store.save(state); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(filepath.Dir(runner.socket), "old.sock")
			if kind == "unlink" {
				if err := os.Link(runner.socket, alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(runner.socket); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Rename(runner.socket, alias); err != nil {
				t.Fatal(err)
			}
			oldRunner := isolatedTmuxRunner{socket: alias, home: runner.home}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = runner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
				_, _ = oldRunner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
			})
			if kind == "rebound" {
				if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"new-session", "-d", "-s", "replacement", "--", "sleep", "60"}}); err != nil {
					t.Fatal(err)
				}
				out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", "replacement:", "--", "sleep", "60"}})
				if err != nil || strings.TrimSpace(string(out)) != captured.ID {
					t.Fatalf("replacement did not reuse the window ID: %s, %v", out, err)
				}
				for key, value := range map[string]string{"@cli_workmux_id": state.ID, "@cli_workmux_cleanup": captured.Token} {
					if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-w", "-t", captured.ID, key, value}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if exited, err := tmuxServerExited(captured.ServerPID); err != nil || exited {
				t.Fatalf("test lost original live tmux server: %v, %v", exited, err)
			}
			f.app.Mux = mux
			f.app.cleanupTimeout = 50 * time.Millisecond
			if err := f.app.RunCleanup(ctx, command, cleanupReadyFunc(func(data []byte) (int, error) {
				armCleanup(t, store, &state)
				return len(data), nil
			})); err == nil {
				t.Fatal("socket loss/rebind was treated as window closure")
			}
			if current := f.load(t, "topic"); current.Removal.WorktreeRemoved || current.Stage == "removed" || current.Removal.Job.Status != "failed" {
				t.Fatalf("socket identity failure deleted work: %+v", current)
			}
			if _, err := os.Stat(state.Path); err != nil {
				t.Fatal("worktree disappeared", err)
			}
			for range 3 {
				if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
					t.Fatal("public retry discarded the unreachable captured server")
				}
				current := f.load(t, "topic")
				if current.Removal.WorktreeRemoved || current.Stage == "removed" || current.Removal.Job.Token != captured.Token || current.Removal.Job.Window != captured {
					t.Fatalf("public retry erased the original capture: %+v", current)
				}
				if current.ServerPID != captured.ServerPID || current.SocketDevice != captured.SocketDevice || current.SocketInode != captured.SocketInode {
					t.Fatal("retry did not preserve a server baseline independent of the job")
				}
				if _, err := os.Stat(state.Path); err != nil {
					t.Fatal("public retry removed original worktree", err)
				}
			}
			if err := f.run(t, "close", "topic"); err == nil {
				t.Fatal("close claimed the live, unreachable server was gone")
			}
			if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
				t.Fatal("close followed by remove erased the server baseline")
			}
			hostGit(t, state.Path, "commit", "--allow-empty", "-m", "retain newer work")
			if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
				t.Fatal("adopting a new HEAD erased the captured server baseline")
			}
			if current := f.load(t, "topic"); current.ServerPID != captured.ServerPID || current.SocketInode != captured.SocketInode || current.Removal.WorktreeRemoved {
				t.Fatal("new-HEAD recovery discarded the server identity with its old job")
			}
			out, err := oldRunner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", captured.ID, "#{window_id}"}})
			if err != nil || strings.TrimSpace(string(out)) != captured.ID {
				t.Fatalf("original window was killed: %s, %v", out, err)
			}
			if kind == "rebound" {
				out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", captured.ID, "#{window_id}"}})
				if err != nil || strings.TrimSpace(string(out)) != captured.ID {
					t.Fatalf("replacement window was killed: %s, %v", out, err)
				}
			}
		})
	}
}

func TestCleanupSocketLossBeforeCapture(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			ctx, runner, mux, _ := isolatedTmux(t)
			f := newHostFixture(t, "")
			f.app.Mux = mux
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			state := f.load(t, "topic")
			if state.ServerPID <= 1 || state.SocketInode == 0 || state.Socket != runner.socket || state.Removal != nil {
				t.Fatalf("creation did not persist its independent server identity: %+v", state)
			}
			baseline := state.Workspace
			if legacy {
				store := hostStateStore(t, f)
				state.ServerPID, state.SocketDevice, state.SocketInode = 0, 0, 0
				if err := store.save(state); err != nil {
					t.Fatal(err)
				}
				if err := store.unlock(); err != nil {
					t.Fatal(err)
				}
			}
			alias := filepath.Join(filepath.Dir(runner.socket), "old.sock")
			if err := os.Rename(runner.socket, alias); err != nil {
				t.Fatal(err)
			}
			oldRunner := isolatedTmuxRunner{socket: alias, home: runner.home}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = oldRunner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
			})
			for range 3 {
				if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil || !strings.Contains(err.Error(), "restore") {
					t.Fatalf("missing initial socket was treated as absence: %v", err)
				}
				if err := f.run(t, "close", "topic"); err == nil {
					t.Fatal("close silently forgot the unreachable creation baseline")
				}
				current := f.load(t, "topic")
				if current.Stage == "removed" || current.Removal.WorktreeRemoved || current.Removal.Job != nil {
					t.Fatalf("unreachable initial window became an empty capture: %+v", current)
				}
				if !legacy && (current.ServerPID != baseline.ServerPID || current.SocketDevice != baseline.SocketDevice || current.SocketInode != baseline.SocketInode) {
					t.Fatal("close/remove erased the creation identity")
				}
				out, err := oldRunner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", state.Window, "#{window_id}"}})
				if err != nil || strings.TrimSpace(string(out)) != state.Window {
					t.Fatalf("original window did not remain live: %s, %v", out, err)
				}
				if _, err := os.Stat(state.Path); err != nil {
					t.Fatal("initial socket loss removed the worktree", err)
				}
			}
			if err := os.Rename(alias, runner.socket); err != nil {
				t.Fatal(err)
			}
			if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
				t.Fatal("restoring the original socket did not permit normal automatic cleanup", err)
			}
		})
	}
}

func TestCleanupLegacyFailedSocketLoss(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	f := newHostFixture(t, "")
	store, state, _ := queuedCleanup(t, f)
	window, err := mux.Create(ctx, session, freshTmuxWorkspace(state.Workspace), []Pane{{}}, [][]string{{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	state.Window, state.Socket = window, runner.socket
	state.Removal.Job.Status = "failed"
	state.Removal.Job.Window = CleanupWindow{ID: window, Token: state.Removal.Job.Token, Socket: runner.socket}
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(runner.socket), "old.sock")
	if err := os.Rename(runner.socket, alias); err != nil {
		t.Fatal(err)
	}
	oldRunner := isolatedTmuxRunner{socket: alias, home: runner.home}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = oldRunner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
	})
	f.app.Mux = mux
	for range 3 {
		if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil || !strings.Contains(err.Error(), "restore") {
			t.Fatalf("legacy failed job became an empty capture: %v", err)
		}
		if current := f.load(t, "topic"); current.Removal.Job.Token != state.Removal.Job.Token || current.Removal.Job.Window.ID != window || current.Removal.WorktreeRemoved {
			t.Fatal("legacy retry erased unverified window history")
		}
	}
	if _, err := os.Stat(state.Path); err != nil {
		t.Fatal(err)
	}
	out, err := oldRunner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", window, "#{window_id}"}})
	if err != nil || strings.TrimSpace(string(out)) != window {
		t.Fatalf("legacy retry killed the original window: %s, %v", out, err)
	}
}

func TestCleanupRetryRotatesTokenOnVerifiedServer(t *testing.T) {
	ctx, runner, mux, _ := isolatedTmux(t)
	f := newHostFixture(t, "")
	f.app.Mux = mux
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	blocked := mux
	blocked.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
		for _, arg := range p.Args {
			if strings.HasPrefix(arg, "kill-window -t ") {
				return nil, fmt.Errorf("temporary close failure")
			}
		}
		return runner.Run(ctx, p)
	})
	f.app.Mux = blocked
	if err := f.run(t, "remove", "topic", "--keep-branch"); err == nil {
		t.Fatal("expected first close to fail")
	}
	old := f.load(t, "topic")
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-w", "-t", old.Window, "@cli_workmux_cleanup", strings.Repeat("e", 32)}}); err != nil {
		t.Fatal(err)
	}
	f.app.Mux = mux
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal("verified same-server retry could not replace an obsolete token", err)
	}
	current := f.load(t, "topic")
	if current.Stage != "removed" || current.Removal.Job.Token == old.Removal.Job.Token || current.ServerPID != old.ServerPID || current.SocketInode != old.SocketInode {
		t.Fatalf("retry did not preserve server identity while rotating the attempt: %+v", current)
	}
	var ready bytes.Buffer
	command := CleanupCommand{RepoID: old.RepoID, ID: old.ID, Token: old.Removal.Job.Token}
	if err := f.app.RunCleanup(ctx, command, &ready); err == nil || ready.Len() != 0 {
		t.Fatalf("obsolete worker was still accepted: %v, %q", err, ready.String())
	}
}

func TestCleanupBrokenStdoutDoesNotArmWorker(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	state.Socket = ""
	window, err := mux.Create(ctx, session, freshTmuxWorkspace(state.Workspace), []Pane{{}}, [][]string{{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	state.Window, state.Socket = window, runner.socket
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	pane, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_id}"}})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := write.Close(); err != nil {
			t.Error(err)
		}
	})
	pidPath := filepath.Join(f.home, "broken-pipe-worker")
	cmd := exec.CommandContext(ctx, executable, "workmux", "remove", "--keep-branch")
	cmd.Dir = state.Path
	cmd.Env = append(os.Environ(), "HOME="+f.home, "WORKMUX_TEST_HOME="+f.home,
		"WORKMUX_TEST_STATE="+f.state, "WORKMUX_TEST_CONFIG="+f.config, "WORKMUX_TEST_OUTPUT=",
		"WORKMUX_TEST_WORKER_PID="+pidPath, "TMUX="+runner.socket+",0,0", "TMUX_PANE="+strings.TrimSpace(string(pane)))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = write, &stderr
	err = cmd.Run()
	if cmd.ProcessState == nil {
		t.Fatalf("broken-pipe child did not start: %v, %s", err, stderr.String())
	}
	status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if err == nil || !ok || !status.Signaled() || status.Signal() != syscall.SIGPIPE {
		t.Fatalf("child did not exercise real fd1 SIGPIPE: %v, %v, %s", err, status, stderr.String())
	}
	state = f.load(t, "topic")
	if state.Removal == nil || state.Removal.Job.Status != "queued" {
		t.Fatalf("SIGPIPE left an executable armed handoff: %+v", state)
	}
	waitHostFile(t, ctx, pidPath)
	pidData, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidData))
	if err != nil {
		t.Fatal(err)
	}
	waitCleanupProcessExit(t, ctx, pid)
	if present, err := mux.CapturedExists(ctx, state.Workspace, state.Removal.Job.Window); err != nil || !present {
		t.Fatalf("worker closed the window after parent SIGPIPE: %v, %v", present, err)
	}
	if _, err := os.Stat(state.Path); err != nil || f.load(t, "topic").Removal.WorktreeRemoved {
		t.Fatal("worker deleted data after failed scheduling output", err)
	}
}

func TestCleanupWorkerRejectsChangedIdentityRefAndToken(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "admin", "ref", "dirty", "ignored", "token", "cancel", "expired"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			store, state, command := queuedCleanup(t, f)
			ready := cleanupReadyFunc(func(data []byte) (int, error) {
				state.Removal.Job.Status = "armed"
				switch kind {
				case "token":
					state.Removal.Job.Token = strings.Repeat("d", 32)
					state.Removal.Job.Window.Token = state.Removal.Job.Token
				case "cancel":
					state.Removal.Job.Status = "cancelled"
				case "expired":
					state.Removal.Job.Deadline = time.Now().Add(-time.Second).UnixNano()
				}
				if err := store.save(state); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "directory", "symlink":
					old := state.Path + "-old"
					if err := os.Rename(state.Path, old); err != nil {
						t.Fatal(err)
					}
					if kind == "symlink" {
						if err := os.Symlink(old, state.Path); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Mkdir(state.Path, 0700); err != nil {
						t.Fatal(err)
					}
				case "admin":
					path := filepath.Join(state.Removal.Identity.AdminPath, "gitdir")
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(path, path+"-old"); err != nil {
						t.Fatal(err)
					}
					hostWrite(t, path, string(data))
				case "ref":
					hostGit(t, state.Path, "commit", "--allow-empty", "-m", "concurrent commit")
				case "dirty":
					hostWrite(t, filepath.Join(state.Path, "tracked"), "late work")
				case "ignored":
					hostWrite(t, filepath.Join(state.Path, ".env"), "SECRET=value")
				}
				if err := store.unlock(); err != nil {
					t.Fatal(err)
				}
				return len(data), nil
			})
			if err := f.app.RunCleanup(context.Background(), command, ready); err == nil {
				t.Fatalf("accepted changed %s", kind)
			}
			if slices.Contains(f.events, "git:worktree remove") {
				t.Fatal("failed worker deleted data")
			}
			if _, err := os.Lstat(state.Path); err != nil {
				t.Fatal("replacement or original worktree disappeared", err)
			}
		})
	}
}

func TestCleanupWorkerDuplicateCannotRemoveRecreatedWorkspace(t *testing.T) {
	f := newHostFixture(t, "")
	store, state, command := queuedCleanup(t, f)
	if err := f.app.RunCleanup(context.Background(), command, cleanupReadyFunc(func(data []byte) (int, error) {
		armCleanup(t, store, &state)
		return len(data), nil
	})); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	var ready bytes.Buffer
	if err := f.app.RunCleanup(context.Background(), command, &ready); err == nil || ready.Len() != 0 {
		t.Fatalf("old worker accepted recreated same-name workspace: %v, %q", err, ready.String())
	}
	if state := f.load(t, "topic"); state.Stage != "ready" {
		t.Fatal("old worker changed recreated workspace")
	}
}

func TestCleanupSchedulingIsNotCompletionAndCanBeCancelled(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(context.Context, CleanupLaunch) error { return nil })
	if err := f.run(t, "merge", "topic"); err != nil {
		t.Fatal(err)
	}
	state := f.load(t, "topic")
	if state.Stage == "removed" || state.Removal.Job.Status != "armed" || !strings.Contains(f.stdout.String(), "merge succeeded; cleanup scheduled") {
		t.Fatalf("scheduled cleanup reported completion: %+v, %s", state, f.stdout.String())
	}
	if err := f.run(t, "remove", "topic"); err == nil || !strings.Contains(err.Error(), "already scheduled") {
		t.Fatalf("duplicate public invocation was not serialized: %v", err)
	}
	if err := f.run(t, "close", "topic"); err != nil {
		t.Fatal("could not cancel pending cleanup", err)
	}
	if state := f.load(t, "topic"); state.Removal.Job.Status != "cancelled" || state.Removal.WorktreeRemoved {
		t.Fatal("close did not cancel the handoff while preserving worktree")
	}
	f.mux.caller = false
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupExpiredMissingWorkerCanBeRetried(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(context.Context, CleanupLaunch) error { return nil })
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	state := f.load(t, "topic")
	oldToken := state.Removal.Job.Token
	if _, err := os.Stat(state.Path); err != nil {
		t.Fatal("missing worker was treated as completed cleanup", err)
	}
	state.Removal.Job.Deadline = time.Now().Add(-time.Second).UnixNano()
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	f.mux.caller = false
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal("expired handoff could not be retried", err)
	}
	if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal.Job.Token == oldToken {
		t.Fatal("retry reused stale worker token")
	}
}

func TestCleanupRemoveDoesNotClaimNewMergeSuccess(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostGit(t, w.Path, "commit", "--allow-empty", "-m", "unmerged retained work")
	f.stdout.Reset()
	f.mux.caller = true
	f.app.Spawner = cleanupSpawnFunc(func(context.Context, CleanupLaunch) error { return nil })
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.stdout.String(), "cleanup scheduled") || strings.Contains(f.stdout.String(), "merge succeeded") {
		t.Fatalf("remove claimed a new merge: %s", f.stdout.String())
	}
	if err := f.run(t, "merge", "topic"); err == nil || !strings.Contains(err.Error(), "retry remove") {
		t.Fatalf("merge adopted plain removal of a different source commit: %v", err)
	}
}

func TestCleanupExternalUsesSavedSocketWithoutTMUX(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	w.Socket = ""
	window, err := mux.Create(ctx, session, freshTmuxWorkspace(w.Workspace), []Pane{{}}, [][]string{{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	store := hostStateStore(t, f)
	w.Window, w.Socket = window, runner.socket
	if err := store.save(w); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	f.app.Mux = Tmux{Runner: ExecRunner{}}
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal("external cleanup lost original named-server routing", err)
	}
	state := f.load(t, "topic")
	if state.Removal.Job.Window.Socket != runner.socket || state.Removal.Job.Window.Caller {
		t.Fatalf("wrong external target: %+v", state.Removal.Job.Window)
	}
	if present, err := mux.CapturedExists(ctx, state.Workspace, state.Removal.Job.Window); err != nil || present {
		t.Fatalf("source window was not closed on saved server: %v, %v", present, err)
	}
}

func TestCleanupActualSelfWindowProcess(t *testing.T) {
	for _, command := range []string{"remove", "merge"} {
		t.Run(command, func(t *testing.T) {
			ctx, runner, mux, session := isolatedTmux(t)
			f := newHostFixture(t, "pre_remove:\n  - |\n    printf 'run\\n' >> \"$HOME/hook-count\"\n")
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			if command == "merge" {
				hostWrite(t, filepath.Join(w.Path, "tracked"), "merged work\n")
				hostGit(t, w.Path, "commit", "-am", "merge source")
			}
			gate := filepath.Join(f.home, "start")
			proofPath := filepath.Join(f.home, "proof")
			output := filepath.Join(f.home, "output")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			paneCommand := []string{"env", "HOME=" + f.home, "WORKMUX_TEST_HOME=" + f.home,
				"WORKMUX_TEST_STATE=" + f.state, "WORKMUX_TEST_CONFIG=" + f.config,
				"WORKMUX_TEST_GATE=" + gate, "WORKMUX_TEST_PROOF=" + proofPath,
				"WORKMUX_TEST_OUTPUT=" + output, "WORKMUX_TEST_HOLD_PARENT=1", executable, "workmux", command}
			if command == "remove" {
				paneCommand = append(paneCommand, "--keep-branch")
			}
			window, err := mux.Create(ctx, session, freshTmuxWorkspace(w.Workspace), []Pane{{}}, [][]string{paneCommand})
			if err != nil {
				t.Fatal(err)
			}
			store := hostStateStore(t, f)
			w.Window, w.Socket = window, runner.socket
			if err := store.save(w); err != nil {
				t.Fatal(err)
			}
			if err := store.unlock(); err != nil {
				t.Fatal(err)
			}
			pidData, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_pid}"}})
			if err != nil {
				t.Fatal(err)
			}
			parentPID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil || parentPID <= 1 {
				t.Fatalf("invalid source pane PID %q", pidData)
			}
			if command == "merge" {
				// Exercise the last-window case: the entire isolated server exits.
				if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"kill-pane", "-t", mux.Getenv("TMUX_PANE")}}); err != nil {
					t.Fatal(err)
				}
			}
			hostWrite(t, gate, "start")
			for {
				state, readErr := readState(filepath.Join(f.state, w.RepoID, w.ID+".json"))
				if readErr == nil && state.Stage == "removed" {
					break
				}
				if readErr == nil && state.Removal != nil && state.Removal.Job != nil && state.Removal.Job.Status == "failed" {
					data, _ := os.ReadFile(filepath.Join(f.state, state.RepoID, state.ID+"."+state.Removal.Job.Token+".cleanup.log"))
					t.Fatalf("actual worker failed: %s", data)
				}
				if ctx.Err() != nil {
					out, _ := runner.Run(context.Background(), Process{Name: "tmux", Args: []string{"capture-pane", "-p", "-t", window}})
					t.Fatalf("actual cleanup timed out: %+v, read error: %v\n%s", state, readErr, out)
				}
				// The worker atomically replaces its journal while this observer has no lock.
				time.Sleep(20 * time.Millisecond)
			}
			data, err := os.ReadFile(proofPath)
			if err != nil {
				t.Fatal(err)
			}
			var proof cleanupProof
			if err := json.Unmarshal(data, &proof); err != nil {
				t.Fatal(err)
			}
			if !proof.WindowGone || !proof.SessionLeader || !proof.NoControllingTerminal || !proof.NullInputOutput || !proof.SurvivingCwd {
				t.Fatalf("worker was not safely detached before deletion: %+v", proof)
			}
			data, err = os.ReadFile(output)
			if err != nil || !bytes.Contains(data, []byte("cleanup scheduled")) {
				t.Fatalf("parent did not report asynchronous scheduling: %s, %v", data, err)
			}
			data, err = os.ReadFile(filepath.Join(f.home, "hook-count"))
			if err != nil || string(data) != "run\n" {
				t.Fatalf("pre_remove did not run exactly once before worker: %q, %v", data, err)
			}
			if _, err := os.Lstat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("actual worktree remains: %v", err)
			}
			state := f.load(t, "topic")
			if present, err := mux.CapturedExists(ctx, state.Workspace, state.Removal.Job.Window); err != nil || present {
				t.Fatalf("source window remains: %v, %v", present, err)
			}
			if command == "remove" {
				if got := hostGit(t, f.root, "rev-parse", "topic"); got != w.InitialCommit {
					t.Fatal("self-window removal deleted kept branch")
				}
			} else if got := hostGit(t, f.root, "rev-parse", "main"); got != state.MergedCommit {
				t.Fatal("self-window merge lost target commit")
			}
			waitCleanupProcessExit(t, ctx, parentPID)
			waitCleanupProcessExit(t, ctx, proof.PID)
		})
	}
}

func waitCleanupProcessExit(t *testing.T, ctx context.Context, pid int) {
	t.Helper()
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if runtime.GOOS == "linux" {
			data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if os.IsNotExist(err) {
				return
			}
			if _, rest, ok := strings.Cut(string(data), ") "); ok && strings.HasPrefix(rest, "Z ") {
				// An adopted child may await the host init's reaper; it is no longer running.
				return
			}
		}
		if ctx.Err() != nil {
			t.Fatalf("cleanup left process %d running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
