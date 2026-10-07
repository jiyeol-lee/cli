package workmux

import (
	"context"
	"crypto/rand"
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
#   - command: bash
#     split: horizontal
# layouts:
#   review:
#     panes:
#       - command: opencode
#       - command: bash
#         split: vertical
#         percentage: 40
# files:
#   copy: ["<global>", ".env.example"]
#   symlink: ["<global>", ".local-config"]
# Hooks have no defaults. Empty lists disable inherited hooks.
# post_create: []
# pre_merge: []
# pre_remove: []
# To append a user command instead: post_create: ["<global>", "git status --short"]
# sandbox:
#   enabled: true
#   audio: false # Opt in to host PipeWire playback AND microphone access.
#   image: localhost/cli-workmux:fedora44
#   opencode_config_dir: ~/dotfiles/.opencode # Optional real directory, not a symlink; mounted read-only at /tmp/.config/opencode.
`

const globalInitExample = `# Global defaults for cli workmux. Nothing here is enabled by init.
# Repository configuration overrides these defaults field by field.
# panes:
#   - command: opencode
#     focus: true
#   - command: bash
#     split: horizontal
# files:
#   copy: [".env.example"]
#   symlink: [".local-config"]
# Hooks have no defaults. Set a list only for commands you want to run.
# post_create: []
# pre_merge: []
# pre_remove: []
# Optional user command: post_create: ["git status --short"]
# sandbox:
#   enabled: true
#   audio: false # Opt in to host PipeWire playback AND microphone access.
#   image: localhost/cli-workmux:fedora44
#   opencode_config_dir: ~/dotfiles/.opencode # Optional real directory, not a symlink; mounted read-only at /tmp/.config/opencode.
`

func parseSandboxCommand(args []string) (Command, error) {
	c := Command{Kind: args[0]}
	usage := func() (Command, error) {
		return Command{}, fmt.Errorf("usage: cli workmux init [-g|--global] | sandbox build [--no-cache] | sandbox run [-- command...]")
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
		if c.Name != "build" && c.Name != "run" {
			return usage()
		}
	}
	for i, arg := range args {
		if arg == "--" {
			if c.Name != "run" && i+1 != len(args) {
				return usage()
			}
			c.ShellCommand = args[i+1:]
			break
		}
		switch arg {
		case "--no-cache":
			if c.Kind != "sandbox" || c.Name != "build" || c.NoCache {
				return usage()
			}
			c.NoCache = true
		case "-g", "--global":
			if c.Kind != "init" || c.Global {
				return usage()
			}
			c.Global = true
		case "--help", "-h":
			if c.Help {
				return usage()
			}
			c.Help = true
		default:
			return usage()
		}
	}
	for _, arg := range c.ShellCommand {
		if strings.ContainsRune(arg, 0) {
			return usage()
		}
	}
	if c.Name == "run" && !c.Help && len(c.ShellCommand) != 0 && strings.TrimSpace(strings.Join(c.ShellCommand, " ")) == "" {
		return usage()
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
		return buildSandbox(ctx, app.Runner, config.Sandbox.Image, command.NoCache, app.Stdin, app.Stdout, app.Stderr)
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
	config, err := LoadConfigForRepo(app.ConfigDir, repo.Root)
	if err != nil {
		return err
	}
	store, err := waitSandboxState(ctx, app.StateDir, repo)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.unlock()) }()
	states, err := store.load()
	if err != nil {
		return err
	}
	state := workspaceState{Workspace: Workspace{
		ID: identity(repo.CommonDir, repo.Current), RepoID: identity(repo.CommonDir),
		Root: repo.Root, CommonDir: repo.CommonDir, Path: repo.Current,
	}}
	for _, saved := range states {
		if saved.Path == repo.Current && saved.Stage != "removed" {
			if saved.Stage != "ready" && saved.Stage != "closed" && saved.Stage != "merged" {
				return fmt.Errorf("workspace setup is incomplete; recover it before starting a sandbox run")
			}
			if err := checkWorkspaceBranch(repo, saved); err != nil {
				return err
			}
			if _, err := g.source(ctx, repo, saved.Workspace); err != nil {
				return fmt.Errorf("validate sandbox workspace: %w", err)
			}
			state = saved
			break
		}
	}
	if state.Removal != nil || state.PendingMergeCommit != "" {
		return fmt.Errorf("workspace has an unfinished operation; recover it before starting a sandbox run")
	}
	state.Config = config
	c, ok := app.Sandbox.(interface {
		RunStandalone(context.Context, Workspace, []string, io.Reader, io.Writer, io.Writer, func() error) error
	})
	if !ok {
		return fmt.Errorf("sandbox run implementation is not configured")
	}
	return c.RunStandalone(ctx, state.Workspace, command.ShellCommand, app.Stdin, app.Stdout, app.Stderr, store.unlock)
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

func buildSandbox(ctx context.Context, runner Runner, image string, noCache bool, stdin io.Reader, stdout, stderr io.Writer) (result error) {
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
	args := []string{"build"}
	if noCache {
		args = append(args, "--no-cache")
	}
	args = append(args, "--tag", image, "--file", filepath.Join(dir, "Containerfile"), dir)
	p := sandboxCurrentEngine().process(args...)
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	_, err = runner.Run(ctx, p)
	if err != nil {
		return fmt.Errorf("build sandbox image: %w", err)
	}
	return nil
}

func (c *Containers) RunStandalone(ctx context.Context, w Workspace, words []string, stdin io.Reader, stdout, stderr io.Writer, visible func() error) (result error) {
	if len(words) == 0 {
		words = []string{"exec bash -i"}
	}
	w.Standalone, w.SandboxRun = true, rand.Text()
	command := strings.Join(words, " ")
	if strings.ContainsRune(command, 0) {
		return fmt.Errorf("sandbox command contains NUL")
	}
	engine, err := c.sessionEngine(ctx)
	if err != nil {
		return err
	}
	w.Config.Sandbox.Enabled = true
	command = c.seedModelCommand(command, stderr)
	args, err := c.runArgs(ctx, engine, w, command, nil, true, true)
	if err != nil {
		return err
	}
	var name string
	for i, arg := range args {
		if arg == "--name" {
			name = args[i+1]
			break
		}
	}
	registration, err := c.registerStandalone(w, engine)
	if err != nil {
		return err
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
				err = fmt.Errorf("sandbox endpoint changed during run cleanup")
			}
			if err == nil && name != "cli-workmux-"+sandboxWorkspaceKey(w)[:16]+"-"+found.Config.Labels[sandboxLabel+"session"] {
				err = fmt.Errorf("sandbox session identity changed during run cleanup")
			}
			if err == nil {
				err = c.act(cleanup, found, "rm", "--force")
			}
		}
		result = errors.Join(result, err, registration(err == nil))
	}()
	if visible != nil {
		if err := visible(); err != nil {
			return err
		}
	}
	p := engine.process(args...)
	p.Foreground = true
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	_, err = c.Runner.Run(ctx, p)
	return err
}
