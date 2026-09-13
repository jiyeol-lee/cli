# cli

`cli` is one personal command-line program for vocabulary practice, AP News reading, a read-only view of today's Google Calendar, scoped conversation memories, and Git worktrees in tmux.

## Install

The vocabulary database uses `github.com/mattn/go-sqlite3`, so builds need CGO and a C compiler.

```sh
go install github.com/jiyeol-lee/cli/cmd/cli@latest
```

To build a checkout:

```sh
go build ./cmd/cli
```

## Commands

```text
cli voca add <phrase>
cli voca delete <phrase>
cli voca list
cli voca study [phrase]
cli voca story
cli voca news

cli gcal list [--text] [--join separator]
cli gcal soon [--text] [--join separator]
cli gcal in-progress [--text] [--join separator]

cli memory directory
cli memory read [--scope all|project|global]
cli memory write <memory words...> --category preference|convention|note --scope project|global
cli memory archive <positive-id> --scope project|global

cli workmux add <branch> [--base <ref>] [-l|--layout <name>] [-b|--background]
cli workmux merge [name] [--into <branch>] [--keep]
cli workmux remove [name] [--keep-branch] [--force]
cli workmux open [name]
cli workmux close [name]
```

Calendar commands emit JSON arrays by default. Empty JSON output is `[]`. `--text` prints one event per line as `Summary (08:30 - 09:00)` for timed events and only the summary for all-day events. Text output always ends with a newline, and an empty result is `N/A`. `--join separator` replaces the newline between text events with the given separator and requires `--text`. The separator may be empty.

`gcal in-progress` returns every timed event where the start is at or before the current time and the end is after it. `gcal soon` returns every timed event tied for the earliest start after the current time. Both commands preserve calendar order and exclude all-day events.

Memory commands store preferences, conventions, and notes at project or global scope. Project scope uses the main Git worktree path, so nested directories and linked worktrees share one group. Outside a Git repository, it uses the absolute current directory. `read` shows the current project's active memories first, followed by global memories. Writes and archives print nothing on success.

## Data locations

Vocabulary, memory, and calendar data live in `$XDG_DATA_HOME/cli`. These apps require an absolute `$XDG_DATA_HOME`. Vocabulary and memory data share the SQLite database at `cli.sqlite3`. The Google token has a fixed location:

```text
$XDG_DATA_HOME/cli/google/oauth-token.json
```

For these apps, data and runtime directories follow `$XDG_DATA_HOME/cli/<app>/` and `$XDG_RUNTIME_DIR/cli/<app>/`. Runtime operations return an error when `$XDG_RUNTIME_DIR` is unset. Directories use mode `0700`; database and token files use mode `0600`.

Workmux uses `~/.config/cli/workmux` for configuration and `$XDG_STATE_HOME/cli/workmux` for state, defaulting to `~/.local/state/cli/workmux` when `$XDG_STATE_HOME` is unset. A supplied `$XDG_STATE_HOME` must be absolute. It uses private JSON files and per-repository `flock` locks, not SQLite. It does not require `$XDG_DATA_HOME` or `$XDG_RUNTIME_DIR` to be set.

## OpenCode API setup

`study` and `story` call the streaming Responses API at `https://opencode.ai/zen/go/v1/responses` with model `gpt-5.6-luna` and `low` reasoning effort.

```sh
export OPENCODE_GO_API_KEY=...
```

No API key is needed for vocabulary storage or AP News.

## Google Calendar setup

Create an OAuth 2.0 client for a desktop application in Google Cloud, enable the Google Calendar API, and configure the client to allow and use this redirect URL:

```text
http://localhost:8000/callback
```

Then export the credentials:

```sh
export GOOGLE_CLIENT_ID=...
export GOOGLE_CLIENT_SECRET=...
```

The first calendar command prints an authorization URL and tries to open it in the system browser. A short-lived loopback server receives the callback. Port 8000 on localhost must be available during the first authorization and any reauthorization. Later runs load and refresh the saved token. The read-only calendar scope is the only requested scope. Set `GCAL_CALENDAR_ID` to override the default `primary` calendar.

## AP News reader

`cli voca news` scrapes AP's public HTML, so AP markup changes can break headline or article extraction. It needs these shell programs:

```text
awk  fold  less  tput
```

Inside tmux it uses `tmux display-message` instead of `tput` to determine width. The reader folds each article to the terminal width, pipes it through `awk` to color headings, preserves a terminal hyperlink for the title, and sends the result directly to `less`.

## Workmux

Workmux manages one linked Git worktree and one tmux window per workspace. It requires Linux, Bash, Git, and tmux 3.2 or newer. Without configuration, it opens one host shell. With `sandbox.enabled: true`, every pane and lifecycle hook runs in one persistent container, including panes with no command.

Run all `cli workmux` commands in a host shell, from the repository or one of its worktrees. `add` and `open` require a live tmux pane with valid `$TMUX` and `$TMUX_PANE`. Keep a separate host window for lifecycle commands when using the sandbox. The image has no `cli` binary, and there is no host RPC service inside it.

### Sandbox setup

The sandbox uses only Podman. Workmux forces local mode with `--remote=false`. Use it rootless as your normal login user, not through `sudo podman`. On Fedora, install the host tools and check the engine:

```sh
sudo dnf install git tmux podman
tmux -V
podman --remote=false info
```

Fix rootless setup errors before proceeding, including missing subordinate UID/GID ranges in `/etc/subuid` and `/etc/subgid`. No container engine is needed when the sandbox is disabled.

From this checkout's root, review and manually copy the image files into the external Workmux directory. Back up any customized copies first, since `cp` replaces them. Each line is a separate command:

```sh
mkdir -p -m 0700 "$HOME/.config/cli/workmux"
cp internal/workmux/Containerfile "$HOME/.config/cli/workmux/Containerfile"
cp internal/workmux/.containerignore "$HOME/.config/cli/workmux/.containerignore"
podman --remote=false build -f "$HOME/.config/cli/workmux/Containerfile" -t localhost/cli-workmux:fedora44 "$HOME/.config/cli/workmux"
```

The [Containerfile](internal/workmux/Containerfile) uses Fedora 44 and pins OpenCode 1.18.30. It verifies the release archive against a hardcoded SHA-256 for x86_64 or aarch64. The [release checksums and source reference](internal/workmux/upstream.md#image-and-isolated-tests) are recorded alongside it. `.containerignore` excludes the entire build context, and the image copies no host files. Keep it when building beside your configuration.

The image includes Bash, Git, OpenCode, and common shell tools, but not `cli`, Go, or other language toolchains. Add tools needed by your project to your copied Containerfile and rebuild before creating workspaces.

Unset Podman routing and storage overrides, including `CONTAINER_HOST`, `CONTAINER_CONNECTION`, and `CONTAINERS_STORAGE_CONF`. Errors name the rejected variable; even an exported empty value counts. Workmux never builds an image or pulls a missing image automatically.

Before the first sandbox, start OpenCode normally once on the host. OpenCode 1.18.30 tries to create `.gitignore` in its config directory during startup, even with no plugins. Workmux requires that file to be readable, regular, and neither symlinked nor hardlinked before it provisions a worktree. If the config directory already exists and only `.gitignore` is missing, you may explicitly create an empty file on the host:

```sh
touch -- "${XDG_CONFIG_HOME:-$HOME/.config}/opencode/.gitignore"
```

Workmux never creates or rewrites this OpenCode metadata and never falls back to writable config. Plugins may need dependencies preinstalled on the host and tool paths available inside the image. Only a dependency-free local plugin has been integration-tested, not arbitrary plugins or external tools.

### Configuration

Workmux reads `~/.config/cli/workmux/config.yaml`, then `~/.config/cli/workmux/<repo-name>.yaml`. It always uses this literal `~/.config` location, regardless of `$XDG_CONFIG_HOME`. It never reads Workmux YAML from a checkout.

The repository name is the basename of the canonical main checkout, not the linked worktree. Repositories with the same name share configuration but have separate state, container identities, and tmux ownership IDs. The name `config` maps to `repo-config.yaml`. Every name starting with `repo-` gains one more `repo-`, so `repo-config` maps to `repo-repo-config.yaml`.

Missing files use defaults. Use `{}` for an empty file, not a blank document. This global example starts OpenCode and a shell in the sandbox. Enabling it shares your host OpenCode credentials and configuration as described under [Sandbox access and limits](#sandbox-access-and-limits). On an enforcing Fedora host, complete the [SELinux setup](#selinux-on-fedora) before `add`.

```yaml
# ~/.config/cli/workmux/config.yaml
panes:
  - command: opencode
    focus: true
  - split: horizontal
    percentage: 30
layouts:
  shell:
    panes: []
pre_merge: [git diff --check HEAD]
sandbox:
  enabled: true
  image: localhost/cli-workmux:fedora44
```

For a checkout named `myapp`, an optional repository file can add local files and hooks:

```yaml
# ~/.config/cli/workmux/myapp.yaml
files:
  copy: ["<global>", .env]
  symlink: ["<global>", local-data]
post_create: ["<global>", git status --short]
pre_remove: []
```

The accepted top-level keys are `panes`, `files`, `layouts`, `post_create`, `pre_merge`, `pre_remove`, and `sandbox`.

`sandbox` accepts only `enabled` and `image`. There is no runtime setting. Remove the obsolete `sandbox.container` block from older YAML files; it is now an error.

- Omitted keys inherit. Repository pane lists replace global panes. Hook lists and each file list also replace their global list.
- An exact `"<global>"` entry inserts the corresponding global list at that position. It works only in repository hook lists, `files.copy`, and `files.symlink`, not in panes or command strings.
- `[]` clears a hook or file list. `panes: []`, `panes: [{}]`, and an empty `command` all give a shell. Layouts merge by name, with a repository definition replacing the whole same-named layout. Sandbox fields inherit individually, including an explicit `enabled: false` override.
- Unknown or duplicate keys, null values, YAML anchors, aliases, merge keys, and multiple documents are errors. There is no `agent` setting or `<agent>` expansion. Use an ordinary command such as `opencode`.

Each pane after the first splits the previous pane. `horizontal` puts the new pane below it and is the default; `vertical` puts it beside it. Set either a positive `size` in cells or `percentage` from 1 to 100, not both. The first pane cannot specify `split`. At most one pane may set `focus: true` and at most one may set `zoom: true`. Select a named layout with `add --layout shell`.

File patterns are relative to the main checkout and support globs, including `**`. Directories copy recursively; `files.symlink` creates links back to real files or directories in the main checkout. Those targets are read-only inside the sandbox. Both operations reject source symlinks, including symlinked parents or entries inside a selected directory. Absolute paths, `..`, `.git` paths, overlapping selections, and existing destinations are rejected. An unmatched pattern prints a warning. Use these lists for files absent from the new worktree, often ignored local files, and review secrets such as `.env` before sharing them.

Hooks run sequentially in the workspace directory and stop on the first failure. `post_create` runs after file setup and container startup, before panes. `pre_merge` runs before merging; `pre_remove` runs before cleanup, including forced removal. Hooks and nonempty pane commands use `bash -c` on the host or inside the sandbox. Sandbox hooks have no TTY; empty sandbox panes use interactive Bash, and empty host panes use tmux's default shell. Host hooks with terminal stdin retain the foreground process group so they can read input. Other host hooks use a separate group that is killed on cancellation. Output-pipe waiting after cancellation is bounded to one second; detached background jobs may still survive.

Hooks receive `WM_HANDLE`, `WM_WORKTREE_PATH`, `WM_PROJECT_ROOT`, `WM_CONFIG_DIR`, `WM_BRANCH_NAME`, and `WM_TARGET_BRANCH`, plus `WORKMUX_HANDLE` as an alias for `WM_HANDLE`. `WM_PROJECT_ROOT` is the main checkout. `WM_TARGET_BRANCH` is empty unless a merge has selected one. `WM_CONFIG_DIR` names the external Workmux configuration directory, which is not automatically mounted inside the sandbox.

### SELinux on Fedora

Workmux leaves host labels unchanged. It never adds `:z`, `:Z`, or `label=disable`, and it does not change host ownership recursively. On an enforcing host, a bind mount can fail even when Unix permissions are correct. Check the denial and the exact source path rather than disabling SELinux.

One opt-in approach is to pre-label only the chosen checkout, its sibling worktree directory, the actual OpenCode directories, and the sandbox snapshot directory with `container_file_t`. This recursively changes labels on host files. It uses a shared container label, not a private label for each workspace, and may affect access by other confined applications or containers. Review the paths and credentials before doing it, or ask an administrator to arrange a narrower policy.

The following example assumes an existing checkout at `$HOME/dev/myapp`. Replace that path and its `myapp__worktrees` sibling with your reviewed paths. XDG overrides below must be absolute and point outside the repository. Do not substitute your entire home, `~/.config`, `~/.local/share`, a system directory, or all application state. Existing Workmux state and worktree-parent directories must already be private; `mkdir -p` does not fix their modes.

```sh
mkdir -p -m 0700 "$HOME/dev/myapp__worktrees"
mkdir -p -m 0700 "${XDG_DATA_HOME:-$HOME/.local/share}/opencode"
mkdir -p -m 0700 "${XDG_STATE_HOME:-$HOME/.local/state}/cli/workmux"
mkdir -p -m 0700 "${XDG_STATE_HOME:-$HOME/.local/state}/cli/workmux/containers"
chcon -R -t container_file_t -- "$HOME/dev/myapp"
chcon -R -t container_file_t -- "$HOME/dev/myapp__worktrees"
chcon -R -t container_file_t -- "${XDG_DATA_HOME:-$HOME/.local/share}/opencode"
chcon -R -t container_file_t -- "${XDG_CONFIG_HOME:-$HOME/.config}/opencode"
chcon -R -t container_file_t -- "${XDG_STATE_HOME:-$HOME/.local/state}/cli/workmux/containers"
```

Review labels again after moving files into these directories or a system relabel. `chcon` is not a persistent SELinux policy rule. See Podman's [bind-mount labeling notes](https://docs.podman.io/en/latest/markdown/podman-run.1.html) for the effects of shared labels. Production Workmux does none of this automatically.

### Work with a branch

From the main checkout, start a host tmux session if you are not already in one, then create a workspace:

```sh
tmux new-session -s dev
cli workmux add feature/login --base main
```

For `/home/me/dev/myapp`, this creates `/home/me/dev/myapp__worktrees/feature-login` and a window named `feature-login`. The branch stays `feature/login`. `add` prints the worktree path and focuses the window. `--background` creates it without switching focus, but still requires a live tmux pane.

New branches start at the current worktree's `HEAD` unless `--base` is supplied. An existing local branch is reused unchanged, and then `--base` is an error. A branch already checked out elsewhere, an occupied destination, or a slash-to-dash name collision is refused.

Edit and commit with normal Git commands in the workspace. Lifecycle commands accept the branch or its unique slash-to-dash handle. Omit the name only when the host shell's current directory is inside that managed worktree.

To stop work and return later:

```sh
cli workmux close feature/login
cli workmux open feature/login
```

`close` stops the container and closes the tmux window without deleting the worktree or branch. `open` focuses an existing window, or restarts the saved container and recreates the saved panes on the original tmux server. Container-only files survive close/open, but container processes do not. An exited pane stays visible; use close/open to restart its command.

Once changes are committed and both source and target worktrees are clean:

```sh
cli workmux merge feature/login
```

This performs a normal Git merge followed by workspace cleanup. It neither squashes nor rebases. The target is the local branch named by `origin/HEAD`, falling back to `main`, then `master`. Use `--into <branch>` to choose another local target checked out in another worktree. `--keep` skips cleanup and retains the window, worktree, branch, and container for further work. Merge conflicts preserve the workspace; resolve or abort the Git operation in the reported target path, then retry.

To remove a workspace without merging, use `remove`. `--keep-branch` preserves its branch and committed work, including unmerged commits, but does not permit losing dirty files:

```sh
cli workmux remove feature/login --keep-branch
```

Without `--keep-branch`, removal requires the branch to be safely merged into the detected target or the target recorded by a prior merge. `--force` explicitly permits discarding dirty files and deleting an unmerged branch. Combine it with `--keep-branch` to retain commits while discarding worktree files. Force does not skip hooks, unfinished Git operations, locks, or ownership checks. Back up any needed container-only files before removal.

Cleanup automatically closes the owned source window before deleting the worktree, branch, and container. There is no need to close it manually first. From another host window or shell, cleanup waits for the window to disappear and completes before returning. From a host pane in the source window itself, it hands cleanup to a detached worker and prints `cleanup scheduled` or `merge succeeded; cleanup scheduled`. That is not completion. If the scheduling message cannot be written, the worker is not armed to delete anything.

The worker closes its source window shortly afterward. Cleanup waits up to five seconds for window closure and rechecks the captured tmux and worktree identities before deleting files. Sandbox cleanup stops all container execution before closing the window. Detached host jobs can survive tmux closure; stop them yourself before cleanup.

Merge ignores ignored files, but cleanup is stricter. Unknown or changed ignored files block deletion. Only unchanged ignored files installed by `add` may be removed automatically. Build artifacts created by hooks or tests, including Go test outputs, can therefore leave a successful merge with cleanup still pending. Move out files you need and retry `remove`. If you have reviewed everything and intend to discard it, explicitly use `cli workmux remove <name> --force`. There is no `merge --force`.

Use `cli workmux --help` or `cli workmux help <command>` for usage. `--` ends option parsing.

### Sandbox access and limits

OpenCode is an ordinary pane command. Workmux adds no permission-bypass flags. It automatically mounts these whole directories for every sandbox, even one with only shell panes:

| Host directory | Guest directory | Access |
| --- | --- | --- |
| `$XDG_DATA_HOME/opencode`, default `~/.local/share/opencode` | `/tmp/.local/share/opencode` | Read-write |
| `$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode` | `/tmp/.config/opencode` | Read-only |

Unlike Workmux's own YAML location, these mounts honor XDG overrides and reject relative paths. Missing data directories are created with mode `0700`; config must already meet the `.gitignore` prerequisite. Authenticate OpenCode on the host before using it in a workspace. The guest can read and change all OpenCode data, including credentials. Host file edits, credential rotation, and configuration changes are visible through the directory mounts. All workspaces share them.

The guest uses `HOME=/tmp` and XDG data, config, state, and cache locations under `/tmp`. Host state and cache directories are not shared. Workmux does not mount the host home directory, engine socket, SSH keys or agent, or tmux socket, and it does not forward the general host environment or arbitrary API-key variables. Effective Git `user.name` and `user.email` supply author and committer identity. The guest runs as the host's numeric UID:GID with all capabilities dropped and `no-new-privileges`; Podman uses `--userns=keep-id`.

The linked worktree is writable. The main checkout and common Git directory are read-only, with writable mounts for shared objects, refs, logs, an existing `rr-cache`, and this worktree's Git administration directory. Git config snapshots, pointer files, hooks, and policy directories remain read-only. Snapshots preserve Git config values but omit `core.worktree` and include directives. Repository Git config includes are rejected outright, not silently followed or made safe by stripping them.

The sandbox rejects submodules, nested repositories, unsafe Git layouts, symlinked mount sources or policy files, symbolic links in writable Git data, host submounts, and sockets, devices, or pipes in mounted directory trees. Hardlinks in every writable bind source are rejected, including Git objects, worktree files, and OpenCode data. Use `git clone --no-hardlinks` for local clones. Workmux does not repair linked files on the host. OpenCode data symlinks must be relative and resolve inside that data directory. OpenCode directories and private state must not overlap repository mounts. Bare repositories and separate main Git directories are unsupported. See the [source reference and detailed limits](internal/workmux/upstream.md) for details.

This is not branch-level or strict host-execution isolation. Shared refs let the guest change any branch, not only its workspace branch. The host may later execute agent-written content through Git filters, merge drivers, hooks, builds, plugins, or other tools. Review content before running host operations on it. Git snapshots can contain secrets, as can external hook configuration and saved state. Access to OpenCode credentials is deliberate. Networking uses the engine's unrestricted default; Workmux adds no firewall or proxy.

### Saved configuration and recovery

`add` saves the effective configuration and selected layout with the workspace. Later operations use those saved panes, sandbox settings, and hooks. `open` never reruns file setup or `post_create`, and edits to pane or hook configuration affect only new workspaces.

`open` also loads the current external configuration and refuses it if the effective sandbox `enabled` or `image` differs from the saved values. Restore them, or preserve your work and remove/recreate the workspace to adopt new settings. Close, merge, and remove use the saved settings. Changes to the image ID, mount paths, protected Git policy, or injected Git identity can require container recreation. Container labels and private endpoint records also pin the local Podman storage identity. Restore the original storage if it changes, including for close or remove.

Existing saved Podman workspaces load without manual state edits, including records with an empty legacy runtime. New saves omit the old container setting. Disabled legacy workspaces also load without a backend choice. An enabled workspace saved with another backend is refused before any container operation; recover it with the previous implementation rather than changing its state to claim Podman ownership.

State directories use mode `0700`; JSON records, locks, cleanup logs, endpoint records, and Git config snapshots use `0600`. A per-repository `flock` serializes Workmux commands, not Git or other writers. Completed removals retain a recovery record; container snapshots and endpoint records remain under the state's `containers` subtree.

The scheduling notice gives the private log path, `<state-dir>/<repo-id>/<workspace-id>.<token>.cleanup.log`. It records fixed phase diagnostics, not hook commands or subprocess stderr. `complete` marks finished cleanup. `merge succeeded; cleanup incomplete` means the Git merge succeeded but cleanup did not finish. Inspect the error or log and retry `remove` from a surviving host shell in the repository.

If a worker never starts or is killed, retry `remove` after its 20-second handoff lease expires. The lease limits readiness and lock waiting, not cleanup execution. A running worker holds the repository lock. `close` can cancel an unclaimed cleanup and close the window while preserving the worktree; retry `remove` to finish later.

Other failures report the saved stage and preserve remaining work. Incomplete file or hook setup cannot resume with `open`; preserve needed files, remove the partial workspace, and add it again. A pane-creation failure after setup can use close/open. Retry `close` if that command failed to close its window. Do not delete state to bypass checks. For damaged snapshots, follow the [recovery notes](internal/workmux/upstream.md) and never remove snapshots used by a live container.

### Integration tests

The isolated real-tmux and container tests passed with tmux 3.7c and Podman 5.8.4 on x86_64. The ARM64 image remains untested with a real engine.

The container test is opt-in and needs the image built above. From this checkout's root:

```sh
export WORKMUX_CONTAINER_TEST=1
export WORKMUX_CONTAINER_IMAGE=localhost/cli-workmux:fedora44
go test ./internal/workmux -run '^TestContainerIntegration$' -count=1 -v
```

On an enforcing SELinux host, use this variant to explicitly allow relabeling only the test's own temporary directory:

```sh
WORKMUX_CONTAINER_RELABEL_TEST=1 go test ./internal/workmux -run '^TestContainerIntegration$' -count=1 -v
```

The test uses an isolated repository, fake OpenCode data/config, and a uniquely named container that it removes afterward. It checks OpenCode 1.18.30 startup with `opencode serve` and `/config`, including initialization of a dependency-free local plugin through the read-only config mount. No provider API calls or model inference run, and it never reads your credentials or writes your configuration. `WORKMUX_CONTAINER_RELABEL_TEST=1` permits `chcon` only on the test fixture, not production paths.

These tests use isolated tmux servers and sockets, including cleanup launched from the workspace's own window:

```sh
go test ./internal/workmux -run '^TestTmuxIsolatedServerQuickCommandsAndRename$' -count=1 -v
go test ./internal/workmux -run '^TestCleanupActualSelfWindowProcess$' -count=1 -v
```

The real-tmux tests also run during ordinary tests when tmux 3.2+ is installed; `-short` skips them. The real-container test requires `WORKMUX_CONTAINER_TEST` and is not skipped by `-short` once enabled.
