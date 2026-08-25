package memory

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fakeWorktreeRunner struct {
	output    string
	err       error
	directory string
}

func (r *fakeWorktreeRunner) List(_ context.Context, directory string) ([]byte, error) {
	r.directory = directory
	return []byte(r.output), r.err
}

func TestResolverUsesFirstWorktree(t *testing.T) {
	runner := &fakeWorktreeRunner{output: "worktree /main/repo\nHEAD abc\n\nworktree /linked/repo\nHEAD def\n"}
	resolver := Resolver{Runner: runner, Getwd: func() (string, error) { return "nested", nil }}
	got, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/main/repo" {
		t.Fatalf("directory = %q", got)
	}
	if !filepath.IsAbs(runner.directory) || !strings.HasSuffix(runner.directory, "/nested") {
		t.Fatalf("git directory = %q", runner.directory)
	}
}

func TestResolverFallsBackOutsideGit(t *testing.T) {
	runner := &fakeWorktreeRunner{err: errNotGitRepository}
	resolver := Resolver{Runner: runner, Getwd: func() (string, error) { return "/outside", nil }}
	got, err := resolver.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "/outside" {
		t.Fatalf("directory = %q", got)
	}
}

func TestResolverErrors(t *testing.T) {
	tests := []struct {
		name   string
		runner *fakeWorktreeRunner
		ctx    context.Context
	}{
		{name: "missing git", runner: &fakeWorktreeRunner{err: errors.New("executable file not found")}, ctx: context.Background()},
		{name: "other Git failure", runner: &fakeWorktreeRunner{err: &exec.ExitError{}}, ctx: context.Background()},
		{name: "malformed output", runner: &fakeWorktreeRunner{output: "HEAD abc\n"}, ctx: context.Background()},
		{name: "relative worktree", runner: &fakeWorktreeRunner{output: "worktree relative\n"}, ctx: context.Background()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := Resolver{Runner: tt.runner, Getwd: func() (string, error) { return "/cwd", nil }}
			if _, err := resolver.Resolve(tt.ctx); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestResolverReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := Resolver{Runner: &fakeWorktreeRunner{err: &exec.ExitError{}}, Getwd: func() (string, error) { return "/cwd", nil }}
	if _, err := resolver.Resolve(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}
