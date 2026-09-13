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
	if w.Config.Sandbox.Enabled {
		if app.Sandbox == nil {
			return fmt.Errorf("sandbox is required for saved %s hooks", kind)
		}
		if err := app.Sandbox.Ensure(ctx, w); err != nil {
			return fmt.Errorf("start sandbox for %s: %w", kind, err)
		}
	}
	for i, command := range commands {
		var err error
		if w.Config.Sandbox.Enabled {
			err = app.Sandbox.Exec(ctx, w, command, workspaceEnv(w, app.ConfigDir), app.Stdin, app.Stdout, app.Stderr)
		} else {
			_, err = app.Runner.Run(ctx, Process{
				Name: "bash", Args: []string{"-c", command}, Dir: w.Path,
				Env: workspaceEnv(w, app.ConfigDir), CleanGitEnv: true, ProcessGroup: true,
				Stdin: app.Stdin, Stdout: app.Stdout, Stderr: app.Stderr,
			})
		}
		if err != nil {
			return fmt.Errorf("%s hook %d failed; workspace preserved: %w", kind, i+1, err)
		}
	}
	return nil
}

func (app *App) paneCommands(w Workspace, panes []Pane) ([][]string, error) {
	commands := make([][]string, len(panes))
	for i, pane := range panes {
		if w.Config.Sandbox.Enabled {
			if app.Sandbox == nil {
				return nil, fmt.Errorf("sandbox is required for saved pane commands")
			}
			commands[i] = app.Sandbox.PaneCommand(w, pane.Command)
			if len(commands[i]) == 0 {
				return nil, fmt.Errorf("sandbox returned an empty pane command; refusing host fallback")
			}
		} else if pane.Command != "" {
			commands[i] = []string{"bash", "-c", pane.Command}
		}
	}
	return commands, nil
}
