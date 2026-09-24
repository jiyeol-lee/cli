package workmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSandboxStandaloneConcurrent(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	app.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: filepath.Dir(w.Root)}
	var events []string
	app.Mux = &hostTestMux{events: &events, window: "@7"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered := make(chan []string, 2)
	exit := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var mu sync.Mutex
	next := 0
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		call := sandboxTestPodmanProcess(p)
		mu.Lock()
		if call.Name == "podman" && call.Args[0] == "run" {
			index := next
			next++
			sandboxTestSession(t, engine, call.Args)
			mu.Unlock()
			entered <- call.Args
			select {
			case <-exit[index]:
				return nil, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		defer mu.Unlock()
		return engine.Run(ctx, p)
	})
	done := []chan error{make(chan error, 1), make(chan error, 1)}
	var launches [][]string
	for i := range 2 {
		go func() { done[i] <- app.Run(ctx, Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"bash"}}) }()
		select {
		case args := <-entered:
			launches = append(launches, args)
		case err := <-done[i]:
			t.Fatalf("launch failed: %v", err)
		case <-ctx.Done():
			t.Fatal("launch retained repository lock")
		}
	}
	value := func(args []string, flag string) string {
		for i, arg := range args {
			if arg == flag {
				return args[i+1]
			}
		}
		return ""
	}
	if value(launches[0], "--name") == value(launches[1], "--name") {
		t.Fatal("concurrent runs share an identity")
	}
	for i, args := range launches {
		if strings.Join(args[:3], " ") != "run --rm -it" {
			t.Fatalf("run %d is not unconditionally interactive: %v", i, args)
		}
	}
	if err := c.checkStandalone(ctx, w); err == nil || !strings.Contains(err.Error(), "active standalone") {
		t.Fatalf("missing checkout protection: %v", err)
	}
	if err := c.Stop(ctx, w); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	remaining := len(engine.sessions)
	mu.Unlock()
	if remaining != 2 {
		t.Fatal("managed stop affected standalone sessions")
	}
	for i := range 2 {
		close(exit[i])
		if err := <-done[i]; err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		remaining := len(engine.sessions)
		mu.Unlock()
		if remaining != 1-i {
			t.Fatalf("independent cleanup left %d sessions", remaining)
		}
	}
	if err := c.checkStandalone(ctx, w); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app.StateDir, w.RepoID, w.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("standalone created managed state: %v", err)
	}
}

func TestSandboxRunFailedRegistrationUnlocks(t *testing.T) {
	sandboxTestNonroot(t)
	for _, mode := range []string{"short", "error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			app := sandboxCommandApp(c, w)
			repo, err := (gitHost{runner: app.Runner}).discover(t.Context(), w.Path)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := fmt.Errorf("launch failed")
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				call := sandboxTestPodmanProcess(p)
				if call.Name == "podman" && call.Args[0] == "run" {
					store, err := lockState(app.StateDir, repo)
					if err != nil {
						t.Fatal("launch retained repository lock")
					}
					if err := store.unlock(); err != nil {
						t.Fatal(err)
					}
					switch mode {
					case "error":
						return nil, failure
					case "cancel":
						cancel()
						return nil, context.Cause(ctx)
					}
					return nil, nil
				}
				return engine.Run(ctx, p)
			})
			err = app.Run(ctx, Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"bash"}})
			if mode == "short" && err != nil || mode == "error" && !errors.Is(err, failure) || mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("launch result: %v", err)
			}
			store, err := lockState(app.StateDir, repo)
			if err != nil {
				t.Fatal(err)
			}
			defer store.unlock()
			states, err := store.load()
			if err != nil || len(states) != 0 {
				t.Fatalf("created managed state: %+v %v", states, err)
			}
			if err := c.checkStandalone(t.Context(), w); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSandboxLegacyExplicitShellDiscovery(t *testing.T) {
	sandboxTestNonroot(t)
	for _, action := range []string{"close", "remove", "merge", "orphan"} {
		t.Run(action, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			app := sandboxCommandApp(c, w)
			var events []string
			app.Mux = &hostTestMux{events: &events, window: "@7"}
			args, err := c.PaneCommand(t.Context(), w, "bash")
			if err != nil {
				t.Fatal(err)
			}
			sandboxTestSession(t, engine, args)
			repo, err := (gitHost{runner: app.Runner}).discover(t.Context(), w.Path)
			if err != nil {
				t.Fatal(err)
			}
			if action == "orphan" {
				store, err := lockState(app.StateDir, repo)
				if err != nil {
					t.Fatal(err)
				}
				w.Config.Sandbox.Enabled = false
				if err := store.save(workspaceState{Workspace: w}); err != nil {
					t.Fatal(err)
				}
				if err := store.unlock(); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(w.Path); err != nil {
					t.Fatal(err)
				}
			}
			app.Getwd = func() (string, error) { return w.Root, nil }
			command := Command{Kind: action, Name: w.Branch, Into: "main", Force: true, KeepBranch: true}
			if action == "orphan" {
				command.Kind = "remove"
			}
			if err := app.Run(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if len(engine.sessions) != 0 {
				t.Fatal("lifecycle skipped an unrecorded explicit shell")
			}
		})
	}
}

func TestSandboxFinishedStandaloneHostOnlyRemoval(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	var events []string
	app.Mux = &hostTestMux{events: &events, window: "@7"}
	if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(w), "runs")
	if _, err := os.Stat(parent); err != nil {
		t.Fatal(err)
	}
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		if sandboxTestPodmanProcess(p).Name == "podman" {
			t.Error("host-only cleanup consulted unavailable Podman")
			return nil, fmt.Errorf("Podman unavailable")
		}
		return engine.Run(ctx, p)
	})
	app.Getwd = func() (string, error) { return w.Root, nil }
	if err := app.Run(t.Context(), Command{Kind: "close", Name: w.Branch}); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), Command{Kind: "remove", Name: w.Branch, KeepBranch: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree remains: %v", err)
	}
}

func TestRememberSandboxHistory(t *testing.T) {
	for _, test := range []struct {
		name  string
		entry string
		file  bool
		want  bool
	}{
		{name: "absent"},
		{name: "empty"},
		{name: "standalone parent", entry: "runs"},
		{name: "standalone snapshot", entry: "runs/session/fingerprint"},
		{name: "legacy snapshot", entry: strings.Repeat("a", 64), want: true},
		{name: "endpoint", entry: "endpoint", file: true, want: true},
		{name: "unexpected file", entry: "runs", file: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := t.TempDir()
			state := workspaceState{}
			parent := filepath.Join(base, "containers", sandboxWorkspaceKey(state.Workspace))
			if test.name != "absent" {
				path := filepath.Join(parent, test.entry)
				if test.file {
					sandboxTestWrite(t, path, "record")
				} else if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := rememberSandbox(base, &state); err != nil {
				t.Fatal(err)
			}
			if state.SandboxUsed != test.want {
				t.Fatalf("SandboxUsed = %v, want %v", state.SandboxUsed, test.want)
			}
		})
	}
}
