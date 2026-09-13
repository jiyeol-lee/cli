package workmux

import (
	"bufio"
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
  sandbox build
  sandbox shell [-e|--exec] [-- command...]
  add <branch> [--base <ref>] [-l|--layout <name>] [-b|--background]
      [-o|--open-if-exists] [--name <handle>] [--dry-run]
      [-H|--no-hooks] [-F|--no-file-ops] [-C|--no-pane-cmds]
  merge [name] [--into <branch>] [-k|--keep|--cleanup] [--rebase|--squash]
      [--ignore-uncommitted] [--no-verify] [-H|--no-hooks]
  remove|rm [name...] [-k|--keep-branch] [-f|--force]
  open <name...> [--run-hooks] [--force-files] [-n|--new]
  close [name]

Names may be a branch or its unique worktree handle. Omitting a name
requires the current directory to be inside a linked worktree. Open requires
a name unless --new is given. Plain Git worktrees are discovered automatically.
Use -- to end options. Use help [command] or --help for help.

Git ignores ignored files when checking for uncommitted changes.

init creates repository overrides; init -g creates global defaults outside Git too.
Examples: cli workmux init; cli workmux init --global

Cleanup closes the owned tmux window before removing the worktree. When
called inside that window, a detached worker finishes cleanup; the command
reports scheduled, not completed. Retry remove after a failed handoff.
Detached/background jobs may survive tmux closure. merge --keep keeps resources.
`

type Command struct {
	Global                                            bool
	Exec                                              bool
	ShellCommand                                      []string
	Kind                                              string
	Name, Base, Layout, Into                          string
	Background, Keep, KeepBranch                      bool
	Force, Help                                       bool
	Names                                             []string
	Handle                                            string
	OpenIfExists, NoHooks, NoFileOps, NoPaneCmds      bool
	RunHooks, ForceFiles, IgnoreUncommitted, NoVerify bool
	New                                               bool
	DryRun                                            bool
	Rebase, Squash, Cleanup                           bool
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
		case "-b":
			flag = "--background"
		case "-o":
			flag = "--open-if-exists"
		case "-n":
			flag = "--new"
		case "-H":
			flag = "--no-hooks"
		case "-F":
			flag = "--no-file-ops"
		case "-C":
			flag = "--no-pane-cmds"
		case "-f":
			flag = "--force"
		case "-k":
			flag = "--keep"
			if command.Kind == "remove" {
				flag = "--keep-branch"
			}
		}
		if seen[flag] {
			return usage("duplicate option " + flag)
		}
		seen[flag] = true
		var destination *string
		switch {
		case flag == "--help":
			command.Help = true
		case command.Kind == "add" && flag == "--name":
			destination = &command.Handle
		case command.Kind == "add" && flag == "--dry-run":
			command.DryRun = true
		case command.Kind == "add" && flag == "--open-if-exists":
			command.OpenIfExists = true
		case (command.Kind == "add" || command.Kind == "merge") && flag == "--no-hooks":
			command.NoHooks = true
		case command.Kind == "add" && flag == "--no-file-ops":
			command.NoFileOps = true
		case command.Kind == "add" && flag == "--no-pane-cmds":
			command.NoPaneCmds = true
		case command.Kind == "open" && flag == "--run-hooks":
			command.RunHooks = true
		case command.Kind == "open" && flag == "--new":
			command.New = true
		case command.Kind == "open" && flag == "--force-files":
			command.ForceFiles = true
		case command.Kind == "merge" && flag == "--ignore-uncommitted":
			command.IgnoreUncommitted = true
		case command.Kind == "merge" && flag == "--no-verify":
			command.NoVerify = true
		case command.Kind == "merge" && flag == "--rebase":
			command.Rebase = true
		case command.Kind == "merge" && flag == "--squash":
			command.Squash = true
		case command.Kind == "merge" && flag == "--cleanup":
			command.Cleanup = true
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
	for _, name := range command.Names {
		if err := safeName(name); err != nil {
			return usage("invalid workspace name")
		}
	}
	if (command.Kind == "add" || command.Kind == "open" && !command.New) && command.Name == "" && !command.Help {
		return usage(command.Kind + " requires a name")
	}
	if command.Rebase && command.Squash {
		return usage("--rebase and --squash are mutually exclusive")
	}
	if command.Keep && command.Cleanup {
		return usage("--keep and --cleanup are mutually exclusive")
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

func (app App) Run(ctx context.Context, command Command) (resultErr error) {
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
			if err := app.Run(ctx, single); err != nil {
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
	if command.DryRun {
		handle, err := g.branchName(ctx, repo.Root, command.Name)
		if err != nil {
			return err
		}
		if command.Handle != "" {
			handle = workspaceSlug(command.Handle)
		}
		if !safeRelative(handle) || strings.ContainsAny(handle, "/\\") {
			return fmt.Errorf("invalid workspace handle")
		}
		config, err := LoadConfigForRepo(app.ConfigDir, repo.Root)
		if err != nil {
			return err
		}
		panes, err := config.SelectPanes(command.Layout)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(app.Stdout, "Would add branch %s at %s with %d panes. File operations: %t; hooks: %t; pane commands: %t.\n", command.Name, workspacePath(repo.Root, handle), len(panes), !command.NoFileOps, !command.NoHooks, !command.NoPaneCmds)
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
	if command.NoHooks && state.Removal == nil {
		state.Config.PostCreate, state.Config.PreMerge, state.Config.PreRemove = nil, nil, nil
	}
	if command.Kind == "open" {
		if _, err := state.Config.SelectPanes(""); err != nil {
			return err
		}
	}
	if _, err := g.branchName(ctx, repo.Root, state.Branch); err != nil {
		return err
	}
	if state.Window == "" && state.Socket == "" && state.Removal == nil && state.Stage != "removed" {
		if mux, ok := app.Mux.(interface {
			Adopt(context.Context, Workspace) (string, error)
		}); ok {
			if _, err := g.source(ctx, repo, state.Workspace); err == nil {
				state.Window, err = mux.Adopt(ctx, state.Workspace)
				if err != nil {
					return err
				}
				if state.Window != "" {
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
					if err := store.save(state); err != nil {
						return err
					}
				}
			}
		}
	}
	var closeWindow CleanupWindow
	primaryWindow := state.Window
	switch command.Kind {
	case "open":
		state.NewWindow = command.New
		if command.ForceFiles || command.RunHooks {
			if state.Removal != nil || state.PendingMergeCommit != "" {
				return fmt.Errorf("cannot reprovision during an unfinished operation")
			}
			if _, err := g.source(ctx, repo, state.Workspace); err != nil {
				return err
			}
		}
		if command.ForceFiles {
			if err := ApplyFiles(ctx, repo.Root, state.Path, state.Config.Files, app.Stderr); err != nil {
				return err
			}
		}
		if command.RunHooks {
			if err := app.hooks(ctx, state.Workspace, "post_create", state.Config.PostCreate); err != nil {
				return err
			}
		}
		if (command.ForceFiles || command.RunHooks) && (state.Stage == "worktree" || state.Stage == "files" || state.Stage == "sandbox") {
			state.Stage = "ready"
		}
		err = app.open(ctx, g, repo, store, &state)
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
		err = rememberSandbox(app.StateDir, &state)
	}
	return state, err
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
		if state.Removal == nil && state.PendingMergeCommit == "" {
			state.Branch = tree.Branch
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
	handle, err := g.branchName(ctx, repo.Root, command.Name)
	if err != nil {
		return err
	}
	if command.Handle != "" {
		handle = workspaceSlug(command.Handle)
	}
	if command.OpenIfExists {
		for _, tree := range repo.Worktrees {
			if tree.Branch == command.Name {
				state, err := app.resolveWorkspace(ctx, g, repo, states, command.Name)
				if err != nil {
					return err
				}
				state.Config, err = LoadConfigForRepo(app.ConfigDir, repo.Root)
				if err != nil {
					return err
				}
				return app.open(ctx, g, repo, store, &state)
			}
		}
	}
	if !safeRelative(handle) || strings.ContainsAny(handle, "/\\") {
		return fmt.Errorf("branch does not produce a safe workspace handle")
	}
	path := workspacePath(repo.Root, handle)
	var kept *workspaceState
	for _, state := range states {
		if state.Handle == handle || state.Branch == command.Name {
			if state.Stage != "removed" {
				if err := app.reconcileAdd(ctx, g, repo, store, &state); err != nil {
					return err
				}
			}
			orphan := state.Removal != nil && state.Removal.Recovery != nil
			if state.Branch != command.Name && !orphan {
				return fmt.Errorf("branch slug collides with saved workspace %q", state.Branch)
			}
			if state.Removal != nil && state.Removal.KeepBranch && !orphan {
				kept = &state
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
	config, err := LoadConfigForRepo(app.ConfigDir, repo.Root)
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
	if command.NoHooks {
		config.PostCreate, config.PreMerge, config.PreRemove = nil, nil, nil
	}
	if command.NoPaneCmds {
		for i := range panes {
			panes[i].Command = ""
		}
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
	} else if kept != nil && kept.BaseRef != "" && validOID(kept.Removal.Head) {
		persisted, err := g.isAncestor(ctx, repo, kept.Removal.Head, oid)
		if err != nil {
			return err
		}
		if persisted {
			base = kept.BaseRef
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
	if !command.NoFileOps {
		if err := ApplyFiles(ctx, repo.Root, path, config.Files, app.Stderr); err != nil {
			return fail(err)
		}
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
	if !command.Background {
		return app.focusCreated(ctx, state.Workspace)
	}
	return nil
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
		if !state.NewWindow || state.Window == "" {
			state.Window = window
		}
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

func (app *App) open(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	panes, err := state.Config.SelectPanes("")
	if err != nil {
		return err
	}
	if state.Stage != "ready" && state.Stage != "closed" && state.Stage != "merged" {
		return fmt.Errorf("cannot open incomplete or removed workspace; remove it to recover without rerunning hooks or files")
	}
	if _, err := g.source(ctx, repo, state.Workspace); err != nil {
		return fmt.Errorf("cannot restore a missing or changed worktree with open; use remove %s then add %s to recover: %w", state.Handle, state.Branch, err)
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
	if window != "" && !state.NewWindow {
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
	if state.PendingSquashTree != "" {
		handled, err := app.resumeSquash(ctx, g, repo, store, state, source, command)
		if handled || err != nil {
			return err
		}
	}
	if state.PendingConflict && state.PendingMergeHead != "" && state.PendingSquashTree == "" {
		target, err := g.target(ctx, repo, state.PendingMergeTarget, state.Branch, true)
		if err != nil {
			return err
		}
		if target.Path == state.PendingMergePath && target.Head == state.PendingMergeHead && g.checkClean(ctx, repo, target, false, false) == nil {
			if err := app.clearMergeAttempt(store, state, command); err != nil {
				return err
			}
		}
	}
	into := command.Into
	if state.PendingMergeCommit != "" {
		if source.Head != state.PendingMergeCommit || into != "" && into != state.PendingMergeTarget {
			return fmt.Errorf("source or target changed during an unfinished merge attempt; refusing to guess")
		}
		into = state.PendingMergeTarget
	}
	if into == "" && state.MergedCommit != "" && !state.MergeKept {
		into = state.MergeTarget
	}
	if into == "" {
		into = state.RetryMergeTarget
	}
	if into == "" {
		into, err = g.localBase(ctx, repo, state.BaseRef, state.Branch)
		if err != nil {
			return err
		}
	}
	if state.MergedCommit != "" && !state.MergeKept && (source.Head != state.MergedCommit || into != state.MergeTarget) {
		return fmt.Errorf("source or target changed after the recorded merge; refusing automatic cleanup")
	}
	source, err = app.prepareMergeSource(ctx, g, repo, source, command)
	if err != nil {
		return err
	}
	target, err := g.target(ctx, repo, into, state.Branch, state.PendingMergeCommit != "")
	if err != nil {
		return err
	}
	if state.PendingMergePath != "" && target.Path != state.PendingMergePath {
		return fmt.Errorf("target worktree changed during an unfinished merge attempt")
	}
	target, err = g.placeTarget(ctx, repo, target, state.Branch)
	if err != nil {
		return err
	}
	checkedSource, err := g.source(ctx, repo, state.Workspace)
	if err != nil {
		return err
	}
	if checkedSource.Head != source.Head {
		return fmt.Errorf("source changed during target placement")
	}
	if err := g.checkClean(ctx, repo, target, false, false); err != nil {
		return err
	}
	alreadyMerged := false
	if state.PendingMergeCommit != "" && (state.PendingMergeCommit != source.Head || state.PendingMergeTarget != target.Branch) {
		return fmt.Errorf("source or target changed during an unfinished merge attempt; refusing to guess")
	}
	if state.MergedCommit != "" && !state.MergeKept {
		if source.Head != state.MergedCommit || target.Branch != state.MergeTarget {
			return fmt.Errorf("source or target changed after the recorded merge; refusing automatic cleanup")
		}
		proof := source.Head
		if state.MergeStrategy == "squash" {
			proof = state.MergeResult
		}
		if err := g.ancestor(ctx, repo, proof, target.Head); err != nil {
			return err
		}
		alreadyMerged = true
	} else if state.PendingMergeCommit == source.Head && state.PendingMergeTarget == target.Branch {
		alreadyMerged, err = g.isAncestor(ctx, repo, source.Head, target.Head)
		if err != nil {
			return err
		}
		if !alreadyMerged && state.PendingMergeHead != "" && state.PendingMergeHead != target.Head {
			return fmt.Errorf("target ref changed during an unfinished merge attempt")
		}
	}
	if !alreadyMerged {
		state.MergeTarget = target.Branch
		if !command.NoVerify {
			if err := app.hooks(ctx, state.Workspace, "pre_merge", state.Config.PreMerge); err != nil {
				return err
			}
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
		if err := g.clean(ctx, repo, checkedSource, command.Keep || command.IgnoreUncommitted); err != nil {
			return err
		}
		if err := g.checkClean(ctx, repo, checkedTarget, false, false); err != nil {
			return err
		}
		if err := g.protectIgnored(ctx, checkedTarget, source.Head); err != nil {
			return err
		}
		if command.Rebase {
			if err := app.interactiveGit(ctx, source.Path, "rebase", "--no-autostash", target.Head); err != nil {
				return fmt.Errorf("rebase failed in %s; continue or abort it there; resources preserved: %w", source.Path, err)
			}
			source, err = g.source(ctx, repo, state.Workspace)
			if err != nil {
				return err
			}
			checked, err := g.target(ctx, repo, target.Branch, state.Branch, true)
			if err != nil {
				return err
			}
			if checked.Head != target.Head || checked.Path != target.Path {
				return fmt.Errorf("target changed during source rebase")
			}
			if err := g.checkClean(ctx, repo, checked, false, false); err != nil {
				return err
			}
		}
		originalStage := state.Stage
		state.PendingMergeCommit, state.PendingMergeTarget = source.Head, target.Branch
		state.PendingMergeHead, state.PendingMergePath = target.Head, target.Path
		state.RetryMergeTarget = ""
		state.MergedCommit, state.MergeKept = "", false
		state.MergeResult, state.PendingSquashTree = "", ""
		state.PendingConflict = false
		state.MergeStrategy = "merge"
		if command.Rebase {
			state.MergeStrategy = "rebase"
		}
		if command.Squash {
			state.MergeStrategy = "squash"
		}
		state.Stage = "merging"
		if err := store.save(*state); err != nil {
			return err
		}
		args := []string{"merge", "--no-edit", "--no-autostash", "--no-overwrite-ignore"}
		targetDirectory, err := identityAt(target.Path, true)
		if err != nil {
			return err
		}
		if command.Squash {
			args = append(args, "--squash")
		}
		args = append(args, "--", source.Head)
		if err := app.interactiveGit(ctx, target.Path, args...); err != nil {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			var aborted bool
			var abortErr error
			if command.Squash {
				aborted, abortErr = g.abortSquash(abortCtx, repo, target, source.Head, targetDirectory)
			} else {
				aborted, abortErr = g.abortMerge(abortCtx, repo, target, source.Head)
			}
			if aborted {
				state.PendingMergeCommit, state.PendingMergeTarget = "", ""
				state.PendingMergeHead, state.PendingMergePath = "", ""
				state.PendingSquashTree, state.MergeStrategy = "", ""
				state.PendingConflict = false
				state.RetryMergeTarget = target.Branch
				state.Stage = "ready"
				if originalStage == "closed" {
					state.Stage = "closed"
				}
				if saveErr := store.save(*state); saveErr != nil {
					return errors.Join(err, fmt.Errorf("merge aborted; record recovery: %w", saveErr))
				}
				return fmt.Errorf("merge failed and was aborted in %s; source retained, correct it and retry: %w", target.Path, err)
			}
			var conflictErr error
			state.PendingConflict, conflictErr = g.mergeConflict(abortCtx, repo, target, source.Head, command.Squash)
			return errors.Join(fmt.Errorf("merge failed; inspect or abort the operation in %s, then retry; no resources were removed: %w", target.Path, err), abortErr, conflictErr, store.save(*state))
		}
		if command.Squash {
			state.PendingSquashTree, err = g.indexTree(ctx, target.Path)
			if err != nil {
				return err
			}
			if err := store.save(*state); err != nil {
				return err
			}
			if err := app.interactiveGit(ctx, target.Path, "commit"); err != nil {
				return fmt.Errorf("squash changes are staged in %s; fix the commit failure and retry merge: %w", target.Path, err)
			}
			state.PendingSquashTree, err = g.headTree(ctx, target.Path)
			if err != nil {
				return err
			}
		}
	}
	completed, err := g.target(ctx, repo, target.Branch, state.Branch, true)
	if err != nil {
		return err
	}
	if completed.Path != target.Path {
		return fmt.Errorf("target worktree changed during merge")
	}
	if err := g.checkClean(ctx, repo, completed, false, false); err != nil {
		return fmt.Errorf("merge did not leave a completed clean target: %w", err)
	}
	if state.MergeStrategy == "squash" {
		if state.PendingSquashTree != "" {
			if err := g.squashResult(ctx, completed, state.PendingMergeHead, state.PendingSquashTree); err != nil {
				return err
			}
		} else if err := g.ancestor(ctx, repo, state.MergeResult, completed.Head); err != nil {
			return err
		}
	} else if err := g.ancestor(ctx, repo, source.Head, completed.Head); err != nil {
		return err
	}
	return app.finishMerge(ctx, g, repo, store, state, source, completed, command)
}

func (app *App) finishMerge(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, source, completed gitWorktree, command Command) error {
	if err := g.checkClean(ctx, repo, completed, false, false); err != nil {
		return err
	}
	state.MergeResult = completed.Head
	state.PendingSquashTree = ""
	state.PendingConflict = false
	state.MergeTarget, state.MergedCommit = completed.Branch, source.Head
	state.PendingMergeCommit, state.PendingMergeTarget = "", ""
	state.PendingMergeHead, state.PendingMergePath, state.RetryMergeTarget = "", "", ""
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
	return mergeCleanupError(app.remove(ctx, g, repo, store, state, Command{Kind: "remove", Force: true}, true))
}

func (app *App) resumeSquash(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, source gitWorktree, command Command) (bool, error) {
	target, err := g.target(ctx, repo, state.PendingMergeTarget, state.Branch, true)
	if err != nil {
		return true, err
	}
	if target.Path != state.PendingMergePath {
		return true, fmt.Errorf("squash target worktree changed")
	}
	if target.Head == state.PendingMergeHead {
		if err := g.checkClean(ctx, repo, target, true, false); err != nil {
			return true, err
		}
		if _, err := g.run(ctx, target.Path, "diff", "--quiet", "--"); err != nil {
			return true, fmt.Errorf("squash target has unstaged changes; work preserved: %w", err)
		}
		tree, err := g.indexTree(ctx, target.Path)
		if err != nil {
			return true, err
		}
		if tree != state.PendingSquashTree {
			if err := g.checkClean(ctx, repo, target, false, false); err != nil {
				return true, fmt.Errorf("staged squash result changed: %w", err)
			}
			return false, app.clearMergeAttempt(store, state, command)
		}
		if source.Head != state.PendingMergeCommit || command.Into != "" && command.Into != state.PendingMergeTarget {
			return true, fmt.Errorf("source or target changed during unfinished squash")
		}
		if err := g.clean(ctx, repo, source, command.Keep || command.IgnoreUncommitted); err != nil {
			return true, err
		}
		if err := app.interactiveGit(ctx, target.Path, "commit"); err != nil {
			return true, err
		}
		state.PendingSquashTree, err = g.headTree(ctx, target.Path)
		if err != nil {
			return true, err
		}
		target, err = g.target(ctx, repo, target.Branch, state.Branch, true)
		if err != nil {
			return true, err
		}
	}
	if source.Head != state.PendingMergeCommit || command.Into != "" && command.Into != state.PendingMergeTarget {
		return true, fmt.Errorf("source or target changed during unfinished squash")
	}
	if err := g.squashResult(ctx, target, state.PendingMergeHead, state.PendingSquashTree); err != nil {
		return true, err
	}
	if err := g.clean(ctx, repo, source, command.Keep || command.IgnoreUncommitted); err != nil {
		return true, err
	}
	return true, app.finishMerge(ctx, g, repo, store, state, source, target, command)
}

func (app *App) clearMergeAttempt(store *stateStore, state *workspaceState, command Command) error {
	config, err := LoadConfigForRepo(app.ConfigDir, state.Root)
	if err != nil {
		return err
	}
	if command.NoHooks {
		config.PostCreate, config.PreMerge, config.PreRemove = nil, nil, nil
	}
	state.Config = config
	state.PendingMergeCommit, state.PendingMergeTarget, state.PendingMergeHead, state.PendingMergePath = "", "", "", ""
	state.PendingSquashTree, state.MergeStrategy, state.MergeResult, state.Stage = "", "", "", "ready"
	state.PendingConflict = false
	return store.save(*state)
}

func mergeCleanupError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("merge succeeded; cleanup incomplete: %w", err)
}

func removalDataCheck(ctx context.Context, g gitHost, repo gitRepository, state workspaceState, source gitWorktree, force bool) error {
	if err := g.clean(ctx, repo, source, force); err != nil {
		return err
	}
	return nil
}

func removalTarget(ctx context.Context, g gitHost, repo gitRepository, state workspaceState) (string, string, error) {
	if state.Removal != nil {
		comparison, err := removalComparison(ctx, g, repo, state.Removal)
		return comparison.Ref, comparison.Commit, err
	}
	if state.MergedCommit != "" && state.MergeTarget != "" {
		ref := "refs/heads/" + state.MergeTarget
		oid, err := g.resolve(ctx, repo.Root, ref)
		return ref, oid, err
	}
	if state.BaseRef != "" {
		branch, err := g.localBase(ctx, repo, state.BaseRef, state.Branch)
		if err != nil {
			return "", "", err
		}
		ref := state.BaseRef
		if branch != "" {
			ref = "refs/heads/" + branch
		}
		out, err := g.run(ctx, repo.Root, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
		if err == nil {
			oid := strings.TrimSpace(string(out))
			if !validOID(oid) {
				return "", "", fmt.Errorf("invalid comparison commit ID")
			}
			return ref, oid, nil
		}
		if !gitExit(err, 1) {
			return "", "", err
		}
	}
	target, err := g.targetBranch(ctx, repo, "", state.Branch)
	if err != nil {
		return "", "", err
	}
	return "refs/heads/" + target.Branch, target.Head, nil
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

func (app *App) removalCheck(ctx context.Context, g gitHost, repo gitRepository, state workspaceState, source gitWorktree, force, keepBranch bool) (string, error) {
	if err := removalDataCheck(ctx, g, repo, state, source, force); err != nil {
		return "", err
	}
	if state.Removal != nil && state.Removal.MergeResult != "" && !keepBranch {
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
	if force || keepBranch {
		return "", nil
	}
	ref, oid, err := removalTarget(ctx, g, repo, state)
	if err != nil {
		return "", err
	}
	if state.Removal != nil && state.Removal.DiscardCommits {
		if source.Head != state.Removal.Head {
			return "", fmt.Errorf("source changed after commit discard approval")
		}
	} else {
		if err := g.ancestor(ctx, repo, source.Head, oid); err != nil {
			return "", err
		}
	}
	return ref, nil
}

func (app *App) confirmRemoval(ctx context.Context, source, ref string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, err := fmt.Fprintf(app.Stdout, "Branch %q has commits not merged into %q. Removing it will discard those commits.\nAre you sure you want to remove %q? [y/N] ", source, ref, source); err != nil {
		return false, err
	}
	if writer, ok := app.Stdout.(interface{ Flush() error }); ok {
		if err := writer.Flush(); err != nil {
			return false, err
		}
	}
	line := ""
	if app.Stdin != nil {
		var err error
		line, err = bufio.NewReader(io.LimitReader(app.Stdin, 4097)).ReadString('\n')
		if err != nil && err != io.EOF {
			return false, fmt.Errorf("read removal confirmation: %w", err)
		}
		if len(line) > 4096 {
			return false, fmt.Errorf("removal confirmation exceeds 4096 bytes")
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
		return false, writer.Flush()
	}
	return false, nil
}

func (app *App) remove(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, command Command, merged bool) error {
	if state.Stage == "removed" {
		return nil
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
	if state.Removal == nil {
		removal := &removalState{KeepBranch: command.KeepBranch, Force: command.Force}
		if present {
			source, err := g.source(ctx, repo, state.Workspace)
			if err != nil {
				return err
			}
			if merged && source.Head != state.MergedCommit {
				return fmt.Errorf("source changed after merge; refusing cleanup")
			}
			removal.Head = source.Head
			if !removal.KeepBranch && state.MergedCommit == source.Head && validOID(state.MergeResult) && (merged || state.MergeStrategy == "squash") {
				removal.MergeResult = state.MergeResult
				removal.Target = "refs/heads/" + state.MergeTarget
				if err := pinRemovalTarget(ctx, g, repo, removal); err != nil {
					return err
				}
				if merged && removal.TargetOID != state.MergeResult {
					return fmt.Errorf("target changed after successful merge")
				}
				if err := g.ancestor(ctx, repo, state.MergeResult, removal.TargetOID); err != nil {
					return err
				}
			}
			removal.Identity, err = captureCleanupIdentity(repo, source)
			if err != nil {
				return err
			}
			if err := removalDataCheck(ctx, g, repo, *state, source, removal.Force); err != nil {
				return err
			}
			if !removal.Force && !removal.KeepBranch && removal.MergeResult == "" {
				removal.Target, removal.TargetOID, err = removalTarget(ctx, g, repo, *state)
				if err != nil {
					return err
				}
				if err := pinRemovalTarget(ctx, g, repo, removal); err != nil {
					return err
				}
				contained, err := g.isAncestor(ctx, repo, source.Head, removal.TargetOID)
				if err != nil {
					return err
				}
				if !contained {
					if merged {
						return fmt.Errorf("source is no longer merged into the recorded target")
					}
					approved, err := app.confirmRemoval(ctx, state.Branch, removal.Target)
					if err != nil || !approved {
						return err
					}
					removal.DiscardCommits = true
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
			if _, err := app.removalCheck(ctx, g, repo, pending, checked, removal.Force, removal.KeepBranch); err != nil {
				return err
			}
		} else {
			removal.Head = state.InitialCommit
			removal.HookDone, removal.WorktreeRemoved = true, true
			removal.KeepBranch = command.KeepBranch || !state.CreatedBranch
			if !removal.KeepBranch && !removal.Force {
				removal.Target, removal.TargetOID, err = removalTarget(ctx, g, repo, *state)
				if err != nil {
					return err
				}
				if err := pinRemovalTarget(ctx, g, repo, removal); err != nil {
					return err
				}
				if err := g.ancestor(ctx, repo, removal.Head, removal.TargetOID); err != nil {
					return err
				}
			}
		}
		state.Removal = removal
		state.MergeKept = false
		state.Stage = "removing"
		if err := store.save(*state); err != nil {
			return err
		}
	} else {
		if command.Force && !merged && !state.Removal.Force {
			if err := verifyCleanupIdentity(state.Workspace, state.Removal.Identity, !present); err != nil {
				return err
			}
			var head string
			if present {
				source, err := g.source(ctx, repo, state.Workspace)
				if err != nil {
					return err
				}
				if err := g.clean(ctx, repo, source, true); err != nil {
					return err
				}
				head = source.Head
			} else {
				head, err = g.branchOID(ctx, repo, state.Branch)
				if err != nil {
					return err
				}
			}
			if head != "" && head != state.Removal.Head && (!command.KeepBranch || state.Removal.BranchDeleted) {
				return fmt.Errorf("source ref changed after cleanup began; --force cannot discard new commits; use --keep-branch to preserve them")
			}
			state.Removal.Force = true
		}
		if state.Removal.Job != nil {
			state.Removal.Job.Status = "cancelled"
		}
		if command.KeepBranch && !state.Removal.BranchDeleted {
			state.Removal.KeepBranch = true
		}
		if !state.Removal.Force && !state.Removal.KeepBranch {
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
			if !command.KeepBranch || merged || removal.BranchDeleted {
				return fmt.Errorf("source ref changed after cleanup began; retry remove --keep-branch to preserve the new commit and revalidate cleanup")
			}
			if _, err := app.removalCheck(ctx, g, repo, *state, source, removal.Force, true); err != nil {
				return err
			}
			removal.Head, removal.Target, removal.TargetOID = source.Head, "", ""
			removal.TargetRefOID = ""
			removal.MergeResult = ""
			removal.DiscardCommits = false
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
	transaction := "start\x00"
	if !removal.Force || removal.MergeResult != "" {
		comparison, err := removalComparison(ctx, g, repo, removal)
		if err != nil {
			return err
		}
		if !removal.DiscardCommits {
			proof := current
			if removal.MergeResult != "" {
				proof = removal.MergeResult
			}
			if err := g.ancestor(ctx, repo, proof, comparison.Commit); err != nil {
				return err
			}
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
