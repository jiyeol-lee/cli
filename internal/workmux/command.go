package workmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Usage = `usage: cli workmux <command>

  add <branch> [--base <ref>] [-l|--layout <name>] [-b|--background]
  merge [name] [--into <branch>] [--keep]
  remove [name] [--keep-branch] [--force]
  open [name]
  close [name]

Names may be a branch or its unique slash-to-dash handle. Omitting a name
requires the current directory to be inside a managed linked worktree.
Use -- to end options. Use help [command] or --help for help.

Merge ignores ignored files. Cleanup preserves unknown or changed ignored
files unless remove --force explicitly allows discarding them. Unchanged
ignored files installed by add may be removed with their worktree.

Cleanup closes the owned tmux window before removing the worktree. When
called inside that window, a detached worker finishes cleanup; the command
reports scheduled, not completed. Retry remove after a failed handoff.
Detached/background jobs may survive tmux closure. merge --keep keeps resources.
`

type Command struct {
	Kind                         string
	Name, Base, Layout, Into     string
	Background, Keep, KeepBranch bool
	Force, Help                  bool
}

func commandKind(kind string) bool {
	switch kind {
	case "add", "merge", "remove", "open", "close":
		return true
	}
	return false
}

func ParseCommand(args []string) (Command, error) {
	var command Command
	usage := func(message string) (Command, error) {
		return Command{}, fmt.Errorf("usage: cli workmux <add|merge|remove|open|close> [options]: %s", message)
	}
	if len(args) == 0 || len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		command.Help = true
		return command, nil
	}
	if args[0] == "help" {
		if len(args) > 2 || len(args) == 2 && !commandKind(args[1]) {
			return usage("help accepts at most one command")
		}
		command.Help = true
		if len(args) == 2 {
			command.Kind = args[1]
		}
		return command, nil
	}
	command.Kind = args[0]
	if !commandKind(command.Kind) {
		return usage("unknown command " + command.Kind)
	}
	seen := make(map[string]bool)
	options := true
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if options && arg == "--" {
			options = false
			continue
		}
		if !options || !strings.HasPrefix(arg, "-") {
			if command.Name != "" || arg == "" {
				return usage("exactly one workspace name is allowed")
			}
			command.Name = arg
			continue
		}
		flag, value, equal := strings.Cut(arg, "=")
		switch flag {
		case "-h":
			flag = "--help"
		case "-l":
			flag = "--layout"
		case "-b":
			flag = "--background"
		}
		if seen[flag] {
			return usage("duplicate option " + flag)
		}
		seen[flag] = true
		var destination *string
		switch {
		case flag == "--help":
			command.Help = true
		case command.Kind == "add" && flag == "--base":
			destination = &command.Base
		case command.Kind == "add" && flag == "--layout":
			destination = &command.Layout
		case command.Kind == "add" && flag == "--background":
			command.Background = true
		case command.Kind == "merge" && flag == "--into":
			destination = &command.Into
		case command.Kind == "merge" && flag == "--keep":
			command.Keep = true
		case command.Kind == "remove" && flag == "--keep-branch":
			command.KeepBranch = true
		case command.Kind == "remove" && flag == "--force":
			command.Force = true
		default:
			return usage("option " + flag + " is not valid for " + command.Kind)
		}
		if destination == nil {
			if equal {
				return usage(flag + " does not take a value")
			}
			continue
		}
		if !equal {
			i++
			if i >= len(args) {
				return usage(flag + " requires a value")
			}
			value = args[i]
		}
		if err := safeName(value); err != nil {
			return usage(flag + " requires a non-option value without newlines")
		}
		*destination = value
	}
	if command.Name != "" {
		if err := safeName(command.Name); err != nil {
			return usage("invalid workspace name")
		}
	}
	if command.Kind == "add" && command.Name == "" && !command.Help {
		return usage("add requires a branch")
	}
	return command, nil
}

type App struct {
	Runner            Runner
	Mux               Multiplexer
	Sandbox           Sandbox
	Spawner           CleanupSpawner
	Stdin             io.Reader
	Stdout, Stderr    io.Writer
	Getwd             func() (string, error)
	HomeDir, StateDir string
	ConfigDir         string
	cleanupScheduled  bool
	cleanupTimeout    time.Duration
}

func (app *App) defaults() error {
	if app.Runner == nil {
		app.Runner = ExecRunner{}
	}
	if app.Mux == nil {
		app.Mux = Tmux{Runner: app.Runner}
	}
	if app.Stdout == nil {
		app.Stdout = io.Discard
	}
	if app.Stderr == nil {
		app.Stderr = io.Discard
	}
	if app.Getwd == nil {
		app.Getwd = os.Getwd
	}
	if app.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		app.HomeDir = home
	}
	if !filepath.IsAbs(app.HomeDir) {
		return fmt.Errorf("home directory must be absolute")
	}
	if app.StateDir == "" {
		base := os.Getenv("XDG_STATE_HOME")
		if base == "" {
			base = filepath.Join(app.HomeDir, ".local", "state")
		}
		if !filepath.IsAbs(base) {
			return fmt.Errorf("XDG_STATE_HOME must be absolute")
		}
		app.StateDir = filepath.Join(base, "cli", "workmux")
	}
	if app.ConfigDir == "" {
		app.ConfigDir = filepath.Join(app.HomeDir, ".config", "cli", "workmux")
	}
	return nil
}

func (app App) Run(ctx context.Context, command Command) (resultErr error) {
	app.cleanupScheduled = false
	if command.Help {
		if app.Stdout != nil {
			_, err := io.WriteString(app.Stdout, Usage)
			return err
		}
		return nil
	}
	if !commandKind(command.Kind) {
		return fmt.Errorf("usage: cli workmux <add|merge|remove|open|close>")
	}
	if err := app.defaults(); err != nil {
		return err
	}
	cwd, err := app.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	g := gitHost{runner: app.Runner}
	repo, err := g.discover(ctx, cwd)
	if err != nil {
		return err
	}
	store, err := lockState(app.StateDir, repo)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.unlock(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release repository lock: %w", err))
		}
	}()
	states, err := store.load()
	if err != nil {
		return fmt.Errorf("load workspace state: %w", err)
	}
	if command.Kind == "add" {
		return app.add(ctx, g, repo, store, states, command)
	}
	state, err := selectWorkspace(states, repo.Current, command.Name)
	if err != nil {
		return err
	}
	if _, err := g.branchName(ctx, repo.Root, state.Branch); err != nil {
		return err
	}
	closeWindow := false
	switch command.Kind {
	case "open":
		err = app.open(ctx, g, repo, store, &state)
	case "close":
		err = app.close(ctx, g, repo, store, &state)
		closeWindow = err == nil
	case "merge":
		err = app.merge(ctx, g, repo, store, &state, command)
	case "remove":
		err = app.remove(ctx, g, repo, store, &state, command, false)
	}
	if err != nil {
		return fmt.Errorf("%s workspace %q (stage %s); work and recovery state preserved: %w", command.Kind, state.Handle, state.Stage, err)
	}
	if app.cleanupScheduled {
		return nil
	}
	if closeWindow {
		// Killing the caller's window can kill this process. No writes or locks may remain.
		if err := store.unlock(); err != nil {
			return fmt.Errorf("release repository lock: %w", err)
		}
		if err := app.Mux.Close(ctx, state.Workspace); err != nil {
			return fmt.Errorf("retry close %s to close the owned tmux window: %w", state.Handle, err)
		}
	}
	return nil
}

func selectWorkspace(states []workspaceState, current, name string) (workspaceState, error) {
	var matches []workspaceState
	for _, state := range states {
		if name != "" && (state.Branch == name || state.Handle == name) || name == "" && state.Path == current {
			matches = append(matches, state)
		}
	}
	if len(matches) == 0 {
		if name == "" {
			return workspaceState{}, fmt.Errorf("a workspace name is required outside a managed linked worktree")
		}
		return workspaceState{}, fmt.Errorf("no managed workspace named %q in this repository", name)
	}
	if len(matches) != 1 {
		return workspaceState{}, fmt.Errorf("workspace name %q is ambiguous", name)
	}
	return matches[0], nil
}

func (app *App) sandboxCheck(ctx context.Context, config SandboxConfig) error {
	if !config.Enabled {
		return nil
	}
	if app.Sandbox == nil {
		return fmt.Errorf("sandbox is enabled but no sandbox implementation is available")
	}
	return app.Sandbox.Check(ctx, config)
}

func (app *App) add(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, states []workspaceState, command Command) error {
	handle, err := g.branchName(ctx, repo.Root, command.Name)
	if err != nil {
		return err
	}
	if !safeRelative(handle) || strings.ContainsAny(handle, "/\\") {
		return fmt.Errorf("branch does not produce a safe workspace handle")
	}
	path := workspacePath(repo.Root, handle)
	for _, state := range states {
		if state.Handle == handle || state.Branch == command.Name {
			if state.Stage != "removed" {
				return fmt.Errorf("workspace %q already exists at stage %s; use open, or remove to recover a partial add", handle, state.Stage)
			}
			if state.Branch != command.Name {
				return fmt.Errorf("branch slug collides with saved workspace %q", state.Branch)
			}
			if window, err := app.Mux.Find(ctx, state.Workspace); err != nil {
				return err
			} else if window != "" {
				return fmt.Errorf("removed workspace still has a tmux window; retry close %s first", handle)
			}
		}
	}
	for _, tree := range repo.Worktrees {
		if tree.Path == path || tree.Branch == command.Name {
			return fmt.Errorf("branch or destination is already registered as a worktree: %s", tree.Path)
		}
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("workspace destination already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	config, err := LoadConfig(app.ConfigDir, filepath.Base(repo.Root))
	if err != nil {
		return err
	}
	if err := config.Validate(); err != nil {
		return err
	}
	panes, err := config.SelectPanes(command.Layout)
	if err != nil {
		return err
	}
	session, err := app.Mux.Session(ctx)
	if err != nil {
		return err
	}
	if err := app.sandboxCheck(ctx, config.Sandbox); err != nil {
		return err
	}
	oid, err := g.branchOID(ctx, repo, command.Name)
	if err != nil {
		return err
	}
	created := oid == ""
	if !created && command.Base != "" {
		return fmt.Errorf("--base is only valid for a new branch; existing branch %q is reused unchanged", command.Name)
	}
	if created {
		base := command.Base
		if base == "" {
			base = "HEAD"
		}
		oid, err = g.resolve(ctx, repo.Current, base)
		if err != nil {
			return err
		}
	}
	state := workspaceState{Workspace: Workspace{
		ID: identity(repo.CommonDir, path), RepoID: identity(repo.CommonDir), Root: repo.Root,
		CommonDir: repo.CommonDir, Path: path, Branch: command.Name, Handle: handle,
		Layout: command.Layout, Stage: "planned", CreatedBranch: created, Config: config,
	}, InitialCommit: oid}
	if config.Sandbox.Enabled {
		state.Container = "cli-workmux-" + state.ID
	}
	if err := store.save(state); err != nil {
		return err
	}
	fail := func(err error) error {
		return fmt.Errorf("add workspace %q stopped at stage %s; files and state preserved at %s; use open if setup completed, or remove %s to recover: %w", handle, state.Stage, path, handle, err)
	}
	if err := repo.validateMain(); err != nil {
		return fail(err)
	}
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return fail(err)
	}
	args := []string{"worktree", "add"}
	if created {
		args = append(args, "-b", command.Name, "--", path, oid)
	} else {
		args = append(args, "--", path, command.Name)
	}
	if _, err := g.run(ctx, repo.Root, args...); err != nil {
		return fail(err)
	}
	state.Stage = "worktree"
	if err := store.save(state); err != nil {
		return fail(err)
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return fail(err)
	}
	before, err := ignoredFiles(ctx, g, path)
	if err != nil {
		return fail(err)
	}
	if err := ApplyFiles(ctx, repo.Root, path, config.Files, app.Stderr); err != nil {
		return fail(err)
	}
	after, err := ignoredFiles(ctx, g, path)
	if err != nil {
		return fail(err)
	}
	state.OwnedIgnored = make(map[string]string)
	for name, hash := range after {
		if _, exists := before[name]; !exists {
			state.OwnedIgnored[name] = hash
		}
	}
	state.Stage = "files"
	if err := store.save(state); err != nil {
		return fail(err)
	}
	if config.Sandbox.Enabled {
		if err := app.Sandbox.Ensure(ctx, state.Workspace); err != nil {
			return fail(err)
		}
	}
	state.Stage = "sandbox"
	if err := store.save(state); err != nil {
		return fail(err)
	}
	if err := app.hooks(ctx, state.Workspace, "post_create", config.PostCreate); err != nil {
		return fail(err)
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return fail(err)
	}
	state.Stage = "ready"
	if err := store.save(state); err != nil {
		return fail(err)
	}
	if err := app.createWindow(ctx, store, &state, session, panes); err != nil {
		return fail(err)
	}
	if _, err := fmt.Fprintln(app.Stdout, state.Path); err != nil {
		return err
	}
	if !command.Background {
		return app.Mux.Focus(ctx, state.Workspace, state.Window)
	}
	return nil
}

func (app *App) createWindow(ctx context.Context, store *stateStore, state *workspaceState, session string, panes []Pane) error {
	commands, err := app.paneCommands(state.Workspace, panes)
	if err != nil {
		return err
	}
	server, err := app.Mux.Server(ctx)
	if err != nil {
		return err
	}
	if state.Socket != "" && state.Socket != server {
		return fmt.Errorf("workspace belongs to another tmux server; reopen it from that server")
	}
	state.Socket = server
	if source, ok := app.Mux.(interface {
		CaptureServer(context.Context, string) (CleanupWindow, error)
	}); ok {
		captured, err := source.CaptureServer(ctx, server)
		if err != nil {
			return err
		}
		state.rememberServer(captured)
	}
	if err := store.save(*state); err != nil {
		return err
	}
	window, createErr := app.Mux.Create(ctx, session, state.Workspace, panes, commands)
	if createErr == nil && !tmuxID(window, '@') {
		return fmt.Errorf("multiplexer returned an invalid window ID")
	}
	if window != "" {
		state.Window = window
		if err := store.save(*state); err != nil {
			return fmt.Errorf("save tmux window identity: %w", err)
		}
	}
	if createErr != nil {
		return fmt.Errorf("create tmux panes; retry close then open to replace partial panes: %w", createErr)
	}
	return nil
}

func (app *App) savedSandbox(state workspaceState) error {
	latest, err := LoadConfig(app.ConfigDir, filepath.Base(state.Root))
	if err != nil {
		return err
	}
	if latest.Sandbox != state.Config.Sandbox {
		return fmt.Errorf("sandbox configuration changed since add; restore the saved enabled setting and image before open (remove still uses the saved configuration)")
	}
	return nil
}

func (app *App) open(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	if state.Stage != "ready" && state.Stage != "closed" && state.Stage != "merged" {
		return fmt.Errorf("cannot open incomplete or removed workspace; remove it to recover without rerunning hooks or files")
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return err
	}
	session, err := app.Mux.Session(ctx)
	if err != nil {
		return err
	}
	if err := app.savedSandbox(*state); err != nil {
		return err
	}
	window, err := app.Mux.Find(ctx, state.Workspace)
	if err != nil {
		return err
	}
	if state.Stage == "closed" && window != "" {
		return fmt.Errorf("close has not finished; retry close before open")
	}
	if window != "" {
		return app.Mux.Focus(ctx, state.Workspace, window)
	}
	if err := app.sandboxCheck(ctx, state.Config.Sandbox); err != nil {
		return err
	}
	if state.Config.Sandbox.Enabled {
		if err := app.Sandbox.Ensure(ctx, state.Workspace); err != nil {
			return err
		}
	}
	panes, err := state.Config.SelectPanes(state.Layout)
	if err != nil {
		return err
	}
	state.Stage = "ready"
	if err := store.save(*state); err != nil {
		return err
	}
	if err := app.createWindow(ctx, store, state, session, panes); err != nil {
		return err
	}
	return app.Mux.Focus(ctx, state.Workspace, state.Window)
}

func (app *App) close(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	if state.Stage == "removed" {
		return nil
	}
	if state.Removal != nil && (state.Config.Sandbox.Enabled || state.Removal.WorktreeRemoved) {
		if state.Removal.Job == nil || !cleanupActive(state.Removal.Job) {
			return fmt.Errorf("removal is incomplete; retry remove before closing its recovery window")
		}
	}
	if state.Removal != nil && state.Removal.Job != nil {
		state.Removal.Job.Status = "cancelled"
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return err
	}
	if state.Config.Sandbox.Enabled {
		if app.Sandbox == nil {
			return fmt.Errorf("saved workspace requires its sandbox implementation")
		}
		if err := app.Sandbox.Stop(ctx, state.Workspace); err != nil {
			return err
		}
	}
	if state.Stage == "ready" || state.Stage == "merged" || state.Stage == "closed" {
		state.Stage = "closed"
	}
	return store.save(*state)
}

func (app *App) merge(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, command Command) error {
	if state.Stage == "removed" {
		if state.MergedCommit == "" {
			return fmt.Errorf("workspace was removed without a recorded merge")
		}
		if command.Into != "" && command.Into != state.MergeTarget {
			return fmt.Errorf("target differs from the recorded merge")
		}
		return nil
	}
	if state.Removal != nil {
		if state.MergedCommit == "" || command.Keep || state.Removal.Head != state.MergedCommit {
			return fmt.Errorf("cleanup is already in progress; retry remove")
		}
		if command.Into != "" && command.Into != state.MergeTarget {
			return fmt.Errorf("target differs from the recorded merge; cleanup is already in progress")
		}
		return mergeCleanupError(app.remove(ctx, g, repo, store, state, Command{Kind: "remove"}, true))
	}
	if state.Stage != "ready" && state.Stage != "closed" && state.Stage != "merged" && state.Stage != "merging" {
		return fmt.Errorf("workspace setup is incomplete; remove it to recover")
	}
	source, err := g.source(ctx, repo, state.Workspace)
	if err != nil {
		return err
	}
	into := command.Into
	if into == "" && state.MergedCommit != "" {
		into = state.MergeTarget
	}
	if into == "" && state.PendingMergeCommit != "" {
		into = state.PendingMergeTarget
	}
	target, err := g.target(ctx, repo, into, state.Branch, true)
	if err != nil {
		return err
	}
	if err := g.clean(ctx, repo, source, false); err != nil {
		return err
	}
	if err := g.clean(ctx, repo, target, false); err != nil {
		return err
	}
	alreadyMerged := false
	if state.PendingMergeCommit != "" && (state.PendingMergeCommit != source.Head || state.PendingMergeTarget != target.Branch) {
		return fmt.Errorf("source or target changed during an unfinished merge attempt; refusing to guess")
	}
	if state.MergedCommit != "" {
		if source.Head != state.MergedCommit || target.Branch != state.MergeTarget {
			if !state.MergeKept {
				return fmt.Errorf("source or target changed after the recorded merge; refusing automatic cleanup")
			}
		} else {
			if err := g.ancestor(ctx, repo, source.Head, target.Head); err != nil {
				return err
			}
			alreadyMerged = true
		}
	} else if state.PendingMergeCommit == source.Head && state.PendingMergeTarget == target.Branch {
		alreadyMerged = g.ancestor(ctx, repo, source.Head, target.Head) == nil
	}
	if !alreadyMerged {
		state.MergeTarget = target.Branch
		if err := app.hooks(ctx, state.Workspace, "pre_merge", state.Config.PreMerge); err != nil {
			return err
		}
		checkedSource, err := g.source(ctx, repo, state.Workspace)
		if err != nil {
			return err
		}
		checkedTarget, err := g.target(ctx, repo, target.Branch, state.Branch, true)
		if err != nil {
			return err
		}
		if source.Head != checkedSource.Head || target.Head != checkedTarget.Head || target.Path != checkedTarget.Path {
			return fmt.Errorf("source or target ref changed during pre_merge; refusing merge")
		}
		if err := g.clean(ctx, repo, checkedSource, false); err != nil {
			return err
		}
		if err := g.clean(ctx, repo, checkedTarget, false); err != nil {
			return err
		}
		state.PendingMergeCommit, state.PendingMergeTarget = source.Head, target.Branch
		state.MergedCommit, state.MergeKept = "", false
		state.Stage = "merging"
		if err := store.save(*state); err != nil {
			return err
		}
		if _, err := g.run(ctx, target.Path, "merge", "--no-edit", "--no-autostash", "--", source.Head); err != nil {
			return fmt.Errorf("merge failed; resolve or abort the operation in %s, then retry; no resources were removed: %w", target.Path, err)
		}
	}
	completed, err := g.target(ctx, repo, target.Branch, state.Branch, true)
	if err != nil {
		return err
	}
	if err := g.clean(ctx, repo, completed, false); err != nil {
		return fmt.Errorf("merge did not leave a completed clean target: %w", err)
	}
	if err := g.ancestor(ctx, repo, source.Head, completed.Head); err != nil {
		return err
	}
	state.MergeTarget, state.MergedCommit = target.Branch, source.Head
	state.PendingMergeCommit, state.PendingMergeTarget = "", ""
	state.MergeKept = command.Keep
	state.Stage = "merged"
	if err := store.save(*state); err != nil {
		if command.Keep {
			return fmt.Errorf("merge succeeded; record completion: %w", err)
		}
		return mergeCleanupError(fmt.Errorf("record merge completion: %w", err))
	}
	if command.Keep {
		return nil
	}
	return mergeCleanupError(app.remove(ctx, g, repo, store, state, Command{Kind: "remove"}, true))
}

func mergeCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("merge succeeded; cleanup incomplete: %w", err)
}

func (app *App) removalCheck(ctx context.Context, g gitHost, repo gitRepository, state workspaceState, source gitWorktree, force, keepBranch bool) (string, error) {
	if err := g.clean(ctx, repo, source, force); err != nil {
		return "", err
	}
	if !force {
		ignored, err := ignoredFiles(ctx, g, source.Path)
		if err != nil {
			return "", err
		}
		for name, hash := range ignored {
			if state.OwnedIgnored[name] != hash {
				return "", fmt.Errorf("unknown or changed ignored file %q would be lost; move it out, or use remove --force to discard it", name)
			}
		}
	}
	if keepBranch {
		return "", nil
	}
	into := state.MergeTarget
	if state.Removal != nil && state.Removal.Target != "" {
		into = state.Removal.Target
	}
	target, err := g.target(ctx, repo, into, state.Branch, false)
	if err != nil {
		if force {
			return "", nil
		}
		return "", err
	}
	if !force {
		if err := g.ancestor(ctx, repo, source.Head, target.Head); err != nil {
			return "", err
		}
	}
	return target.Branch, nil
}

func (app *App) remove(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, command Command, merged bool) error {
	if state.Stage == "removed" {
		return nil
	}
	if state.Removal != nil && cleanupActive(state.Removal.Job) {
		return fmt.Errorf("cleanup is already scheduled; inspect %s and retry remove after it finishes or its handoff expires", cleanupLogPath(store, state.ID, state.Removal.Job.Token))
	}
	if state.Config.Sandbox.Enabled && app.Sandbox == nil {
		return fmt.Errorf("saved workspace requires its sandbox implementation")
	}
	if err := repo.validateMain(); err != nil {
		return err
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return err
	}
	present := false
	for _, tree := range trees {
		if tree.Path == state.Path {
			present = true
		}
	}
	if !present {
		if _, err := os.Lstat(state.Path); err == nil {
			return fmt.Errorf("unregistered data exists at the workspace path; refusing to delete it")
		} else if !os.IsNotExist(err) {
			return err
		}
		if state.Removal == nil && state.Stage != "planned" {
			return fmt.Errorf("worktree disappeared outside recorded cleanup; refusing to guess ownership")
		}
		if state.Removal != nil && !state.Removal.WorktreeRemoved && !state.Removal.WorktreeRemovalStarted {
			return fmt.Errorf("worktree disappeared before the recorded Git removal step; refusing to guess whether cleanup succeeded")
		}
	}
	if present && state.Removal != nil && state.Removal.WorktreeRemoved {
		return fmt.Errorf("worktree reappeared after recorded removal; refusing to act on replacement data")
	}
	if state.Removal == nil {
		removal := &removalState{KeepBranch: command.KeepBranch, Force: command.Force && !merged}
		if present {
			source, err := g.source(ctx, repo, state.Workspace)
			if err != nil {
				return err
			}
			if merged && source.Head != state.MergedCommit {
				return fmt.Errorf("source changed after merge; refusing cleanup")
			}
			removal.Head = source.Head
			removal.Identity, err = captureCleanupIdentity(repo, source)
			if err != nil {
				return err
			}
			removal.Target, err = app.removalCheck(ctx, g, repo, *state, source, removal.Force, removal.KeepBranch)
			if err != nil {
				return err
			}
		} else {
			removal.Head = state.InitialCommit
			removal.HookDone, removal.WorktreeRemoved = true, true
			removal.KeepBranch = command.KeepBranch || !state.CreatedBranch
		}
		state.Removal = removal
		state.MergeKept = false
		state.Stage = "removing"
		if err := store.save(*state); err != nil {
			return err
		}
	} else {
		if state.Removal.Job != nil {
			state.Removal.Job.Status = "cancelled"
		}
		if command.KeepBranch && !state.Removal.BranchDeleted {
			state.Removal.KeepBranch = true
		}
		if command.Force && !merged {
			state.Removal.Force = true
		}
		if err := store.save(*state); err != nil {
			return err
		}
	}
	removal := state.Removal
	if !present {
		removal.WorktreeRemoved = true
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if present && !removal.WorktreeRemoved {
		source, err := g.source(ctx, repo, state.Workspace)
		if err != nil {
			return err
		}
		if removal.Identity == nil {
			removal.Identity, err = captureCleanupIdentity(repo, source)
			if err != nil {
				return err
			}
		}
		if err := verifyCleanupIdentity(state.Workspace, removal.Identity, false); err != nil {
			return err
		}
		if source.Head != removal.Head {
			if !command.KeepBranch || merged || removal.BranchDeleted {
				return fmt.Errorf("source ref changed after cleanup began; retry remove --keep-branch to preserve the new commit and revalidate cleanup")
			}
			if _, err := app.removalCheck(ctx, g, repo, *state, source, removal.Force, true); err != nil {
				return err
			}
			removal.Head, removal.Target = source.Head, ""
			removal.Job = nil
			removal.HookDone, removal.Stopped, removal.WorktreeRemovalStarted = false, false, false
			if err := store.save(*state); err != nil {
				return err
			}
		}
		if _, err := app.removalCheck(ctx, g, repo, *state, source, removal.Force, removal.KeepBranch); err != nil {
			return err
		}
		if !removal.HookDone {
			if err := app.hooks(ctx, state.Workspace, "pre_remove", state.Config.PreRemove); err != nil {
				return err
			}
			removal.HookDone = true
			if err := store.save(*state); err != nil {
				return err
			}
		}
		source, err = g.source(ctx, repo, state.Workspace)
		if err != nil {
			return err
		}
		if source.Head != removal.Head {
			return fmt.Errorf("source ref changed during pre_remove or shutdown; refusing deletion")
		}
		if _, err := app.removalCheck(ctx, g, repo, *state, source, removal.Force, removal.KeepBranch); err != nil {
			return err
		}
	}
	return app.cleanup(ctx, g, repo, store, state, merged)
}

func (app *App) deleteBranch(ctx context.Context, g gitHost, repo gitRepository, state workspaceState) error {
	removal := state.Removal
	if err := repo.validateMain(); err != nil {
		return err
	}
	current, err := g.branchOID(ctx, repo, state.Branch)
	if err != nil || current == "" {
		return err
	}
	if current != removal.Head {
		return fmt.Errorf("branch changed after cleanup began; refusing to delete its new commit")
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return err
	}
	for _, tree := range trees {
		if tree.Branch == state.Branch {
			return fmt.Errorf("branch is now checked out at %s; refusing deletion", tree.Path)
		}
	}
	if !removal.Force {
		target, err := g.target(ctx, repo, removal.Target, state.Branch, false)
		if err != nil {
			return err
		}
		if err := g.ancestor(ctx, repo, current, target.Head); err != nil {
			return err
		}
	}
	_, err = g.run(ctx, repo.Root, "update-ref", "--no-deref", "-d", "refs/heads/"+state.Branch, removal.Head)
	return err
}
