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

func TestSandboxShellLifecycleRegistration(t *testing.T) {
	sandboxTestNonroot(t)
	for _, managed := range []bool{false, true} {
		for _, action := range []string{"close", "remove"} {
			t.Run(fmt.Sprintf("managed=%t/%s", managed, action), func(t *testing.T) {
				c, w, engine := sandboxTestFixture(t)
				w.Config.Sandbox.Enabled = false
				app := sandboxCommandApp(c, w)
				var events []string
				app.Mux = &hostTestMux{events: &events, window: "@7"}
				repo, err := (gitHost{runner: app.Runner}).discover(t.Context(), w.Path)
				if err != nil {
					t.Fatal(err)
				}
				if managed {
					store, err := lockState(app.StateDir, repo)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.save(workspaceState{Workspace: w}); err != nil {
						t.Fatal(err)
					}
					if err := store.unlock(); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				entered, launch, exited := make(chan struct{}), make(chan struct{}), make(chan struct{})
				missing, registered := make(chan struct{}), make(chan struct{})
				var mu sync.Mutex
				var once sync.Once
				var firstInspect sync.Once
				c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
					call := sandboxTestPodmanProcess(p)
					if call.Name == "podman" && call.Args[0] == "run" {
						close(entered)
						select {
						case <-launch:
						case <-ctx.Done():
							return nil, context.Cause(ctx)
						}
						select {
						case <-missing:
						case <-ctx.Done():
							return nil, context.Cause(ctx)
						}
						mu.Lock()
						sandboxTestSession(t, engine, call.Args)
						mu.Unlock()
						close(registered)
						select {
						case <-exited:
							return nil, nil
						case <-ctx.Done():
							return nil, context.Cause(ctx)
						}
					}
					if call.Name == "podman" && call.Args[0] == "container" {
						switch call.Args[1] {
						case "inspect":
							first := false
							firstInspect.Do(func() { first = true })
							if first {
								select {
								case <-launch:
								case <-ctx.Done():
									return nil, context.Cause(ctx)
								}
								mu.Lock()
								out, err := engine.Run(ctx, p)
								mu.Unlock()
								close(missing)
								return out, err
							}
						case "ls":
							// Force the real startup race: inspect sees absence, but
							// its confirmation query sees the newly created container.
							select {
							case <-registered:
							case <-ctx.Done():
								return nil, context.Cause(ctx)
							}
						}
					}
					mu.Lock()
					defer mu.Unlock()
					out, err := engine.Run(ctx, p)
					if call.Name == "podman" && (call.Args[0] == "stop" || call.Args[0] == "rm") && err == nil {
						once.Do(func() { close(exited) })
					}
					return out, err
				})
				// Keep Git discovery separate from the concurrent fake engine's call log.
				app.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: filepath.Dir(w.Root)}
				done := make(chan error, 1)
				go func() { done <- app.Run(ctx, Command{Kind: "sandbox", Name: "shell"}) }()
				select {
				case <-entered:
				case <-ctx.Done():
					t.Fatal("shell never started")
				}
				cleanup := app
				cleanup.Getwd = func() (string, error) { return w.Root, nil }
				command := Command{Kind: action, Name: w.Branch, KeepBranch: true, Force: true}
				if err := cleanup.Run(ctx, command); err == nil || !strings.Contains(err.Error(), "repository is busy") {
					t.Fatalf("cleanup passed unregistered launch: %v", err)
				}
				state, err := readState(filepath.Join(app.StateDir, w.RepoID, w.ID+".json"))
				if err != nil || !state.SandboxUsed {
					t.Fatalf("launch not journaled: %+v %v", state, err)
				}
				close(launch)
				// Wait for lock release, not shell completion. Cleanup must now stop it.
				for {
					store, err := lockState(app.StateDir, repo)
					if err == nil {
						if err := store.unlock(); err != nil {
							t.Fatal(err)
						}
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("interactive shell retained repository lock")
					case <-time.After(time.Millisecond):
					}
				}
				if err := cleanup.Run(ctx, command); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("cleanup missed explicit shell")
				}
				mu.Lock()
				remaining := len(engine.sessions)
				mu.Unlock()
				if remaining != 0 {
					t.Fatal("cleanup left an explicit shell running")
				}
			})
		}
	}
}

func TestSandboxShellFailedRegistrationUnlocks(t *testing.T) {
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
					if store, err := lockState(app.StateDir, repo); err == nil {
						if err := store.unlock(); err != nil {
							t.Error(err)
						}
						t.Error("launch ran without the repository lock")
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
			err = app.Run(ctx, Command{Kind: "sandbox", Name: "shell"})
			if mode == "short" && err != nil || mode == "error" && !errors.Is(err, failure) || mode == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("launch result: %v", err)
			}
			store, err := lockState(app.StateDir, repo)
			if err != nil {
				t.Fatalf("completed launch left repository locked: %v", err)
			}
			defer func() {
				if err := store.unlock(); err != nil {
					t.Error(err)
				}
			}()
			states, err := store.load()
			if err != nil || len(states) != 1 || !states[0].SandboxUsed {
				t.Fatalf("lost conservative recovery record: %+v %v", states, err)
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
			// Simulate an older explicit shell: owned labels and protected mounts,
			// but no SandboxUsed bit and no automatic sandbox configuration.
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
