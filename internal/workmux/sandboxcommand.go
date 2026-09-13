package workmux

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed Containerfile
var sandboxContainerfile []byte

const initExample = `# Repository overrides for cli workmux. Nothing here is enabled by init.
# Global defaults live in config.yaml in this directory.
# panes:
#   - command: opencode
#     focus: true
#   - split: horizontal
# layouts:
#   review:
#     panes:
#       - command: opencode
#       - split: vertical
#         percentage: 40
# files:
#   copy: ["<global>", ".env.example"]
#   symlink: ["<global>", "node_modules"]
# post_create: ["<global>", "git status --short"]
# pre_merge: ["<global>", "go test ./..."]
# pre_remove: ["<global>"]
# sandbox:
#   enabled: true
#   image: localhost/cli-workmux:fedora44
#   target: agent
#   opencode_config_dir: ~/dotfiles/.opencode # Optional real directory, not a symlink.
`

const globalInitExample = `# Global defaults for cli workmux. Nothing here is enabled by init.
# Repository configuration overrides these defaults field by field.
# panes:
#   - command: opencode
#     focus: true
#   - split: horizontal
# files:
#   copy: [".env.example"]
#   symlink: ["node_modules"]
# post_create: ["git status --short"]
# pre_merge: ["go test ./..."]
# pre_remove: []
# sandbox:
#   enabled: true
#   image: localhost/cli-workmux:fedora44
#   target: agent
#   opencode_config_dir: ~/dotfiles/.opencode # Optional real directory, not a symlink.
`

func parseSandboxCommand(args []string) (Command, error) {
	c := Command{Kind: args[0]}
	usage := func() (Command, error) {
		return Command{}, fmt.Errorf("usage: cli workmux init [-g|--global] | sandbox build | sandbox shell [-e|--exec] [-- command...]")
	}
	args = args[1:]
	if c.Kind == "sandbox" {
		if len(args) == 0 {
			return usage()
		}
		if args[0] == "--help" || args[0] == "-h" {
			c.Help = true
			if len(args) != 1 {
				return usage()
			}
			return c, nil
		}
		c.Name, args = args[0], args[1:]
		if c.Name != "build" && c.Name != "shell" {
			return usage()
		}
	}
	for i, arg := range args {
		if arg == "--" {
			if c.Name != "shell" && i+1 != len(args) {
				return usage()
			}
			c.ShellCommand = args[i+1:]
			break
		}
		switch arg {
		case "-g", "--global":
			if c.Kind != "init" || c.Global {
				return usage()
			}
			c.Global = true
		case "--help", "-h":
			c.Help = true
		case "--exec", "-e":
			if c.Name != "shell" || c.Exec {
				return usage()
			}
			c.Exec = true
		default:
			return usage()
		}
	}
	for _, arg := range c.ShellCommand {
		if strings.ContainsRune(arg, 0) {
			return usage()
		}
	}
	return c, nil
}

func (app *App) runSandboxCommand(ctx context.Context, command Command) (result error) {
	if command.Kind == "init" && command.Global {
		return app.createConfig("config.yaml", globalInitExample)
	}
	cwd, err := app.Getwd()
	if err != nil {
		return err
	}
	g := gitHost{runner: app.Runner}
	if command.Kind == "sandbox" && command.Name == "build" {
		// Only an ordinary non-repository directory may fall back to global config.
		_, probeErr := g.run(ctx, cwd, "rev-parse", "--git-dir")
		var config Config
		if probeErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !gitExit(probeErr, 128) || !strings.Contains(probeErr.Error(), "not a git repository") {
				return probeErr
			}
			config, err = loadGlobalConfig(app.ConfigDir)
		} else {
			var repo gitRepository
			repo, err = g.discover(ctx, cwd)
			if err == nil {
				config, err = LoadConfigForRepo(app.ConfigDir, repo.Root)
			}
		}
		if err != nil {
			return err
		}
		return buildSandbox(ctx, app.Runner, config.Sandbox.Image, app.Stdin, app.Stdout, app.Stderr)
	}
	repo, err := g.discover(ctx, cwd)
	if err != nil {
		return err
	}
	if command.Kind == "init" {
		name, err := repoConfigName(filepath.Base(repo.Root))
		if err != nil {
			return err
		}
		return app.createConfig(name, initExample)
	}
	real, err := canonicalDirectory(cwd)
	if err != nil {
		return err
	}
	if repo.Current == repo.Root || real != repo.Current {
		return fmt.Errorf("sandbox shell requires the root of a linked Git worktree, not the main checkout or a subdirectory")
	}
	store, err := lockState(app.StateDir, repo)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.unlock()) }()
	states, err := store.load()
	if err != nil {
		return err
	}
	state, err := app.resolveWorkspace(ctx, g, repo, states, "")
	if err != nil {
		return err
	}
	if state.Removal != nil || state.PendingMergeCommit != "" {
		return fmt.Errorf("workspace has an unfinished operation; recover it before running sandbox shell")
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return err
	}
	if !command.Exec {
		state.Config, err = LoadConfigForRepo(app.ConfigDir, repo.Root)
		if err != nil {
			return err
		}
	}
	c, ok := app.Sandbox.(interface {
		Shell(context.Context, Workspace, bool, []string, io.Reader, io.Writer, io.Writer) error
		ShellRegistered(context.Context, Workspace, []string, io.Reader, io.Writer, io.Writer, func() error) error
	})
	if !ok {
		return fmt.Errorf("sandbox shell implementation is not configured")
	}
	if !command.Exec {
		// Persist before starting, including for plain Git worktrees. Keep this
		// conservative history after exit so recovery also checks the engine.
		state.SandboxUsed = true
		if err := store.save(state); err != nil {
			return err
		}
		return c.ShellRegistered(ctx, state.Workspace, command.ShellCommand, app.Stdin, app.Stdout, app.Stderr, store.unlock)
	}
	if err := store.unlock(); err != nil {
		return err
	}
	return c.Shell(ctx, state.Workspace, command.Exec, command.ShellCommand, app.Stdin, app.Stdout, app.Stderr)
}

func (app *App) createConfig(name, example string) error {
	if err := privateDirectory(app.ConfigDir); err != nil {
		return err
	}
	path := filepath.Join(app.ConfigDir, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create config without overwriting %s: %w", path, err)
	}
	_, writeErr := io.WriteString(file, example)
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	_, err = fmt.Fprintln(app.Stdout, path)
	return err
}

func buildSandbox(ctx context.Context, runner Runner, image string, stdin io.Reader, stdout, stderr io.Writer) (result error) {
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00") {
		return fmt.Errorf("sandbox image must be an explicit image name")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "cli-workmux-build-")
	if err != nil {
		return fmt.Errorf("create sandbox build context: %w", err)
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	if err := os.WriteFile(filepath.Join(dir, "Containerfile"), sandboxContainerfile, 0600); err != nil {
		return err
	}
	p := sandboxCurrentEngine().process("build", "--tag", image, "--file", filepath.Join(dir, "Containerfile"), dir)
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	_, err = runner.Run(ctx, p)
	if err != nil {
		return fmt.Errorf("build sandbox image: %w", err)
	}
	return nil
}

func (c *Containers) Shell(ctx context.Context, w Workspace, existing bool, words []string, stdin io.Reader, stdout, stderr io.Writer) (result error) {
	return c.shell(ctx, w, existing, words, stdin, stdout, stderr, nil)
}

func (c *Containers) ShellRegistered(ctx context.Context, w Workspace, words []string, stdin io.Reader, stdout, stderr io.Writer, visible func() error) error {
	return c.shell(ctx, w, false, words, stdin, stdout, stderr, visible)
}

func (c *Containers) shell(ctx context.Context, w Workspace, existing bool, words []string, stdin io.Reader, stdout, stderr io.Writer, visible func() error) (result error) {
	command := strings.Join(words, " ")
	if len(words) == 0 {
		command = "bash"
	}
	if strings.ContainsRune(command, 0) {
		return fmt.Errorf("sandbox command contains NUL")
	}
	engine, err := c.sessionEngine(ctx)
	if err != nil {
		return err
	}
	var args []string
	var name string
	if existing {
		if w.Container != "" {
			return fmt.Errorf("legacy persistent sandbox is preserved; recover its data before using sandbox shell")
		}
		found, err := c.ownedSessions(ctx, w, true)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no registered running sandbox for this worktree in the current Podman endpoint")
		}
		selected := found[0]
		if !selected.State.Running {
			return fmt.Errorf("registered sandbox %s is not running", selected.Name)
		}
		if selected.Config.Labels[sandboxLabel+"endpoint"] != engine.Endpoint {
			return fmt.Errorf("registered sandbox %s has a missing or changed endpoint; use its original Podman connection", selected.Name)
		}
		if len(found) > 1 {
			var others []string
			for _, other := range found[1:] {
				others = append(others, other.Name)
			}
			if _, err := fmt.Fprintf(stderr, "Using sandbox %s; other registered containers: %s\n", selected.Name, strings.Join(others, ", ")); err != nil {
				return err
			}
		}
		args = []string{"exec", "-it", selected.ID, "bash", "-c", command}
	} else {
		w.Config.Sandbox.Enabled = true
		args, err = c.runArgs(ctx, engine, w, command, nil, true, true)
		if err != nil {
			return err
		}
		for i, arg := range args {
			if arg == "--name" {
				name = args[i+1]
				break
			}
		}
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			found, err := c.inspect(cleanup, engine, name)
			if err == nil && found != nil {
				labels, labelErr := c.ownerLabels(w)
				err = labelErr
				if err == nil {
					err = sandboxMatchLabels(found, labels)
				}
				if err == nil && found.Config.Labels[sandboxLabel+"endpoint"] != engine.Endpoint {
					err = fmt.Errorf("sandbox endpoint changed during shell cleanup")
				}
				if err == nil && name != "cli-workmux-"+sandboxWorkspaceKey(w)[:16]+"-"+found.Config.Labels[sandboxLabel+"session"] {
					err = fmt.Errorf("sandbox session identity changed during shell cleanup")
				}
				if err == nil {
					err = c.act(cleanup, found, "rm", "--force")
				}
			}
			result = errors.Join(result, err)
		}()
	}
	p := engine.process(args...)
	p.Foreground = true
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	if visible != nil {
		return c.runRegisteredShell(ctx, engine, w, name, p, visible)
	}
	_, err = c.Runner.Run(ctx, p)
	return err
}

func (c *Containers) runRegisteredShell(ctx context.Context, engine sandboxEngine, w Workspace, name string, p Process, visible func() error) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.Runner.Run(runCtx, p); done <- err }()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case err := <-done:
			// A short-lived command can finish before its first inspection.
			return err
		case <-ctx.Done():
			cancel()
			return errors.Join(context.Cause(ctx), <-done)
		case <-timer.C:
			found, err := c.inspect(runCtx, engine, name)
			if err == nil && found != nil && found.State.Running {
				var labels map[string]string
				labels, err = c.ownerLabels(w)
				if err == nil {
					err = sandboxMatchLabels(found, labels)
				}
				if err == nil && (found.Config.Labels[sandboxLabel+"endpoint"] != engine.Endpoint || name != "cli-workmux-"+sandboxWorkspaceKey(w)[:16]+"-"+found.Config.Labels[sandboxLabel+"session"]) {
					err = fmt.Errorf("sandbox registration changed before launch became visible")
				}
				if err == nil {
					err = visible()
				}
				if err == nil {
					return <-done
				}
			}
			if err != nil {
				cancel()
				return errors.Join(err, <-done)
			}
		}
	}
}
