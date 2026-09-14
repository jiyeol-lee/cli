package workmux

import (
	"context"
	"fmt"
)

func workspaceEnv(w Workspace, configDir string) []string {
	return []string{
		"WM_HANDLE=" + w.Handle,
		"WM_WORKTREE_PATH=" + w.Path,
		"WM_PROJECT_ROOT=" + w.Root,
		"WM_CONFIG_DIR=" + configDir,
		"WM_BRANCH_NAME=" + w.Branch,
		"WM_TARGET_BRANCH=" + w.MergeTarget,
		"WORKMUX_HANDLE=" + w.Handle,
	}
}

func (app *App) hooks(ctx context.Context, w Workspace, kind string, commands []string) error {
	if len(commands) == 0 {
		return nil
	}
	for i, command := range commands {
		_, err := app.Runner.Run(ctx, Process{
			Name: "bash", Args: []string{"-c", command}, Dir: w.Path,
			Env: workspaceEnv(w, app.ConfigDir), CleanGitEnv: true, ProcessGroup: true,
			Stdin: app.Stdin, Stdout: app.Stdout, Stderr: app.Stderr,
		})
		if err != nil {
			return fmt.Errorf("%s hook %d failed; workspace preserved: %w", kind, i+1, err)
		}
	}
	return nil
}
