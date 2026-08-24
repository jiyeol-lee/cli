package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type WorktreeRunner interface {
	List(context.Context, string) ([]byte, error)
}

var errNotGitRepository = errors.New("not a Git repository")

type GitWorktreeRunner struct{}

func (GitWorktreeRunner) List(ctx context.Context, directory string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", "-C", directory, "worktree", "list", "--porcelain")
	command.Env = append(os.Environ(), "LC_ALL=C")
	output, err := command.Output()
	if err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && strings.Contains(string(exitError.Stderr), "not a git repository") {
			return nil, errNotGitRepository
		}
	}
	return output, err
}

type Resolver struct {
	Runner WorktreeRunner
	Getwd  func() (string, error)
}

func (r Resolver) Resolve(ctx context.Context) (string, error) {
	getwd := r.Getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	cwd, err := getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	output, err := r.Runner.List(ctx, cwd)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(err, errNotGitRepository) {
			return cwd, nil
		}
		return "", fmt.Errorf("list Git worktrees: %w", err)
	}
	line, _, _ := strings.Cut(string(output), "\n")
	const prefix = "worktree "
	if !strings.HasPrefix(line, prefix) || strings.TrimSpace(strings.TrimPrefix(line, prefix)) == "" {
		return "", fmt.Errorf("parse Git worktree list: malformed first record")
	}
	directory := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(directory) {
		return "", fmt.Errorf("parse Git worktree list: worktree path is not absolute")
	}
	return filepath.Clean(directory), nil
}
