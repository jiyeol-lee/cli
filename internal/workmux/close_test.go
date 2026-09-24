package workmux

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type closeTestMux struct {
	Tmux
	onCapture func(Workspace, CleanupWindow)
	onClose   func(Workspace, CleanupWindow)
}

func (mux closeTestMux) Capture(ctx context.Context, w Workspace, token string) (CleanupWindow, error) {
	window, err := mux.Tmux.Capture(ctx, w, token)
	if err == nil && mux.onCapture != nil {
		mux.onCapture(w, window)
	}
	return window, err
}

func (mux closeTestMux) CloseCaptured(ctx context.Context, w Workspace, window CleanupWindow) error {
	if mux.onClose != nil {
		mux.onClose(w, window)
	}
	return mux.Tmux.CloseCaptured(ctx, w, window)
}

func (closeTestMux) Close(context.Context, Workspace) error {
	return fmt.Errorf("public close must not rediscover its target after unlocking")
}

func TestCloseCapturedWindowCannotCloseConcurrentOrphanAdd(t *testing.T) {
	for _, replacement := range []string{"new workspace", "changed window token"} {
		t.Run(replacement, func(t *testing.T) {
			ctx, runner, mux, _ := isolatedTmux(t)
			f := newHostFixture(t, "")
			f.app.Mux = mux
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			old := f.load(t, "topic")
			hostGit(t, f.root, "worktree", "remove", "--", old.Path)
			captured := make(chan CleanupWindow, 1)
			resume := make(chan struct{})
			f.app.Mux = closeTestMux{Tmux: mux, onClose: func(_ Workspace, window CleanupWindow) {
				captured <- window
				select {
				case <-resume:
				case <-ctx.Done():
				}
			}}
			done := make(chan error, 1)
			go func() { done <- f.app.Run(ctx, Command{Kind: "close", Name: "topic"}) }()
			var window CleanupWindow
			select {
			case window = <-captured:
			case <-ctx.Done():
				t.Fatal("close did not capture its target before releasing the lock")
			}
			// Acquiring the same flock proves all close writes and lock release preceded closure.
			store := hostStateStore(t, f)
			if state := f.load(t, "topic"); state.Stage != "closed" || window.ID != old.Window || !validID(window.Token) {
				t.Fatalf("invalid public close checkpoint: %+v, %+v", state, window)
			}
			if err := store.unlock(); err != nil {
				t.Fatal(err)
			}
			if replacement == "new workspace" {
				if err := mux.CloseCaptured(ctx, old.Workspace, window); err != nil {
					t.Fatal(err)
				}
				if err := f.run(t, "add", "topic", "-b"); err != nil {
					t.Fatal("concurrent add could not recreate the orphan", err)
				}
			} else {
				if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-w", "-t", window.ID, "@cli_workmux_cleanup", strings.Repeat("e", 32)}}); err != nil {
					t.Fatal(err)
				}
			}
			fresh := f.load(t, "topic")
			close(resume)
			if err := <-done; err != nil && !strings.Contains(err.Error(), "replacement owned") && !strings.Contains(err.Error(), "ownership token changed") {
				t.Fatal("close failed without checking its captured ownership", err)
			}
			if found, err := mux.Find(ctx, fresh.Workspace); err != nil || found != fresh.Window {
				t.Fatalf("old close killed the new window: %s, %v", found, err)
			}
			if !reflect.DeepEqual(fresh, f.load(t, "topic")) {
				t.Fatal("old close rewrote state after releasing its lock")
			}
		})
	}
}

func TestCloseEmptyCaptureNeverQueriesConcurrentOrphanAdd(t *testing.T) {
	ctx, _, mux, _ := isolatedTmux(t)
	f := newHostFixture(t, "")
	f.app.Mux = mux
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	if err := mux.Close(ctx, w.Workspace); err != nil {
		t.Fatal(err)
	}
	f.app.Runner = ExecRunner{}
	f.app.Mux = closeTestMux{Tmux: mux, onCapture: func(_ Workspace, window CleanupWindow) {
		t.Error("absent public close should not capture")
	}, onClose: func(Workspace, CleanupWindow) {
		t.Error("empty capture queried a potentially recreated target after unlocking")
	}}
	if err := f.run(t, "close", "topic"); err == nil {
		t.Fatal("absent public close succeeded")
	}
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal("subsequent add failed", err)
	}
	fresh := f.load(t, "topic")
	if fresh.Stage != "ready" || fresh.ID != w.ID || fresh.Socket != w.Socket {
		t.Fatalf("concurrent add did not reuse workspace ownership: %+v", fresh)
	}
	if found, err := mux.Find(ctx, fresh.Workspace); err != nil || found != fresh.Window {
		t.Fatalf("new window did not survive empty close: %s, %v", found, err)
	}
}
