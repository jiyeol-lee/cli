# Workmux command guide

`cli workmux` manages Git worktrees, tmux windows, and optional sandbox containers.

- **Worktree:** a separate checkout of a Git branch.
- **Window:** the tmux window containing that checkout's configured panes.
- **Sandbox:** a container running a command against the checkout.

## 1. init

Creates a configuration file with commented examples.

```sh
# Repository-specific configuration
cli workmux init

# Global configuration
cli workmux init --global
```

Files go under:

```text
~/.config/cli/workmux/<repo-name>.yaml
~/.config/cli/workmux/config.yaml
```

Global settings load first, then repository overrides.

- Repository init requires a Git repository.
- Global init works outside Git.
- Neither starts tmux or containers.
- Existing configuration files are not overwritten.
- The generated examples do not enable sandboxing automatically.

## 2. sandbox build

Builds the sandbox image using the CLI's embedded Containerfile.

```sh
cli workmux sandbox build
```

- Uses the configured `sandbox.image`.
- Inside a repository, uses its effective configuration.
- Outside Git, uses global configuration.
- Does not require tmux or `sandbox.enabled: true`.
- Does not use your repository as the build context.

Run this before launching a sandbox if its image is missing.

## 3. sandbox run

Starts a new interactive container for your command.

```sh
cli workmux sandbox run -- opencode
cli workmux sandbox run -- bash
cli workmux sandbox run -- 'go test ./... && opencode'
```

- Works from a main checkout or linked worktree, including subdirectories.
- Preserves your current directory inside the container.
- Always uses `--rm -it`.
- Requires neither tmux nor `workmux add`.
- Runs sandboxed even if `sandbox.enabled` is false.
- Removes the container when its original command exits.

You can run several concurrently. Their container lifecycles are independent, but containers using the same checkout share its files.

OpenCode state is private to each container. Its existing data mount remains shared, and its configuration mount remains read-only.

Quotes around shell expressions keep your host shell from interpreting operators such as `&&`.

## 4. sandbox shell

Enters an already-running sandbox associated with the current checkout.

```sh
cli workmux sandbox shell

# Execute a command inside an existing sandbox
cli workmux sandbox shell -- git status
```

- Uses interactive `podman exec`.
- Never creates a container.
- Errors if no matching running sandbox exists.
- Can find standalone and pane-owned sandboxes.
- When several match, selects one deterministically and reports the selection.

Exiting this additional shell does not stop the container's original command.

To create a fresh shell instead:

```sh
cli workmux sandbox run -- bash
```

## 5. add

Creates a worktree and opens its configured tmux window.

```sh
cli workmux add feature/login
cli workmux add feature/login --base main
```

It performs worktree creation, configured file setup, `post_create` hooks, and pane creation.

If the branch already exists, it can use that branch provided it is not already checked out in another worktree. Otherwise, it creates the branch.

When sandboxing is enabled, selected pane commands run in their own sandbox containers. Other panes run on the host.

| Option | Purpose |
|---|---|
| `--base <ref>` | Starting point for a new branch |
| `--layout <name>` | Choose a configured pane layout |
| `--background` | Create the window without focusing it |
| `--open-if-exists` | Open the branch's existing worktree instead |
| `--name <handle>` | Override the workspace handle |
| `--dry-run` | Report the planned setup without provisioning |
| `--no-hooks` | Skip lifecycle hooks for this invocation |
| `--no-file-ops` | Skip configured copying and symlinking |
| `--no-pane-cmds` | Create panes without launching their configured commands |

`--background` concerns tmux focus. It does not mean a detached standalone container.

## 6. open

Opens an existing worktree in tmux.

```sh
cli workmux open feature/login
```

- Focuses its window if one already exists.
- Otherwise creates a window using current configuration.
- Does not create a new Git worktree.
- Does not normally repeat file setup or `post_create` hooks.

```sh
# Rerun post_create hooks
cli workmux open feature/login --run-hooks

# Reapply configured file setup
cli workmux open feature/login --force-files

# Create another window even if one exists
cli workmux open feature/login --new
```

You can open multiple worktrees:

```sh
cli workmux open feature/login feature/search
```

A name is required unless using `--new`, which can target the current linked worktree.

## 7. close

Closes the workspace's tmux window and stops its managed pane sandboxes.

```sh
cli workmux close feature/login

# From inside the linked worktree
cli workmux close
```

It keeps the worktree, files, and branch. Use `open` to return later.

Standalone containers started with `sandbox run` are not stopped by workspace cleanup ownership rules. However, closing a tmux window can still terminate ordinary processes attached to its panes.

## 8. merge

Merges the workspace branch into a target branch.

```sh
cli workmux merge feature/login --into main
```

By default, successful merging is followed by workspace cleanup, including worktree and branch removal.

```sh
# Merge but retain the workspace and branch
cli workmux merge feature/login --into main --keep

# Rebase the source onto the target before merging
cli workmux merge feature/login --into main --rebase

# Squash changes into a commit on the target
cli workmux merge feature/login --into main --squash
```

| Option | Purpose |
|---|---|
| `--cleanup` | Explicitly request the default cleanup behavior |
| `--ignore-uncommitted` | Allow source working-tree changes through relevant checks; does not commit them |
| `--no-verify` | Skip Workmux's `pre_merge` hook |
| `--no-hooks` | Skip Workmux lifecycle hooks for this invocation |

Without `--into`, Workmux resolves the target using its recorded base and fallback logic.

An active standalone sandbox on the source checkout blocks this operation. Conflicts or incomplete cleanup preserve recovery information rather than blindly deleting resources.

## 9. remove, alias rm

Removes a workspace without merging it.

```sh
cli workmux remove feature/login
cli workmux rm feature/login
```

Normally cleans up its managed sandboxes, tmux resources, worktree, and branch.

```sh
# Remove the worktree but retain its branch
cli workmux remove feature/login --keep-branch

# Force removal, potentially discarding work
cli workmux remove feature/login --force
```

- Normally rejects uncommitted changes.
- Can ask for confirmation before discarding unmerged commits.
- `--keep-branch` preserves commits, not uncommitted worktree files.
- `--force` is destructive, but does not bypass all identity and ownership checks.
- An active standalone sandbox blocks removal.

Multiple names are supported:

```sh
cli workmux remove feature/login feature/search
```

When removing the window from which the command is running, cleanup may finish in a detached worker. "Scheduled" does not mean deletion has already completed.

## 10. Help

```sh
cli workmux --help
cli workmux help add
cli workmux sandbox --help
```

## Typical workflows

Managed worktree:

```sh
cli workmux add feature/login --base main
cli workmux close feature/login
cli workmux open feature/login
cli workmux merge feature/login --into main
```

Standalone sandbox in your current checkout:

```sh
cli workmux sandbox run -- opencode
```

`add` creates, `open` resumes, `close` keeps your work, `merge` integrates it, and `remove` deletes the workspace.
