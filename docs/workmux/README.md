# Workmux command guide

`cli workmux` manages Git worktrees, tmux windows, and optional Podman containers.

- **Worktree:** a separate checkout of a Git branch.
- **Window:** the tmux window containing that checkout's configured panes.
- **Sandbox:** a container running a command against the checkout.

`add` creates worktrees under:

```text
<repo-parent>/worktrees__<repo-name>/<branch-handle>
```

For `/projects/app`, `add feature/login` creates `/projects/worktrees__app/feature-login`. Handles come from branch names: uppercase becomes lowercase and separators become hyphens. Colliding handles are rejected.

Workspace names can be branch names or unique handles. Workmux discovers linked worktrees through Git, including worktrees outside its default directory. Workspace window creation requires execution inside a live tmux pane and tmux 3.2 or newer. Standalone sandbox commands do not require tmux.

## 1. init

Creates a configuration file with commented examples.

```sh
# Repository-specific configuration
cli workmux init

# Global configuration, including outside Git
cli workmux init --global
```

Configuration files live under:

```text
~/.config/cli/workmux/<repo-name>.yaml
~/.config/cli/workmux/config.yaml
```

The repository name is the main checkout's directory basename. Repositories with the same basename share configuration. The reserved name `config` and names beginning with `repo-` receive a `repo-` prefix in their configuration filename.

- Repository init requires a Git repository.
- Global init also accepts `-g`.
- Neither starts tmux or containers.
- Existing files are not overwritten.
- The examples do not enable sandboxing or hooks automatically.
- Configuration files are created with mode `0600`.

## 2. Configuration

Global settings load first, then repository overrides. Workmux configuration uses `~/.config/cli/workmux`, not `$XDG_CONFIG_HOME`.

This is an illustrative repository configuration, not the default configuration:

```yaml
panes:
  - name: opencode
    command: opencode
    focus: true

  - name: shell
    split: horizontal
    percentage: 35

layouts:
  review:
    panes:
      - name: editor
        command: nvim
        focus: true

      - name: shell
        split: horizontal
        percentage: 35

files:
  copy:
    - ".env"
  symlink:
    - ".local-config"

post_create: []
pre_merge: []
pre_remove: []

sandbox:
  enabled: false
  image: localhost/cli-workmux:fedora44

  # Optional read-only OpenCode configuration override.
  # opencode_config_dir: ~/.config/opencode
```

### Defaults and inheritance

Without configuration, sandboxing is disabled. The default window has a focused host shell and a horizontal pane running `clear`. With sandboxing enabled and no explicit pane list, the default window has a focused sandboxed OpenCode pane and a horizontal host shell.

There are no default hooks or file operations.

| Setting | Repository override behavior |
|---|---|
| `panes` | Replace the complete inherited list |
| `layouts` | Override layouts by name; other layouts remain |
| Hook lists | Replace the inherited list |
| `files.copy`, `files.symlink` | Replace each inherited list independently |
| `sandbox` | Override individual fields |

For top-level `panes`, hook lists, and file lists, omission or `null` inherits; `[]` explicitly clears. Each named layout must specify a non-null `panes` list. An empty pane list still opens the window's initial shell. Hook and file lists support `<global>` to insert the global list:

```yaml
post_create:
  - "<global>"
  - "git status --short"
```

Unknown fields, duplicate YAML keys, multiple YAML documents, and YAML merge keys are rejected. All layouts are validated, including unused layouts.

### Panes and layouts

`panes` defines the default window. `layouts.<name>.panes` defines a named arrangement selected by `add --layout <name>` whenever it creates a tmux window, including for an existing worktree. An already-open window is focused without rearranging its panes.

| Pane field | Behavior |
|---|---|
| `name` | Pane name |
| `command` | Startup command; omit it for a host shell |
| `split` | `horizontal` or `vertical`; required after the first pane |
| `target` | Zero-based index of an earlier pane to split; defaults to the previous pane |
| `size` | Absolute split size |
| `percentage` | Split percentage from 1 to 100 |
| `focus` | Select this pane; the last selected pane wins |
| `zoom` | Zoom and select this pane; at most one pane may request it |

The first pane cannot specify a split, size, or percentage. A later pane cannot specify both size and percentage.

### Files

`files.copy` copies from the main checkout. `files.symlink` creates relative links back to it. Paths keep the same relative destination in the linked worktree.

- Literal paths, globs, and recursive `**` patterns are supported.
- Absolute patterns must be inside the main checkout.
- Explicit selections of the checkout's root `.git` entry or its contents are rejected. Broad selections such as `.` and `**` skip that entry and its contents.
- Unmatched patterns are skipped.
- Existing destination files and conflicting directories can be replaced.
- Directory copies merge with existing directories.
- Editing a symlinked file changes the main checkout's file.
- Explicitly selected source symlinks may resolve outside the repository.

These operations run during new worktree creation only. Opening an existing worktree does not repeat them.

### Hooks

Only explicitly configured commands execute. Each list runs sequentially and stops on its first failure.

| Hook | When it runs |
|---|---|
| `post_create` | After file setup for a newly created worktree |
| `pre_merge` | Before merging |
| `pre_remove` | Before removing a workspace, including successful merge cleanup |

Hooks run through host Bash in the workspace directory, even when pane sandboxing is enabled. They receive `WM_HANDLE`, `WORKMUX_HANDLE`, `WM_WORKTREE_PATH`, `WM_PROJECT_ROOT`, `WM_CONFIG_DIR`, `WM_BRANCH_NAME`, and `WM_TARGET_BRANCH`. The target variable is set when a merge target is known.

### Sandbox configuration

| Field | Default and behavior |
|---|---|
| `enabled` | `false`; enables automatic OpenCode pane sandboxing |
| `image` | `localhost/cli-workmux:fedora44` |
| `opencode_config_dir` | `$XDG_CONFIG_HOME/opencode`, otherwise `~/.config/opencode` |

When enabled, OpenCode pane commands run in separate containers. Recognition uses the leading executable named exactly `opencode`, including explicit or quoted paths and an optional leading `exec`. A compound command beginning with OpenCode runs entirely in its container; OpenCode appearing after another command is not detected. Recognition does not resolve aliases or recognize `env` or shell wrappers. Other commands and empty-command shell panes remain on the host. Sandbox preparation errors do not fall back to host execution.

Sandboxed panes launch `cli workmux sandbox run -- opencode`, with the configured command kept intact as one payload argument. The pane resolves `cli` through its own `PATH` and uses its own environment, just as if you typed the command there. Short, single-line launches appear directly in the pane; long launches or those containing terminal control characters use a private, self-deleting script. These runs use the same lifecycle as manual sandbox runs. Pane creation hands off asynchronously; it does not wait for the sandbox command to finish.

If the OpenCode configuration directory exists, it is mounted read-only at `/tmp/.config/opencode`. A missing directory is not created or mounted. Overrides support `~/...` and must identify a real directory rather than a symlink.

Sandbox runs wait for the repository's preparation lock, then release it after registering and before running the foreground command. Cancellation ends the lock wait. Other workmux commands still report a busy repository immediately.

## 3. sandbox build

Builds the sandbox image using the CLI's embedded Containerfile.

```sh
cli workmux sandbox build

# Rebuild without cached layers
cli workmux sandbox build --no-cache
```

- Uses the configured `sandbox.image`.
- Inside a repository, uses its effective configuration; outside Git, uses global configuration.
- Does not require tmux or `sandbox.enabled: true`.
- Builds from an isolated context, not your repository.
- `--no-cache` is forwarded to Podman.

The bundled image uses Fedora 44 and installs OpenCode, Bash, Git, curl, CA certificates, ripgrep, archive utilities, findutils, coreutils, and shadow utilities. Its default command is Bash. It does not install Go or Node.js. The build downloads the OpenCode installer; its version is not pinned.

Run this before launching a sandbox if its image is missing. Launch does not build or pull an image automatically.

## 4. sandbox run

Starts a new interactive container. With no command, opens interactive Bash.

```sh
cli workmux sandbox run
cli workmux sandbox run -- opencode
cli workmux sandbox run -- 'git status && opencode'
```

- Works from a main checkout or linked worktree, including subdirectories.
- Always starts at that checkout's root, not the caller's subdirectory.
- Always uses `--rm -it`.
- Requires neither tmux nor `workmux add`.
- Runs sandboxed even if `sandbox.enabled` is false.
- Removes the container when its original command exits.

Commands are joined into Bash shell text. Quotes must survive your host shell when the container command needs quoted arguments or operators.

You can run several containers concurrently. Their lifecycles are independent, but containers using the same checkout share writable files. Changes to mounted checkout files persist after container removal.

OpenCode state and cache are private to each container. Its data mount remains shared and writable, and its configuration mount remains read-only. Linked-worktree sandboxes mount the main checkout read-only and protect sensitive Git metadata while permitting the Git writes needed for work in the selected checkout. This is not a separate Git repository or a read-only checkout.

Before each sandbox run, including sandboxed panes, Workmux copies the host's `~/.local/state/opencode/model.json` into `/tmp/.local/state/opencode/model.json` inside the container. This is a fresh snapshot, not a mount; container changes never copy back to the host. The directory has `0700` permissions and the file has `0600` permissions. Missing, unreadable, symlinked, or larger-than-64-KiB source files produce a warning and skip the copy. A startup command larger than 120 KiB after adding the copy also skips seeding. Guest copy failures warn and continue; symlinked destination directories are rejected without modification. Existing sandbox mount safety checks still apply.

Active sandbox runs, whether launched manually or in an OpenCode pane, block merge and removal of their checkout until they exit. Use `close` to close the owned panes, then retry after their sessions finish cleanup. Legacy persistent containers are rejected by new runs and preserved for explicit recovery and removal.

## 5. add

Creates a worktree and opens its configured tmux window, or opens the exact branch's existing linked worktree.

```sh
cli workmux add feature/login
cli workmux add feature/login --base main
cli workmux add feature/login --layout review
```

| Option | Purpose |
|---|---|
| `--base <ref>` | Starting point for a new branch |
| `--layout <name>`, `-l <name>` | Choose a configured pane layout when creating a tmux window |

For a new workspace, it creates the worktree, applies configured files, runs explicit `post_create` hooks, creates panes, and focuses the window.

If the branch already exists without a worktree, it uses that branch without resetting it. Otherwise it creates a branch from `--base`, or the current checkout's branch if no base is supplied. Detached HEAD requires `--base` for a new branch.

If the exact branch already has a linked worktree, Workmux reuses that checkout without repeating file setup or `post_create` hooks. `--base` does not reset or change the existing branch. If its tmux window exists, Workmux focuses it without changing its panes. If the window is missing, Workmux creates one using `--layout` when supplied, otherwise the default panes. The requested layout is validated even when a window already exists. The main checkout is not a linked workspace.

Setup failures preserve files and recovery state rather than automatically rolling back. Incomplete setup cannot be reprovisioned through `open`; use removal and recreation after inspecting the retained work.

Once a workspace is recorded, its branch identity is retained. Switching or renaming its branch, or detaching HEAD, blocks workspace commands until the recorded branch is restored. File setup is followed by worktree validation before `post_create` hooks run.

## 6. open

Opens existing linked worktrees in tmux. A name is required.

```sh
cli workmux open feature/login
cli workmux open feature/login feature/search
```

- Focuses the owned window if one exists.
- Otherwise creates one using current default pane configuration.
- Never creates a Git worktree or an additional window when an owned one exists.
- Never repeats file setup or hooks.
- When creating a window, uses default panes rather than restoring a named layout originally selected by `add --layout`.
- Processes multiple names sequentially; a failure stops further processing without undoing earlier openings.

## 7. close

Closes the workspace's tmux window and its attached sandbox runs.

```sh
cli workmux close feature/login

# From inside the linked worktree
cli workmux close
```

It keeps the worktree, files, and branch. Use `open` to return later. Close errors if no matching window is open.

Closing a pane terminates its attached sandbox process. Workspace ownership cleanup does not stop unrelated manual sandbox runs. Legacy managed containers are stopped without a grace period. Detached host jobs may survive window closure.

If a workspace has several owned windows, named close selects one window. Other windows' public sandbox runs keep running. Legacy managed container stopping is workspace-wide.

## 8. Dirty-work confirmation

Merge and removal use the same policy:

1. Detect staged, unstaged, and nonignored untracked changes.
2. Warn that continuing permanently discards them before the operation.
3. Ask `y/N`.
4. On `y`, discard staged and unstaged changes, delete untracked files, then start the operation.

Only `y`, case-insensitive and whitespace-trimmed, approves. `N`, empty input, and EOF cancel without mutation. Discarding does not commit or stash work.

Discarded changes and deleted files are not restored if the subsequent operation fails. Changes introduced after confirmation are not silently discarded.

## 9. merge

Fast-forwards a target branch to the workspace's committed history, then cleans up the source worktree and branch on success. It does not create a merge commit, rebase, or squash. Requires Git 2.31 or newer.

```sh
cli workmux merge feature/login --into main
```

| Option | Purpose |
|---|---|
| `--into <branch>` | Select the target local branch |

The source and target histories must not have diverged. If they have, Workmux stops before dirty-work confirmation, hooks, or switching the main checkout, and asks you to rebase manually onto the selected local target before retrying. For a target named `main`:

```sh
# From inside the linked worktree
git rebase main
cli workmux merge --into main
```

Retry only after the rebase succeeds. If it stops for conflicts, resolve them and continue the rebase, or abort it before starting another operation.

If the source's commits are already included in the target, including when both point to the same commit, Workmux skips Git integration and performs normal cleanup. Dirty source files still follow the confirmation policy above.

Without `--into`, target selection uses a recorded local base, then a local branch corresponding to `origin/HEAD`, then local `main` or `master`. Each new merge attempt selects its target independently. Attempts that did not complete integration do not supply a target to later invocations. Repeat `--into` when retrying to select the same explicit target. If the target has no worktree, Workmux may switch the clean main checkout to it and leave it there. It does not fetch or push; the history check uses the selected local target.

An active sandbox run on the source checkout, including an OpenCode pane, blocks merging. The target must be clean; ignored target files that incoming changes would overwrite are protected. Workmux rechecks the source and target after hooks and enforces fast-forward-only integration even when Git configuration requests another merge behavior. Failed operations preserve work that was not already discarded by confirmation; Workmux does not automatically reset or abort Git operations. Finish or abort any unfinished Git operation manually before retrying.

A successful integration is not undone if cleanup fails. Workmux reports that merge succeeded but cleanup is incomplete and retains cleanup checkpoints. Successful integration and its cleanup remain bound to the recorded source and target commits, including when an interruption delays cleanup. Retry removal to finish cleanup; recorded branch, commit, and ownership checks still apply.

## 10. remove, alias rm

Removes a workspace without merging it. Successful removal always deletes its branch, including unmerged commits, without a commit-loss prompt.

```sh
cli workmux remove feature/login
cli workmux rm feature/login
cli workmux remove feature/login feature/search
```

- Dirty files trigger the shared `y/N` confirmation before removal.
- Removes managed sandboxes, owned tmux resources, the worktree, and the branch.
- Unmerged commits alone do not trigger confirmation.
- An active sandbox run, including an OpenCode pane, blocks removal.
- Identity, ownership, worktree locks, unfinished Git operations, and source changes can still block cleanup.

Git's native removal restrictions still apply. Workmux does not recursively delete submodule Git metadata. If it blocks removal, inspect the retained data and finish that cleanup manually.

Multiple names are processed sequentially, with the caller's workspace last. A failure does not undo already-completed removals.

When removing the window from which the command is running, cleanup may finish in a detached worker. "Scheduled" does not mean deletion has completed. Failed or interrupted cleanup retains recovery checkpoints; inspect the reported diagnostic log and retry removal as instructed. Replacement directories or changed branches are not silently deleted.

## 11. Help

```sh
cli workmux --help
cli workmux help add
cli workmux sandbox --help
```

Use `--` to end option parsing.

## Typical workflows

Managed worktree:

```sh
cli workmux add feature/login --base main
cli workmux close feature/login
cli workmux open feature/login
cli workmux merge feature/login --into main
```

Standalone OpenCode sandbox:

```sh
cli workmux sandbox build
cli workmux sandbox run -- opencode
```

`add` creates or opens, `open` resumes, `close` keeps your work, `merge` integrates committed history and deletes the workspace, and `remove` deletes it without merging.
