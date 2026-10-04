package workmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const Usage = `usage: cli workmux <command>

  init [-g|--global]
  sandbox build [--no-cache]
  sandbox run [-- command...]
  add <branch> [--base <ref>] [-l|--layout <name>]
  merge [name] [--into <branch>]
  remove|rm [name...]
  open <name...>
  close [name]

Names may be a branch or its unique worktree handle. Omitting a name
requires the current directory to be inside a linked worktree. Open requires
a name. Plain Git worktrees are discovered automatically.
Use -- to end options. Use help [command] or --help for help.

Dirty source changes require confirmation and are permanently discarded before
merge or removal. Ignored files are removed with the worktree during cleanup.

init creates repository overrides; init -g creates global defaults outside Git too.
Examples: cli workmux init; cli workmux init --global
Sandbox run opens interactive Bash by default; use -- opencode to launch OpenCode.

Cleanup closes the owned tmux window before removing the worktree. When
called inside that window, a detached worker finishes cleanup; the command
reports scheduled, not completed. Retry remove after a failed handoff.
Detached/background jobs may survive tmux closure.
`

type Command struct {
	Global, NoCache          bool
	ShellCommand             []string
	Kind                     string
	Name, Base, Layout, Into string
	Help                     bool
	Names                    []string
}

func commandKind(kind string) bool {
	switch kind {
	case "add", "merge", "remove", "open", "close", "init", "sandbox":
		return true
	}
	return false
}

func ParseCommand(args []string) (Command, error) {
	if len(args) > 0 && (args[0] == "init" || args[0] == "sandbox") {
		return parseSandboxCommand(args)
	}
	var command Command
	usage := func(message string) (Command, error) {
		return Command{}, fmt.Errorf("usage: cli workmux <init|sandbox|add|merge|remove|open|close> [options]: %s", message)
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
	if command.Kind == "rm" {
		command.Kind = "remove"
	}
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
			if (command.Name != "" && command.Kind != "open" && command.Kind != "remove") || arg == "" {
				return usage("exactly one workspace name is allowed")
			}
			if command.Name == "" {
				command.Name = arg
			}
			command.Names = append(command.Names, arg)
			continue
		}
		flag, value, equal := strings.Cut(arg, "=")
		switch flag {
		case "-h":
			flag = "--help"
		case "-l":
			flag = "--layout"
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
		case command.Kind == "merge" && flag == "--into":
			destination = &command.Into
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
	for _, name := range command.Names {
		if err := safeName(name); err != nil {
			return usage("invalid workspace name")
		}
	}
	if (command.Kind == "add" || command.Kind == "open") && command.Name == "" && !command.Help {
		return usage(command.Kind + " requires a name")
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
	createdWindow     string
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

var errDiscardCancelled = fmt.Errorf("workspace operation cancelled")

func (app App) Run(ctx context.Context, command Command) error {
	err := app.run(ctx, command)
	if errors.Is(err, errDiscardCancelled) {
		return nil
	}
	return err
}

func (app App) run(ctx context.Context, command Command) (resultErr error) {
	if len(command.Names) > 1 {
		names := command.Names
		if command.Kind == "remove" {
			if err := app.defaults(); err != nil {
				return err
			}
			cwd, err := app.Getwd()
			if err != nil {
				return err
			}
			repo, err := (gitHost{runner: app.Runner}).discover(ctx, cwd)
			if err != nil {
				return err
			}
			var first, current []string
			seen := make(map[string]bool)
			for _, name := range names {
				if seen[name] {
					continue
				}
				seen[name] = true
				caller := name == "." && repo.Current != repo.Root
				for _, tree := range repo.Worktrees {
					if tree.Path == repo.Current && (tree.Branch == name || workspaceSlug(tree.Branch) == name || filepath.Base(tree.Path) == name) {
						caller = true
					}
				}
				if caller {
					current = append(current, name)
				} else {
					first = append(first, name)
				}
			}
			names = append(first, current...)
		}
		for _, name := range names {
			single := command
			single.Name, single.Names = name, nil
			if err := app.run(ctx, single); err != nil {
				return err
			}
		}
		return nil
	}
	app.cleanupScheduled = false
	if command.Help {
		if app.Stdout != nil {
			_, err := io.WriteString(app.Stdout, Usage)
			return err
		}
		return nil
	}
	if !commandKind(command.Kind) {
		return fmt.Errorf("usage: cli workmux <init|sandbox|add|merge|remove|open|close>")
	}
	if (command.Kind == "add" || command.Kind == "open") && command.Name == "" {
		return fmt.Errorf("usage: cli workmux %s requires a name", command.Kind)
	}
	if err := app.defaults(); err != nil {
		return err
	}
	if command.Kind == "init" || command.Kind == "sandbox" {
		return app.runSandboxCommand(ctx, command)
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
			if resultErr == errDiscardCancelled {
				resultErr = nil
			}
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
	state, err := app.resolveWorkspace(ctx, g, repo, states, command.Name)
	if err != nil {
		return err
	}
	if state.Removal == nil && state.PendingMergeCommit == "" && state.Stage != "removed" {
		state.Config, err = LoadConfigForRepo(app.ConfigDir, repo.Root)
		if err != nil {
			return err
		}
	}
	if command.Kind == "open" {
		if _, err := state.Config.SelectPanes(""); err != nil {
			return err
		}
	}
	if _, err := g.branchName(ctx, repo.Root, state.Branch); err != nil {
		return err
	}
	if command.Kind != "merge" && command.Kind != "remove" && state.Removal == nil {
		if err := app.adoptWindow(ctx, g, repo, store, &state); err != nil {
			return err
		}
	}
	var closeWindow CleanupWindow
	primaryWindow := state.Window
	switch command.Kind {
	case "open":
		err = app.open(ctx, g, repo, store, &state, "")
	case "close":
		if command.Name == "" {
			if mux, ok := app.Mux.(interface {
				CurrentWindow(context.Context, Workspace) (string, error)
			}); ok {
				state.Window, err = mux.CurrentWindow(ctx, state.Workspace)
				if err != nil {
					return err
				}
			}
		}
		if window, findErr := app.Mux.Find(ctx, state.Workspace); findErr != nil {
			return findErr
		} else if window == "" {
			return fmt.Errorf("workspace has no open tmux window")
		}
		err = app.close(ctx, g, repo, store, &state)
		if err == nil {
			var token [16]byte
			if _, err = rand.Read(token[:]); err == nil {
				closeWindow, err = app.Mux.Capture(ctx, state.Workspace, hex.EncodeToString(token[:]))
			}
			if err == nil {
				state.rememberServer(closeWindow)
				if command.Name == "" && primaryWindow != "" && primaryWindow != closeWindow.ID {
					state.Window = primaryWindow
				}
				err = store.save(state)
			}
		}
	case "merge":
		err = app.merge(ctx, g, repo, store, &state, command)
	case "remove":
		err = app.remove(ctx, g, repo, store, &state, command, false)
	}
	if err != nil {
		if errors.Is(err, errDiscardCancelled) {
			return errDiscardCancelled
		}
		return fmt.Errorf("%s workspace %q (stage %s); work and recovery state preserved: %w", command.Kind, state.Handle, state.Stage, err)
	}
	if app.cleanupScheduled {
		return nil
	}
	if closeWindow.ID != "" {
		// Killing the caller's window can kill this process. No writes or locks may remain.
		if err := store.unlock(); err != nil {
			return fmt.Errorf("release repository lock: %w", err)
		}
		if err := app.Mux.CloseCaptured(ctx, state.Workspace, closeWindow); err != nil {
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
	if len(matches) > 1 {
		var active []workspaceState
		for _, state := range matches {
			if state.Stage != "removed" {
				active = append(active, state)
			}
		}
		if len(active) != 0 {
			matches = active
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

func (app *App) resolveWorkspace(ctx context.Context, g gitHost, repo gitRepository, states []workspaceState, name string) (workspaceState, error) {
	state, err := app.resolveWorkspaceState(ctx, g, repo, states, name)
	if err == nil {
		err = checkWorkspaceBranch(repo, state)
	}
	if err == nil {
		err = rememberSandbox(app.StateDir, &state)
	}
	return state, err
}

func checkWorkspaceBranch(repo gitRepository, state workspaceState) error {
	if state.Stage == "removed" {
		return nil
	}
	for _, tree := range repo.Worktrees {
		if tree.Path != state.Path || tree.Branch == state.Branch {
			continue
		}
		actual := tree.Branch
		if actual == "" {
			actual = "detached HEAD"
		}
		return fmt.Errorf("workspace %q records branch %q but its worktree is on %q; restore the recorded branch %q before retrying", state.Handle, state.Branch, actual, state.Branch)
	}
	return nil
}

func (app *App) resolveWorkspaceState(ctx context.Context, g gitHost, repo gitRepository, states []workspaceState, name string) (workspaceState, error) {
	if name == "." {
		name = ""
	}
	for _, state := range states {
		if (state.Removal != nil && state.Stage != "removed" || state.PendingMergeCommit != "") && (name != "" && (name == state.Branch || name == state.Handle) || name == "" && state.Path == repo.Current) {
			return state, nil
		}
	}
	var matches []gitWorktree
	var branches []gitWorktree
	for _, tree := range repo.Worktrees {
		if tree.Bare || tree.Path == repo.Root {
			continue
		}
		if name == "" && tree.Path == repo.Current || name != "" && (tree.Branch == name || workspaceSlug(tree.Branch) == name || filepath.Base(tree.Path) == name) {
			matches = append(matches, tree)
			if tree.Branch == name {
				branches = append(branches, tree)
			}
		}
	}
	if len(branches) != 0 {
		matches = branches
	}
	if len(matches) > 1 {
		return workspaceState{}, fmt.Errorf("workspace name %q is ambiguous", name)
	}
	if len(matches) == 0 {
		return selectWorkspace(states, repo.Current, name)
	}
	tree := matches[0]
	for _, state := range states {
		if state.Path != tree.Path {
			continue
		}
		if state.Stage == "removed" {
			continue
		}
		return state, nil
	}
	if err := repo.validateTree(tree); err != nil {
		return workspaceState{}, err
	}
	base := ""
	out, err := g.run(ctx, repo.Root, "config", "--get", "branch."+tree.Branch+".workmux-base")
	if err == nil {
		candidate := strings.TrimSpace(string(out))
		if safeComparisonRef(candidate, tree.Branch) == nil {
			base = candidate
		}
	} else if !gitExit(err, 1) {
		return workspaceState{}, err
	}
	return workspaceState{Workspace: Workspace{
		ID: identity(repo.CommonDir, tree.Path), RepoID: identity(repo.CommonDir), Root: repo.Root,
		CommonDir: repo.CommonDir, Path: tree.Path, Branch: tree.Branch, BaseRef: base,
		Handle: workspaceSlug(filepath.Base(tree.Path)), Stage: "ready",
	}, InitialCommit: tree.Head}, nil
}

func (app *App) add(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, states []workspaceState, command Command) error {
	config, err := LoadConfigForRepo(app.ConfigDir, repo.Root)
	if err != nil {
		return err
	}
	handle, err := g.branchName(ctx, repo.Root, command.Name)
	if err != nil {
		return err
	}
	for _, tree := range repo.Worktrees {
		if tree.Branch == command.Name {
			if tree.Path == repo.Root {
				return fmt.Errorf("main checkout is not a workspace")
			}
			if tree.Prunable {
				continue
			}
			state, err := app.resolveWorkspace(ctx, g, repo, states, command.Name)
			if err != nil {
				return err
			}
			state.Config = config
			return app.open(ctx, g, repo, store, &state, command.Layout)
		}
	}
	if !safeRelative(handle) || strings.ContainsAny(handle, "/\\") {
		return fmt.Errorf("branch does not produce a safe workspace handle")
	}
	panes, err := config.SelectPanes(command.Layout)
	if err != nil {
		return err
	}
	path := workspacePath(repo.Root, handle)
	for _, state := range states {
		if state.Handle == handle || state.Branch == command.Name {
			if state.Stage != "removed" {
				if err := checkWorkspaceBranch(repo, state); err != nil {
					return err
				}
				if err := app.reconcileAdd(ctx, g, repo, store, &state); err != nil {
					return err
				}
			}
			orphan := state.Removal != nil && state.Removal.Recovery != nil
			if state.Branch != command.Name && !orphan {
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
	session, err := app.Mux.Session(ctx)
	if err != nil {
		return err
	}
	oid, err := g.branchOID(ctx, repo, command.Name)
	if err != nil {
		return err
	}
	created := oid == ""
	base := ""
	if created {
		base = strings.TrimPrefix(command.Base, "refs/heads/")
		if base == "" {
			for _, tree := range repo.Worktrees {
				if tree.Path == repo.Current {
					base = tree.Branch
				}
			}
			if base == "" {
				return fmt.Errorf("add from detached HEAD requires --base")
			}
		}
		if err := safeComparisonRef(base, command.Name); err != nil {
			return err
		}
		ref := command.Base
		if ref == "" {
			ref = "refs/heads/" + base
		}
		oid, err = g.resolve(ctx, repo.Current, ref)
		if err != nil {
			return err
		}
	}
	state := workspaceState{Workspace: Workspace{
		ID: identity(repo.CommonDir, path), RepoID: identity(repo.CommonDir), Root: repo.Root,
		CommonDir: repo.CommonDir, Path: path, Branch: command.Name, BaseRef: base, Handle: handle,
		Layout: command.Layout, Stage: "planned", CreatedBranch: created, Config: config,
	}, InitialCommit: oid}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return err
	}
	for _, tree := range trees {
		if tree.Path == path || tree.Branch == command.Name {
			return fmt.Errorf("branch or destination is already registered as a worktree: %s", tree.Path)
		}
	}
	if err := missingWorkspacePath(path); err != nil {
		return err
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
	if err := ApplyFiles(ctx, repo.Root, path, config.Files, app.Stderr); err != nil {
		return fail(err)
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return fail(err)
	}
	state.Stage = "files"
	if err := store.save(state); err != nil {
		return fail(err)
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
	return app.focusCreated(ctx, state.Workspace)
}

func (app *App) adoptWindow(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	if state.Window != "" || state.Socket != "" || state.Stage == "removed" {
		return nil
	}
	mux, ok := app.Mux.(interface {
		Adopt(context.Context, Workspace) (string, error)
	})
	if !ok {
		return nil
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return nil
	}
	window, err := mux.Adopt(ctx, state.Workspace)
	if err != nil || window == "" {
		return err
	}
	state.Window = window
	state.Socket, err = app.Mux.Server(ctx)
	if err != nil {
		return err
	}
	if capture, ok := app.Mux.(interface {
		CaptureServer(context.Context, string) (CleanupWindow, error)
	}); ok {
		server, err := capture.CaptureServer(ctx, state.Socket)
		if err != nil {
			return err
		}
		state.rememberServer(server)
	}
	return store.save(*state)
}

func (app *App) createWindow(ctx context.Context, store *stateStore, state *workspaceState, session string, panes []Pane) error {
	commands, err := app.paneCommands(ctx, state.Workspace, panes)
	if err != nil {
		return err
	}
	for _, command := range commands {
		if len(command) > 1 {
			state.SandboxUsed = true
		}
	}
	server, err := app.Mux.Server(ctx)
	if err != nil {
		return err
	}
	if state.ServerPID != 0 {
		gone, err := tmuxServerExited(state.ServerPID)
		if err != nil {
			return err
		}
		if gone {
			state.Socket, state.Window = "", ""
			state.ServerPID, state.SocketDevice, state.SocketInode = 0, 0, 0
		}
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
		app.createdWindow = window
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

func (app *App) focusCreated(ctx context.Context, w Workspace) error {
	if err := app.Mux.Focus(ctx, w, app.createdWindow); err != nil {
		if window, findErr := app.Mux.Find(ctx, w); findErr == nil && window == "" {
			return nil
		}
		return err
	}
	return nil
}

func (app *App) open(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, layout string) error {
	panes, err := state.Config.SelectPanes(layout)
	if err != nil {
		return err
	}
	if state.Stage != "ready" && state.Stage != "closed" && state.Stage != "merged" {
		return fmt.Errorf("cannot open incomplete or removed workspace; remove it to recover without rerunning hooks or files")
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return fmt.Errorf("cannot open workspace; inspect and restore the recorded worktree before retrying: %w", err)
	}
	session, err := app.Mux.Session(ctx)
	if err != nil {
		return err
	}
	window, err := app.Mux.Find(ctx, state.Workspace)
	if err != nil {
		return err
	}
	state.Window = window
	if window != "" {
		return app.Mux.Focus(ctx, state.Workspace, window)
	}
	state.Stage = "ready"
	if err := store.save(*state); err != nil {
		return err
	}
	if err := app.createWindow(ctx, store, state, session, panes); err != nil {
		return err
	}
	return app.focusCreated(ctx, state.Workspace)
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
	presence, err := g.workspacePresence(ctx, repo, state.Workspace)
	if err != nil {
		return err
	}
	if !presence.Directory {
		if _, err := app.Mux.Find(ctx, state.Workspace); err != nil {
			return err
		}
	}
	if workspaceHasSandbox(state.Workspace) && (presence.Directory || expectedContainer(state.Workspace)) {
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
	if err := app.checkStandalone(ctx, state.Workspace); err != nil {
		return err
	}
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
		if state.MergedCommit == "" || state.Removal.Head != state.MergedCommit {
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
	if err := app.checkMergeAttempt(ctx, g, repo, store, state, source); err != nil {
		return err
	}
	into := command.Into
	recorded := state.MergedCommit != "" && !state.MergeKept
	if recorded {
		if source.Head != state.MergedCommit || into != "" && into != state.MergeTarget {
			return fmt.Errorf("source or target changed after the recorded merge; refusing automatic cleanup")
		}
		into = state.MergeTarget
	}
	if into == "" {
		into, err = g.localBase(ctx, repo, state.BaseRef, state.Branch)
		if err != nil {
			return err
		}
	}
	target, err := g.target(ctx, repo, into, state.Branch, false)
	if err != nil {
		return err
	}
	proof := source.Head
	if recorded && state.MergeStrategy == "squash" {
		proof = state.MergeResult
	}
	checkAncestry := func(checkCtx context.Context, checkedTarget gitWorktree) (bool, error) {
		if recorded {
			return true, g.ancestor(checkCtx, repo, proof, checkedTarget.Head)
		}
		return g.fastForward(checkCtx, repo, source, checkedTarget)
	}
	alreadyMerged, err := checkAncestry(ctx, target)
	if err != nil {
		return err
	}
	sourceIdentity, err := captureGitIdentity(repo, source)
	if err != nil {
		return err
	}
	var targetIdentity map[string]fileIdentity
	if target.Path != "" {
		if err := g.checkClean(ctx, repo, target, false, false); err != nil {
			return err
		}
		targetIdentity, err = captureGitIdentity(repo, target)
		if err != nil {
			return err
		}
	} else if !alreadyMerged {
		trees, err := g.worktrees(ctx, repo.Root)
		if err != nil {
			return err
		}
		if len(trees) == 0 || trees[0].Path != repo.Root {
			return fmt.Errorf("main worktree identity changed")
		}
		if err := g.checkClean(ctx, repo, trees[0], false, false); err != nil {
			return err
		}
	}
	checkAttempt := func(allowDirtySource bool) error {
		if err := verifyGitIdentity(sourceIdentity); err != nil {
			return err
		}
		if err := verifyGitIdentity(targetIdentity); err != nil {
			return err
		}
		checkedSource, err := g.source(ctx, repo, state.Workspace)
		if err != nil {
			return err
		}
		checkedTarget, err := g.target(ctx, repo, target.Branch, state.Branch, false)
		if err != nil {
			return err
		}
		if _, err := checkAncestry(ctx, checkedTarget); err != nil {
			return err
		}
		if source.Head != checkedSource.Head || target.Head != checkedTarget.Head || target.Path != checkedTarget.Path {
			return fmt.Errorf("source or target changed during merge preflight; refusing merge")
		}
		if err := g.clean(ctx, repo, checkedSource, allowDirtySource); err != nil {
			return err
		}
		if checkedTarget.Path != "" {
			if err := g.checkClean(ctx, repo, checkedTarget, false, false); err != nil {
				return err
			}
		}
		return nil
	}
	approved, err := app.discardSource(ctx, g, repo, state.Workspace, source, func() error { return checkAttempt(true) })
	if err != nil || !approved {
		return err
	}
	if err := checkAttempt(false); err != nil {
		return err
	}
	state.MergeTarget = target.Branch
	if !recorded && !alreadyMerged {
		if err := app.hooks(ctx, state.Workspace, "pre_merge", state.Config.PreMerge); err != nil {
			return err
		}
		if err := checkAttempt(false); err != nil {
			return err
		}
		target, err = g.placeTarget(ctx, repo, target, state.Branch, func() error { return checkAttempt(false) })
		if err != nil {
			return err
		}
		targetIdentity, err = captureGitIdentity(repo, target)
		if err != nil {
			return err
		}
		if err := checkAttempt(false); err != nil {
			return err
		}
		if err := g.protectIgnored(ctx, target, source.Head); err != nil {
			return err
		}
		state.PendingMergeCommit, state.PendingMergeTarget = source.Head, target.Branch
		state.PendingMergeHead, state.PendingMergePath = target.Head, target.Path
		state.RetryMergeTarget = ""
		state.MergedCommit, state.MergeKept = "", false
		state.MergeResult, state.PendingSquashTree = "", ""
		state.PendingConflict = false
		state.MergeStrategy = ""
		state.Stage = "merging"
		if err := store.save(*state); err != nil {
			return err
		}
		if err := checkAttempt(false); err != nil {
			return errors.Join(err, app.clearMergeAttempt(store, state))
		}
		// config-env handles '=' in branch names. A standard explicit strategy
		// also prevents configured ours/subtree strategies from disabling FF-only.
		gitErr := app.interactiveGit(ctx, target.Path, "--config-env=branch."+target.Branch+".mergeOptions=WORKMUX_EMPTY_MERGE_OPTIONS", "merge", "--ff-only", "--strategy=recursive", "--no-squash", "--no-edit", "--no-autostash", "--no-overwrite-ignore", "--", source.Head)
		// Git can report failure after advancing the target. Check the result even
		// when the caller cancelled, and never roll back committed integration.
		verifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		completed, completionErr := g.target(verifyCtx, repo, target.Branch, state.Branch, true)
		if completionErr == nil && completed.Path != target.Path {
			completionErr = fmt.Errorf("target worktree changed during merge")
		}
		if completionErr == nil {
			completionErr = verifyGitIdentity(targetIdentity)
		}
		if completionErr == nil {
			completionErr = g.ancestor(verifyCtx, repo, source.Head, completed.Head)
		}
		if completionErr != nil {
			if completed.Path == target.Path && completed.Head == target.Head && verifyGitIdentity(targetIdentity) == nil && g.checkClean(verifyCtx, repo, completed, false, false) == nil {
				completionErr = errors.Join(completionErr, app.clearMergeAttempt(store, state))
			}
			return errors.Join(fmt.Errorf("merge did not complete; inspect %s and manually complete or abort any unfinished Git operation before retrying", target.Path), gitErr, completionErr)
		}
		if err := app.recordMerge(store, state, source, completed); err != nil {
			return mergeCleanupError(errors.Join(gitErr, err))
		}
		if gitErr != nil {
			return mergeCleanupError(fmt.Errorf("Git reported an error after verified integration; retry merge or remove to finish cleanup: %w", gitErr))
		}
		if err := verifyGitIdentity(sourceIdentity); err != nil {
			return mergeCleanupError(err)
		}
		if err := g.checkClean(ctx, repo, completed, false, false); err != nil {
			return mergeCleanupError(err)
		}
		return mergeCleanupError(app.remove(ctx, g, repo, store, state, Command{Kind: "remove"}, true))
	}
	if !recorded {
		state.MergeStrategy = ""
	}
	return app.finishMerge(ctx, g, repo, store, state, source, target)
}

func (app *App) finishMerge(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, source, completed gitWorktree) error {
	if completed.Path != "" {
		if err := g.checkClean(ctx, repo, completed, false, false); err != nil {
			return err
		}
	}
	if err := app.recordMerge(store, state, source, completed); err != nil {
		return mergeCleanupError(err)
	}
	return mergeCleanupError(app.remove(ctx, g, repo, store, state, Command{Kind: "remove"}, true))
}

func (app *App) recordMerge(store *stateStore, state *workspaceState, source, completed gitWorktree) error {
	state.MergeResult = completed.Head
	state.PendingSquashTree = ""
	state.PendingConflict = false
	state.MergeTarget, state.MergedCommit = completed.Branch, source.Head
	state.PendingMergeCommit, state.PendingMergeTarget = "", ""
	state.PendingMergeHead, state.PendingMergePath, state.RetryMergeTarget = "", "", ""
	state.MergeKept = false
	state.Stage = "merged"
	if err := store.save(*state); err != nil {
		return fmt.Errorf("record merge completion: %w", err)
	}
	return nil
}

func (app *App) checkMergeAttempt(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, source gitWorktree) error {
	if err := g.clean(ctx, repo, source, true); err != nil {
		return fmt.Errorf("manually complete or abort the unfinished Git operation in %s before retrying merge: %w", source.Path, err)
	}
	if state.PendingMergeCommit != "" && state.MergeStrategy == "squash" {
		if err := g.clean(ctx, repo, source, false); err != nil {
			return fmt.Errorf("previous squash has dirty source work in %s; manually complete or abort it before retrying merge: %w", source.Path, err)
		}
	}
	if state.PendingMergeCommit != "" || state.PendingMergePath != "" || state.PendingMergeTarget != "" || state.RetryMergeTarget != "" {
		// Check old locations for unfinished work, but never resume their operation.
		trees, err := g.worktrees(ctx, repo.Root)
		if err != nil {
			return err
		}
		for _, tree := range trees {
			matches := state.PendingMergePath != "" && tree.Path == state.PendingMergePath || state.PendingMergeTarget != "" && tree.Branch == state.PendingMergeTarget || state.RetryMergeTarget != "" && tree.Branch == state.RetryMergeTarget
			if !matches {
				continue
			}
			if err := g.checkClean(ctx, repo, tree, false, false); err != nil {
				return fmt.Errorf("previous merge left unfinished Git work or a dirty result in %s; manually complete or abort it before retrying merge: %w", tree.Path, err)
			}
		}
	}
	if state.PendingMergeCommit != "" {
		completed, err := g.completedMergeAttempt(ctx, repo, *state)
		if err != nil {
			return fmt.Errorf("cannot verify previous merge outcome; inspect it manually; recovery evidence preserved: %w", err)
		}
		if completed.Branch != "" {
			integrated := source
			integrated.Head = state.PendingMergeCommit
			state.MergeStrategy = ""
			return app.recordMerge(store, state, integrated, completed)
		}
	}
	if state.PendingMergeCommit != "" || state.PendingMergeTarget != "" || state.PendingMergePath != "" || state.RetryMergeTarget != "" || state.Stage == "merging" {
		return app.clearMergeAttempt(store, state)
	}
	return nil
}

func (app *App) clearMergeAttempt(store *stateStore, state *workspaceState) error {
	config, err := LoadConfigForRepo(app.ConfigDir, state.Root)
	if err != nil {
		return err
	}
	state.Config = config
	state.PendingMergeCommit, state.PendingMergeTarget, state.PendingMergeHead, state.PendingMergePath = "", "", "", ""
	state.RetryMergeTarget, state.PendingSquashTree = "", ""
	if state.MergedCommit == "" || state.MergeKept {
		state.MergeStrategy, state.MergeResult = "", ""
		state.Stage = "ready"
	} else {
		state.Stage = "merged"
	}
	state.PendingConflict = false
	return store.save(*state)
}

func mergeCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("merge succeeded; cleanup incomplete: %w", err)
}

func removalComparison(ctx context.Context, g gitHost, repo gitRepository, removal *removalState) (gitComparison, error) {
	ref := removal.Target
	if ref == "" {
		return gitComparison{}, fmt.Errorf("cleanup lacks a recorded comparison ref")
	}
	if removal.TargetOID == "" && !strings.HasPrefix(ref, "refs/") && !validOID(ref) {
		// Before comparison OIDs were recorded, an unqualified target meant a local branch.
		if _, err := g.branchName(ctx, repo.Root, ref); err != nil {
			return gitComparison{}, err
		}
		ref = "refs/heads/" + ref
	}
	comparison, err := g.comparison(ctx, repo.Root, ref)
	if err != nil {
		return comparison, err
	}
	if removal.TargetOID != "" && comparison.Commit != removal.TargetOID || removal.TargetRefOID != "" && (comparison.RefOID != removal.TargetRefOID || comparison.Ref != removal.Target) {
		return comparison, fmt.Errorf("comparison ref changed after cleanup approval; work preserved")
	}
	return comparison, nil
}

func pinRemovalTarget(ctx context.Context, g gitHost, repo gitRepository, removal *removalState) error {
	comparison, err := removalComparison(ctx, g, repo, removal)
	if err != nil {
		return err
	}
	removal.Target, removal.TargetOID, removal.TargetRefOID = comparison.Ref, comparison.Commit, comparison.RefOID
	return nil
}

func (app *App) removalCheck(ctx context.Context, g gitHost, repo gitRepository, state workspaceState, source gitWorktree) (string, error) {
	if err := g.clean(ctx, repo, source, false); err != nil {
		return "", err
	}
	if state.Removal != nil && state.Removal.MergeResult != "" {
		if source.Head != state.Removal.Head {
			return "", fmt.Errorf("source changed after successful merge")
		}
		comparison, err := removalComparison(ctx, g, repo, state.Removal)
		if err != nil {
			return "", err
		}
		if err := g.ancestor(ctx, repo, state.Removal.MergeResult, comparison.Commit); err != nil {
			return "", err
		}
		return comparison.Ref, nil
	}
	return "", nil
}

func (app *App) confirmDiscard(ctx context.Context, source string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(app.Stdout, "Workspace %q has staged, unstaged, or untracked changes. They will be permanently discarded before this operation, even if it fails.\nDiscard these changes? [y/N] ", source); err != nil {
		return false, err
	}
	if writer, ok := app.Stdout.(interface{ Flush() error }); ok {
		if err := writer.Flush(); err != nil {
			return false, err
		}
	}
	line := ""
	if app.Stdin != nil {
		for {
			var one [1]byte
			_, err := io.ReadFull(app.Stdin, one[:])
			if err == io.EOF {
				line = ""
				break
			}
			if err != nil {
				return false, fmt.Errorf("read discard confirmation: %w", err)
			}
			if one[0] == '\n' {
				break
			}
			line += string(one[:])
			if len(line) > 4096 {
				return false, fmt.Errorf("discard confirmation exceeds 4096 bytes")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(line), "y") {
		return true, nil
	}
	_, err := fmt.Fprintln(app.Stdout, "Aborted")
	if err != nil {
		return false, err
	}
	if writer, ok := app.Stdout.(interface{ Flush() error }); ok {
		if err := writer.Flush(); err != nil {
			return false, err
		}
	}
	return false, errDiscardCancelled
}

func (app *App) remove(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, command Command, merged bool) error {
	if err := app.checkStandalone(ctx, state.Workspace); err != nil {
		return err
	}
	if state.Stage == "removed" {
		return nil
	}
	if state.Removal != nil && state.Removal.LegacyKeepBranch {
		return fmt.Errorf("unfinished legacy branch-preserving cleanup; recover it with the previous workmux implementation before retrying")
	}
	if state.Removal != nil && cleanupActive(state.Removal.Job) {
		return fmt.Errorf("cleanup is already scheduled; inspect %s and retry remove after it finishes or its handoff expires", cleanupLogPath(store, state.ID, state.Removal.Job.Token))
	}
	presence, err := g.workspacePresence(ctx, repo, state.Workspace)
	if err != nil {
		return err
	}
	if state.Removal != nil && state.Removal.Recovery != nil {
		return app.recoverOrphan(ctx, g, repo, store, state, presence)
	}
	normalRemoval := state.Removal != nil && (state.Removal.WorktreeRemovalStarted || state.Removal.WorktreeRemoved)
	if !presence.Directory && !normalRemoval {
		return app.recoverOrphan(ctx, g, repo, store, state, presence)
	}
	if workspaceHasSandbox(state.Workspace) && app.Sandbox == nil {
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
	newRemoval := state.Removal == nil
	if newRemoval {
		removal := &removalState{}
		if present {
			source, err := g.source(ctx, repo, state.Workspace)
			if err != nil {
				return err
			}
			if state.PendingMergeCommit != "" {
				if err := app.checkMergeAttempt(ctx, g, repo, store, state, source); err != nil {
					return err
				}
			}
			recordedCleanup := state.MergedCommit != "" && !state.MergeKept
			if (merged || recordedCleanup) && source.Head != state.MergedCommit {
				return fmt.Errorf("source changed after merge; refusing cleanup")
			}
			removal.Head = source.Head
			if merged || recordedCleanup {
				result := state.MergeResult
				if result == "" && state.MergeStrategy != "squash" {
					result = state.MergedCommit
				}
				if !validOID(result) {
					return fmt.Errorf("recorded merge lacks its integration proof; refusing cleanup")
				}
				removal.MergeResult = result
				removal.Target = "refs/heads/" + state.MergeTarget
				if err := pinRemovalTarget(ctx, g, repo, removal); err != nil {
					return err
				}
				if merged && removal.TargetOID != result {
					return fmt.Errorf("target changed after successful merge")
				}
				if err := g.ancestor(ctx, repo, result, removal.TargetOID); err != nil {
					return err
				}
			}
			removal.Identity, err = captureCleanupIdentity(repo, source)
			if err != nil {
				return err
			}
			if merged {
				if err := g.clean(ctx, repo, source, false); err != nil {
					return err
				}
			} else {
				approved, err := app.discardSource(ctx, g, repo, state.Workspace, source)
				if err != nil || !approved {
					return err
				}
			}
			checked, err := g.source(ctx, repo, state.Workspace)
			if err != nil {
				return err
			}
			if checked.Head != removal.Head {
				return fmt.Errorf("source ref changed during removal confirmation")
			}
			pending := *state
			pending.Removal = removal
			if _, err := app.removalCheck(ctx, g, repo, pending, checked); err != nil {
				return err
			}
		} else {
			removal.Head = state.InitialCommit
			removal.HookDone, removal.WorktreeRemoved = true, true
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
		if state.Removal.MergeResult != "" {
			if err := pinRemovalTarget(ctx, g, repo, state.Removal); err != nil {
				return err
			}
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
			return fmt.Errorf("source ref changed after cleanup began; work preserved; inspect the journal before recovering with the recorded original HEAD")
		}
		if !merged && !newRemoval {
			approved, err := app.discardSource(ctx, g, repo, state.Workspace, source)
			if err != nil || !approved {
				return err
			}
		}
		if _, err := app.removalCheck(ctx, g, repo, *state, source); err != nil {
			return err
		}
		if err := app.adoptWindow(ctx, g, repo, store, state); err != nil {
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
		if _, err := app.removalCheck(ctx, g, repo, *state, source); err != nil {
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
	transaction := "start\x00"
	if removal.MergeResult != "" {
		comparison, err := removalComparison(ctx, g, repo, removal)
		if err != nil {
			return err
		}
		if err := g.ancestor(ctx, repo, removal.MergeResult, comparison.Commit); err != nil {
			return err
		}
		if comparison.RefOID != "" {
			if err := safeComparisonRef(comparison.Ref, state.Branch); err != nil {
				return err
			}
			transaction += "verify " + comparison.Ref + "\x00" + comparison.RefOID + "\x00"
		}
	}
	transaction += "delete refs/heads/" + state.Branch + "\x00" + removal.Head + "\x00prepare\x00commit\x00"
	_, err = g.runInput(ctx, repo.Root, strings.NewReader(transaction), "update-ref", "--no-deref", "--stdin", "-z")
	return err
}
