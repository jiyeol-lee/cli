package workmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

type sandboxLockContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *sandboxLockContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

type sandboxLockGitRunner struct{}

func (sandboxLockGitRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	// Only the lock wait should announce that it reached the retry select.
	if observed, ok := ctx.(*sandboxLockContext); ok {
		ctx = observed.Context
	}
	return (ExecRunner{}).Run(ctx, p)
}

type sandboxLockSandbox struct {
	Sandbox
	run func(context.Context, Workspace, []string, func() error) error
}

func (sandbox sandboxLockSandbox) RunStandalone(ctx context.Context, w Workspace, words []string, _ io.Reader, _, _ io.Writer, visible func() error) error {
	return sandbox.run(ctx, w, words, visible)
}

func sandboxLockFixture(t *testing.T) (App, gitRepository, Workspace) {
	t.Helper()
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "topic"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic").Workspace
	app := f.app
	app.Runner = sandboxLockGitRunner{}
	app.Getwd = func() (string, error) { return w.Path, nil }
	app.Stdout, app.Stderr = io.Discard, io.Discard
	repo, err := (gitHost{runner: app.Runner}).discover(t.Context(), w.Path)
	if err != nil {
		t.Fatal(err)
	}
	return app, repo, w
}

func sandboxLockAwait(t *testing.T, ctx context.Context, waiting <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("sandbox returned before waiting for the repository lock: %v", err)
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
}

func TestSandboxLockWaitsForCreator(t *testing.T) {
	app, repo, w := sandboxLockFixture(t)
	creator, err := lockState(app.StateDir, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = creator.unlock() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	observed := &sandboxLockContext{Context: ctx, waiting: make(chan struct{})}
	launched := make(chan Workspace, 1)
	app.Sandbox = sandboxLockSandbox{run: func(_ context.Context, got Workspace, words []string, visible func() error) error {
		if len(words) != 1 || words[0] != "opencode" {
			return fmt.Errorf("pane payload changed")
		}
		if err := visible(); err != nil {
			return err
		}
		launched <- got
		return nil
	}}
	command, err := ParseCommand([]string{"sandbox", "run", "--", "opencode"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- app.Run(observed, command) }()
	sandboxLockAwait(t, ctx, observed.waiting, done)
	select {
	case got := <-launched:
		t.Fatalf("sandbox launched while its creator held the lock: %+v", got)
	case err := <-done:
		t.Fatalf("sandbox returned while its creator held the lock: %v", err)
	default:
	}
	if _, err := lockState(app.StateDir, repo); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("ordinary command did not retain nonblocking busy behavior: %v", err)
	}
	if err := creator.unlock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	if got := <-launched; got.ID != w.ID || got.Path != w.Path || got.Stage != "ready" {
		t.Fatalf("sandbox did not load the creator's managed workspace: %+v", got)
	}
}

func TestSandboxLockCancellationPreservesCause(t *testing.T) {
	for _, cause := range []error{ErrHangup, context.DeadlineExceeded} {
		t.Run(cause.Error(), func(t *testing.T) {
			app, repo, _ := sandboxLockFixture(t)
			creator, err := lockState(app.StateDir, repo)
			if err != nil {
				t.Fatal(err)
			}
			defer creator.unlock()
			var ctx context.Context
			var cancel func()
			if cause == context.DeadlineExceeded {
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
			} else {
				cancelCtx, cancelCause := context.WithCancelCause(t.Context())
				ctx = cancelCtx
				cancel = func() { cancelCause(cause) }
			}
			defer cancel()
			observed := &sandboxLockContext{Context: ctx, waiting: make(chan struct{})}
			app.Sandbox = sandboxLockSandbox{run: func(context.Context, Workspace, []string, func() error) error {
				t.Error("canceled lock wait launched a sandbox")
				return nil
			}}
			done := make(chan error, 1)
			go func() { done <- app.Run(observed, Command{Kind: "sandbox", Name: "run"}) }()
			guard, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			sandboxLockAwait(t, guard, observed.waiting, done)
			if cause != context.DeadlineExceeded {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, cause) {
					t.Fatalf("lock cancellation = %v, want %v", err, cause)
				}
			case <-guard.Done():
				t.Fatal("cancellation did not end the repository lock wait")
			}
		})
	}
}

func TestSandboxLockSerialPreparationIndependentRuns(t *testing.T) {
	app, repo, _ := sandboxLockFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	firstPrepared := make(chan struct{})
	releasePreparation := make(chan struct{})
	live := make(chan string, 2)
	exit := make(chan struct{})
	defer close(exit)
	app.Sandbox = sandboxLockSandbox{run: func(ctx context.Context, _ Workspace, words []string, visible func() error) error {
		if words[0] == "first" {
			close(firstPrepared)
			select {
			case <-releasePreparation:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
		}
		if err := visible(); err != nil {
			return err
		}
		live <- words[0]
		select {
		case <-exit:
			return nil
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}}
	firstDone, secondDone := make(chan error, 1), make(chan error, 1)
	go func() {
		firstDone <- app.Run(ctx, Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"first"}})
	}()
	sandboxLockAwait(t, ctx, firstPrepared, firstDone)
	observed := &sandboxLockContext{Context: ctx, waiting: make(chan struct{})}
	go func() {
		secondDone <- app.Run(observed, Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"second"}})
	}()
	sandboxLockAwait(t, ctx, observed.waiting, secondDone)
	select {
	case name := <-live:
		t.Fatalf("run became live before serialized preparation finished: %s", name)
	default:
	}
	close(releasePreparation)
	seen := make(map[string]bool)
	for range 2 {
		select {
		case name := <-live:
			seen[name] = true
		case err := <-firstDone:
			t.Fatalf("first run exited instead of remaining live: %v", err)
		case err := <-secondDone:
			t.Fatalf("second run exited instead of remaining live: %v", err)
		case <-ctx.Done():
			t.Fatal(context.Cause(ctx))
		}
	}
	if !seen["first"] || !seen["second"] {
		t.Fatalf("concurrent live runs = %v", seen)
	}
	store, err := lockState(app.StateDir, repo)
	if err != nil {
		t.Fatalf("live runs retained the preparation lock: %v", err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	cancel()
	for _, done := range []<-chan error{firstDone, secondDone} {
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("live run cancellation = %v", err)
		}
	}
}

func TestSandboxLockDoesNotRetryInvalidState(t *testing.T) {
	for _, kind := range []string{"directory", "lock file", "lock open"} {
		t.Run(kind, func(t *testing.T) {
			app, repo, _ := sandboxLockFixture(t)
			path := filepath.Join(app.StateDir, identity(repo.CommonDir), "lock")
			switch kind {
			case "directory":
				path = app.StateDir
			case "lock open":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if kind != "lock open" {
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			observed := &sandboxLockContext{Context: ctx, waiting: make(chan struct{})}
			store, err := waitSandboxState(observed, app.StateDir, repo)
			if err == nil || store != nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("invalid state lock = %v, %v", store, err)
			}
			select {
			case <-observed.waiting:
				t.Fatal("invalid state entered the contention retry loop")
			default:
			}
		})
	}
}
