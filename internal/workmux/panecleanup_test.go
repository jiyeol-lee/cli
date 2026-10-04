package workmux

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestPaneHangupCleanupLeavesManualRunActive(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	paneCtx, hangup := context.WithCancelCause(t.Context())
	defer hangup(context.Canceled)
	var pane, manual *sandboxInspection
	var removed []string
	cleanupCalls := 0
	runPaths := func(found *sandboxInspection) (string, string) {
		session := found.Config.Labels[sandboxLabel+"session"]
		return filepath.Join(standaloneDirectory(c.StateDir, w), session+".json"),
			filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(w), "runs", session)
	}
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name != "podman" {
			return engine.Run(ctx, p)
		}
		if paneCtx.Err() != nil {
			if ctx.Err() != nil {
				t.Fatalf("cleanup inherited canceled context: %v", ctx.Err())
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("cleanup has no deadline")
			}
			cleanupCalls++
		}
		if podman.Args[0] == "stop" {
			t.Fatal("pane cleanup stopped another session")
		}
		if podman.Args[0] == "rm" {
			if len(podman.Args) != 3 || podman.Args[1] != "--force" {
				t.Fatalf("cleanup removal = %q", podman.Args)
			}
			removed = append(removed, podman.Args[2])
		}
		if podman.Args[0] != "run" {
			return engine.Run(ctx, p)
		}
		if !p.Foreground {
			t.Fatal("sandbox run is not foreground")
		}
		found := sandboxTestSession(t, engine, podman.Args)
		registration, snapshots := runPaths(found)
		if _, err := os.Stat(registration); err != nil {
			t.Fatalf("run missing registration: %v", err)
		}
		if _, err := os.Stat(snapshots); err != nil {
			t.Fatalf("run missing mount snapshots: %v", err)
		}
		switch podman.Args[len(podman.Args)-1] {
		case "pane":
			pane = found
			hangup(ErrHangup)
			<-ctx.Done()
			return nil, context.Cause(ctx)
		case "manual":
			manual = found
			manualRegistration, err := os.ReadFile(registration)
			if err != nil {
				t.Fatal(err)
			}
			manualSnapshots := standaloneLifecycleSnapshots(t, snapshots)
			// Nest the pane run so both sessions are live without sharing the fake engine across goroutines.
			if err := c.RunStandalone(paneCtx, w, []string{"pane"}, nil, io.Discard, io.Discard, nil); !errors.Is(err, ErrHangup) {
				t.Fatalf("pane hangup = %v", err)
			}
			if pane == nil || !slices.Equal(removed, []string{pane.ID}) {
				t.Fatalf("pane cleanup removed %v, pane = %+v", removed, pane)
			}
			if ctx.Err() != nil || !manual.State.Running || len(engine.sessions) != 1 || engine.sessions[0] != manual {
				t.Fatal("pane cleanup canceled or removed the manual run")
			}
			paneRegistration, paneSnapshots := runPaths(pane)
			for _, path := range []string{paneRegistration, paneSnapshots} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("pane cleanup left %s: %v", path, err)
				}
			}
			if data, err := os.ReadFile(registration); err != nil || string(data) != string(manualRegistration) {
				t.Fatalf("pane cleanup changed manual registration: %q, %v", data, err)
			}
			remaining := standaloneLifecycleSnapshots(t, snapshots)
			if len(remaining) != len(manualSnapshots) {
				t.Fatal("pane cleanup changed manual snapshots")
			}
			for path, data := range manualSnapshots {
				if current, ok := remaining[path]; !ok || string(current) != string(data) {
					t.Fatalf("pane cleanup changed manual snapshot %s", path)
				}
			}
			if err := c.checkStandalone(ctx, w); err == nil || !strings.Contains(err.Error(), "active standalone sandbox") {
				t.Fatalf("manual registration no longer blocks checkout removal: %v", err)
			}
			return nil, nil
		default:
			t.Fatalf("unexpected sandbox command: %q", podman.Args)
			return nil, nil
		}
	})
	if err := c.RunStandalone(t.Context(), w, []string{"manual"}, nil, io.Discard, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if pane == nil || manual == nil || !slices.Equal(removed, []string{pane.ID, manual.ID}) || len(engine.sessions) != 0 {
		t.Fatalf("final cleanup removed = %v, sessions = %d", removed, len(engine.sessions))
	}
	if cleanupCalls == 0 {
		t.Fatal("hangup did not run cleanup")
	}
	for _, found := range []*sandboxInspection{pane, manual} {
		registration, snapshots := runPaths(found)
		for _, path := range []string{registration, snapshots} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("completed run left %s: %v", path, err)
			}
		}
	}
	if err := c.checkStandalone(t.Context(), w); err != nil {
		t.Fatalf("completed runs still block checkout removal: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "tracked")); err != nil || string(data) != "base\n" {
		t.Fatalf("cleanup changed checkout content: %q, %v", data, err)
	}
}
