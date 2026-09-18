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

Workmux manages linked Git worktrees and tmux windows. Run lifecycle commands in a host shell inside the repository or a linked worktree. The current implementation targets Linux with Bash, Git, and tmux 3.2+. `add` and `open` need a live tmux pane with valid `$TMUX` and `$TMUX_PANE`. Podman is needed only for sandboxed commands.

The completed rewrite follows selected behaviors from Workmux source `2be0294cb5fc06ddbfa9a0dbb138a95ef81b61b3`, built as `0.1.262`. It does not implement every upstream command. Handles support ASCII/English naming only, with plain `wm-` window prefixes and no emoji, Nerd Font decoration, or transliteration. See [reference, limits, and verification gaps](internal/workmux/upstream.md).

### Start and reopen work

```sh
tmux new-session -s dev
cli workmux add feature/login --base main
cli workmux close feature/login
cli workmux open feature/login
```

For a checkout named `myapp`, this creates sibling `myapp__worktrees/feature-login` and window `wm-feature-login`. The branch stays `feature/login`. `add` prints the path and focuses the window; `--background` avoids switching focus. `--name <handle>` overrides the directory/window handle.

New branches start from the invoking checkout's branch and save it as their base. `--base <ref>` selects another starting ref; detached HEAD requires it. Existing local branches are reused unchanged, without resetting them to `--base`. A branch already checked out elsewhere, occupied destination, or handle collision is refused.

Names can identify a branch or unique worktree handle. Git discovery also supports manually created linked worktrees without prior Workmux JSON state. Name omission for merge, remove, or close requires being inside a linked worktree. `open` requires names unless `--new` is used. Ownership checks still protect unrelated tmux windows and directories.

```text
cli workmux init [-g|--global]
cli workmux sandbox build
cli workmux sandbox shell [-e|--exec] [-- command...]
cli workmux add <branch> [--base <ref>] [-l|--layout <name>] [-b|--background]
    [-o|--open-if-exists] [--name <handle>] [--dry-run]
    [-H|--no-hooks] [-F|--no-file-ops] [-C|--no-pane-cmds]
cli workmux open <name...> [--run-hooks] [--force-files] [-n|--new]
cli workmux close [name]
cli workmux merge [name] [--into <branch>] [-k|--keep|--cleanup] [--rebase|--squash]
    [--ignore-uncommitted] [--no-verify] [-H|--no-hooks]
cli workmux remove|rm [name...] [-k|--keep-branch] [-f|--force]
```

`add --open-if-exists` opens an existing branch worktree. `--dry-run` reports the plan without provisioning. The three add skip flags suppress lifecycle hooks, file operations, or pane commands for that invocation. `open` focuses an existing window or creates one using current configuration. `--new` creates another window; `--force-files` reapplies file setup and `--run-hooks` reruns `post_create`. `close` closes the owned window and stops owned sandbox sessions without deleting the worktree or branch. Use `--` to end options and `cli workmux --help` for usage.

### Configuration and panes

Each ordinary operation reads `~/.config/cli/workmux/config.yaml`, then `~/.config/cli/workmux/<repo-name>.yaml`. This literal location ignores `$XDG_CONFIG_HOME`; checkout YAML is never read. The repository name is the canonical main checkout's basename. Same-named repositories share config, not state or runtime ownership. `config` maps to `repo-config.yaml`; names beginning `repo-` gain another `repo-` prefix.

Run `cli workmux init` from the repository or a linked worktree to create the repository file with commented examples. Run `cli workmux init -g` or `cli workmux init --global` to create `~/.config/cli/workmux/config.yaml`, even outside Git. Global defaults load first; repository configuration overrides them. Both forms enable nothing, never write checkout YAML, and refuse an existing file or symlink. New directories are `0700` and the file is `0600`; an existing unsafe config directory is rejected rather than chmodded. Repository init needs Git; global init does not. Neither needs tmux, Podman, or an image.

Missing files, empty files, comment-only files, and a bare `---` document use defaults. Explicit top-level `null` and `~` are rejected, matching upstream. Optional null fields inherit; anchors and aliases work. YAML `<<` merge keys are ignored rather than merged.

```yaml
# ~/.config/cli/workmux/config.yaml
panes:
  - command: opencode
    focus: true
  - split: horizontal
    percentage: 30
layouts:
  shell:
    panes: [{}]
pre_merge: [git diff --check HEAD]
sandbox:
  enabled: true
  image: localhost/cli-workmux:fedora44
  target: agent
```

```yaml
# ~/.config/cli/workmux/myapp.yaml
files:
  copy: ["<global>", .env]
  symlink: ["<global>", local-data]
post_create: ["<global>", git status --short]
pre_remove: []
```

Top-level keys are `panes`, `files`, `layouts`, `post_create`, `pre_merge`, `pre_remove`, and `sandbox`. Sandbox fields are `enabled`, `image`, `target`, and optional `opencode_config_dir`; obsolete runtime/container settings and `selinux_type` are rejected. There is no `agent` field or `<agent>` placeholder. Use a literal command such as `opencode`.

Repository lists replace global lists. Exact `"<global>"` entries splice the global hook or file list into a repository list, not pane commands. `[]` clears a list; `panes: []` leaves the initial tmux shell, and `panes: [{}]` explicitly requests one shell. Layouts merge by name with whole-layout replacement. Sandbox fields inherit individually, including explicit `enabled: false`.

Without pane configuration, two panes open: the first is a focused host shell; the second runs `clear` and splits horizontally. `horizontal` maps to tmux `-h`, side by side. `vertical` maps to `-v`, top and bottom. Every later pane requires `split`; it splits the previous pane unless `target` names an earlier zero-based pane index. The first pane cannot specify split, size, or percentage. Later panes may use `size` from 0 to 65535 cells or `percentage` from 1 to 100, not both. Explicit `size: 0` is accepted. `name` is accepted but ignored by tmux. Last focus wins; at most one pane may zoom, which also marks it focused.

Panes start with tmux's default shell. Configured commands use a login-shell handshake, then native command sending rather than a command followed by a replacement shell. Quitting an application returns to that same host shell without rerunning its startup files. Bootstrap may invoke an extra shell, so initialization is not promised to run exactly once during add. Host commands such as `exit` or `exec` retain their normal effects. Final shell exit closes the pane normally unless your own `remain-on-exit` setting keeps it visible; Workmux does not force that setting.

CLI-owned bootstrap and private launcher scripts use `/bin/bash` on the host. This does not change tmux's selected interactive shell or the semantics of native host commands. Sandboxed pane and finite command strings use `bash -c`, as do explicit sandbox shell commands. Custom sandbox images must provide Bash on `PATH`; the bundled image already does.

### Files and host hooks

Patterns select paths in the main checkout and support globs including `**`. Absolute patterns must remain lexically inside that checkout; `..` components are rejected. Copies overwrite existing files, merge directories, and preserve regular-file permissions. A directly selected source symlink is followed, while symlinks encountered recursively retain their target text. User-selected source links can resolve outside the checkout. Review patterns and secrets before sharing them.

`files.symlink` explicitly replaces the destination with a relative link to the source in the main checkout. Destination-parent checks prevent following unrelated destination symlinks. Special copy sources and unmatched patterns are skipped without warnings. File setup is not limited to absent or ignored files and may overwrite tracked destinations.

Hooks always run on the host with `bash -c` in the worktree, regardless of sandbox settings. They run sequentially and stop on failure. `post_create` follows file setup and precedes panes; `pre_merge` precedes merging; `pre_remove` precedes cleanup. Unset `pre_remove` uses the upstream Node cleanup script when the main checkout contains a supported npm, pnpm, or Yarn lockfile. `pre_remove: []` disables that default.

Hooks receive `WM_HANDLE`, `WM_WORKTREE_PATH`, `WM_PROJECT_ROOT`, `WM_CONFIG_DIR`, `WM_BRANCH_NAME`, `WM_TARGET_BRANCH`, and alias `WORKMUX_HANDLE`. The target is empty outside a selected merge. Host hooks with terminal input can read it. Cancellation does not guarantee that detached background jobs stop.

### SELinux

Every generated sandbox bind mount uses Podman's `--mount ... relabel=shared`, equivalent to `:z`. This includes read-only mounts, the entire main checkout, nested Git mounts, protected snapshots, and OpenCode data and configuration. Read-only mounts still prevent container writes. Shared labels allow concurrent sandbox containers to use the same files without assigning private container categories. No custom SELinux policy or configuration knob is needed; Workmux does not disable labeling, select a custom process domain, or change ownership.

On native SELinux hosts, Podman recursively changes the actual host file labels. These labels persist after the container exits, including across the entire main checkout, not just the writable worktree. Relabeling does not follow symlinks to outside content, but hardlink aliases share an inode: an alias outside a mounted tree receives the same changed label. Review these host-side effects before opening a sandbox. Host-created files normally inherit their parent directory's label; files moved into a mounted tree can retain old labels and remain inaccessible. A later launch may not revisit an already shared-labeled tree, so reopening is not a guaranteed repair for moved files.

Remove `sandbox.selinux_type` from configuration yourself. Saved workspace JSON accepts and discards legacy string values, and subsequent saves omit the field. Workmux never installs or uninstalls host policy. If you installed the former custom module, review its other users and remove it manually only if appropriate.

After saving configuration from another pane, close and reopen the affected workspace panes to launch fresh containers. Fresh panes, `sandbox shell`, and finite sandbox commands all use shared relabeling. `sandbox shell --exec` enters an existing container without relabeling or recreating it. Image builds are unchanged; rebuilding the image is not needed for this change.

The opt-in `TestSharedRelabelIntegration` requires native enforcing SELinux and a nonroot user. Set `WORKMUX_SHARED_RELABEL_TEST=1`, `WORKMUX_CONTAINER_ARCHIVE` to an absolute path to a prebuilt image archive, and `WORKMUX_CONTAINER_IMAGE` to its image name. It loads the image into separate temporary Podman storage and mounts only temporary repositories and fake OpenCode credentials. It does not build images or run host relabel helpers. Without the opt-in or required platform, the test skips; ordinary Go tests do not establish real SELinux behavior.

### Sandbox setup and access

Build the image explicitly. Workmux uses Podman only and respects its native connection and storage environment, including remote connection settings. It does not force `--remote=false`. Add, open, and shell never build or pull a missing image. Use the engine/image store that will run your panes.

```sh
podman info
cli workmux sandbox build
```

`sandbox build` has no flags and needs no tmux session or source checkout. It uses the embedded [Containerfile](internal/workmux/Containerfile) in a private temporary context containing only that file, then deletes the context even after a failure or cancellation. It does not read a local Dockerfile or send repository files or credentials as build context. Inside a repository it builds the effective configured `sandbox.image`; outside Git it uses global config. The default tag is `localhost/cli-workmux:fedora44`. `sandbox.enabled` need not be true.

The Containerfile uses Fedora 44 and the official `https://opencode.ai/install` script to install latest OpenCode. It runs Bash with `HOME=/root` and `--no-modify-path`, installs the binary into `/usr/local/bin`, then cleans up. There is no version pin or architecture case table. Builds use Podman's normal cache, which can retain an older OpenCode binary. There is no automatic runtime update. For a deliberate uncached update, use the reviewed Containerfile and ignore file from this source checkout with your effective image tag:

```sh
podman build --no-cache -f internal/workmux/Containerfile -t localhost/cli-workmux:fedora44 internal/workmux
```

The image includes common shell tools, not `cli`, Go, or project-specific toolchains. If `add` stopped at stage `ready` because the image was missing, run `cli workmux sandbox build`, then `cli workmux open <name>`. Open keeps the existing worktree and does not rerun file setup or hooks unless explicitly requested. Do not remove and re-add just to fix a missing image.

Run an explicit sandbox shell from the **root of a linked Git worktree**, including a plain linked worktree not previously managed by Workmux. The main checkout and worktree subdirectories are rejected. These commands need no tmux session, do not create worktrees or run hooks, and work even when automatic sandboxing is disabled:

```sh
cli workmux sandbox shell
cli workmux sandbox shell -- git status --short
cli workmux sandbox shell -e
cli workmux sandbox shell --exec -- 'printf "%s\\n" "$PWD"'
```

Fresh mode starts an owned `--rm` container using the same protected Git and OpenCode mounts as sandboxed panes. Bash is the default. Arguments after `--` are joined with spaces and executed by `bash -c`, not passed as a literal argv. Quote shell syntax accordingly. Exit or cancellation removes the fresh container, not persistent bind-mounted files. The CLI forwards SIGINT and SIGTERM to its foreground Podman client and returns 130 or 143 respectively. Exec cancellation does not remove the original container.

Both shell modes always use `-it`, matching the pinned upstream implementation even when redirected. Container stdout and stderr share a PTY and normally arrive together on the Podman client's stdout; terminal line endings and formatting can differ from pipe output. The separately injected stderr receives Podman client diagnostics, not a guaranteed separate container stderr stream. These commands are not a byte-preserving noninteractive pipe interface.

Before launching a fresh shell, Workmux saves `sandbox_used` in the workspace record, including for plain linked Git worktrees and workspaces whose automatic sandboxing is disabled. It holds the repository lock until the owned container is inspected as running, or the launch finishes. Close, merge cleanup, remove, and orphan recovery can then stop/remove it without racing an unregistered launch. The interactive session does not retain the lock. The usage record persists after exit so recovery still checks the engine; protected mount snapshots also identify older explicit shells whose workspace record lacks this bit. Ordinary host-only workspaces without sandbox history do not acquire a Podman dependency.

`--exec` enters the first registered container for this repository and worktree in Podman's listing order. With several matches it names the selected container and the others. Registration uses ownership labels, including the endpoint recorded at launch; it does not select by image or focused pane. Missing, stopped, mismatched, and pre-endpoint sessions produce errors rather than falling back to unrelated containers. Exec does not inspect or build the image and does not stop the original container on exit. Legacy persistent containers remain subject to the migration restrictions below. No separate local session records are created; Podman's `--rm` removes the registration with the container, while mount protection snapshots remain available to other live sessions.

With sandboxing enabled, `target: agent` is the default. Recognized executable stems are `claude`, `gemini`, `agy`, `opencode`, `codex`, `pi`, `omp`, `kiro-cli`, `vibe`, and `grok`. Recognition supports assignment/`env` prefixes and executable symlink resolution, not arbitrary shell wrappers. Ordinary `nvim`, development commands, and empty shell panes stay on the host. `target: all` also sandboxes nonempty non-agent commands, including default `clear`; omitted and empty commands still leave host shells.

Each selected command runs in its own `podman run --rm -it` session. Quitting it removes its container writable layer and returns to the existing host shell. There is no persistent shared container, guest-shell fallback, or sandbox lifecycle hook. Keep files in bind-mounted paths if they must survive. A failed sandbox launch does not run the application on the host.

| Host directory | Guest directory | Access |
| --- | --- | --- |
| `$XDG_DATA_HOME/opencode`, default `~/.local/share/opencode` | `/tmp/.local/share/opencode` | Read-write |
| `$XDG_STATE_HOME/opencode`, default `~/.local/state/opencode` | `/tmp/.local/state/opencode` | Read-write |
| `$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode` | `/tmp/.config/opencode` | Read-only, if present |

Set `sandbox.opencode_config_dir` in global or repository config to select a different host config directory, for example `opencode_config_dir: ~/dotfiles/.opencode`. It accepts a clean absolute path or a `~/` path and takes precedence over `XDG_CONFIG_HOME`. Repository values override global values; `null` inherits. An empty or unset value uses the default above. If the default directory is a symlink, select its real target with this setting. Symlink and mount-overlap checks still apply. A missing directory remains optional and is not created. Data and auth mounts are unchanged.

These automatic mounts honor absolute XDG overrides and reject relative values. Missing data and state directories are created at launch with mode `0700`. Missing host config is not created or mounted; the guest can initialize its own config. No host `.gitignore` is required by Workmux. Older OpenCode builds have failed with existing empty read-only config, but that is not a claim that latest always needs preinitialized host files. Shared credentials are accessible to the guest; sharing does not perform login or validate providers. Host cache and private Workmux state are not mounted. OpenCode data, config, and state paths must not overlap each other, repository mounts, or private Workmux state, and mounted directories must not use symlinks in their paths. OpenCode state and private Workmux state may share an XDG state parent as separate, non-overlapping directories.

The guest uses `HOME=/tmp`, XDG paths under `/tmp`, and the host's numeric UID:GID with `--userns=keep-id`. Workmux does not automatically mount the host home, engine/tmux sockets, SSH keys or agent, or forward arbitrary host API keys. It supplies effective Git name/email identity. The worktree is writable; common Git metadata uses read-only mounts with selected writable data/admin overlays. Git modules and includes use protected snapshots, not a fully writable `.git` directory.

This is not branch-level or strict host-execution isolation. Shared refs can change other branches. Host hooks, builds, filters, merge drivers, and plugins may execute agent-written content later. Networking uses Podman's default. Bind mounts use shared SELinux relabeling with persistent host-side effects described above; ownership remains unchanged. Real macOS and remote Podman have not been exercised; native connection argv support is not a tested remote deployment.

### Merge and remove safely

```sh
cli workmux merge feature/login --keep
cli workmux merge feature/login --into main
cli workmux remove feature/login --keep-branch
```

Ordinary merge commits staged source changes interactively using Git's editor selection, including `EDITOR`. It does not stage other files. Host Git suppresses Git hooks and signing through its protected constructor. Without `--keep` or `--ignore-uncommitted`, unstaged and nonignored untracked source files block merge. `--keep` allows those files and retains the worktree, branch, and windows.

`--ignore-uncommitted` skips the staged commit too and merges the existing HEAD, not uncommitted changes. Without `--keep`, successful cleanup can discard those staged, unstaged, and untracked files. Ignored artifacts have no extra hash gate and may be removed during cleanup. Back up anything needed before choosing cleanup.

Default strategy is a normal merge followed by cleanup. `--cleanup` explicitly selects that default and conflicts with `--keep`. `--rebase` first rebases the source; `--squash` stages a squash in the target and commits it interactively. The two strategies are mutually exclusive. `--no-verify` skips only Workmux's `pre_merge` hook, not every hook; `--no-hooks` skips all lifecycle hooks for the operation.

A fresh target is `--into`, then a usable saved local base, then the local branch corresponding to `origin/HEAD`, then `main` or `master`. A saved tag or commit is not a local merge target. Retries retain their recorded destination. Workmux uses the target's existing worktree or switches the main checkout to it and leaves it there. Tracked target changes and unfinished operations block merging; Git collision checks and ignored-target protection remain.

Normal merge failure attempts a guarded abort in the target. Squash failure uses guarded squash recovery; failed squash commits retain staged target changes for retry. Failed rebases stay in the source for manual continue or abort. If abort is unsafe or fails, inspect the reported path and recovery state before retrying. No successful cleanup is claimed on merge failure.

`remove` normally removes the worktree and branch. Unmerged commits require `[y/N]` confirmation unless `--keep-branch` or `--force` is supplied. `--keep-branch` retains committed work, not dirty files. `--force` permits discarding dirty files and unmerged commits but does not bypass hooks, locks, unfinished Git operations, or ownership checks.

Cleanup closes owned windows before deleting the worktree. From inside an owned window it hands off to a detached worker and reports `cleanup scheduled`, not completion. Inspect the reported private cleanup log and retry `remove` after a failed handoff expires. `merge succeeded; cleanup incomplete` means the merge is already done. Ref and filesystem identities are rechecked; changed refs preserve the branch even if worktree removal already completed. Detached host jobs may survive tmux closure.

### Legacy migration and recovery

Old records with a nonempty saved container name still represent persistent containers. Creating a new sandbox pane refuses them rather than silently deleting container-local data. `close` stops the owned legacy container and keeps its local data; explicit `remove` deletes it. Legacy endpoint records protect access to the original Podman store. Do not edit JSON or erase endpoint records to bypass ownership checks.

Before migration, back up needed container-local files and commit or externally back up every worktree change, including ignored files. From another surviving host shell:

```sh
cli workmux remove feature/login --keep-branch
# Wait for cleanup completion before recreating.
cli workmux add feature/login
```

Reuse `--name <handle>` if you chose a custom handle. `--keep-branch` is not a dirty-file backup, and re-add does not restore container-local files. Do not use `--force` unless discarding remaining worktree data is intentional. Existing windows keep their launch behavior until closed; reopening uses fresh config, not the old saved pane layout.

Normal operations reload config, while unfinished merge/cleanup journals retain their recovery inputs. Private state uses `0700` directories and `0600` records with per-repository locks. Those locks do not serialize ordinary Git commands or other filesystem writers.

If a recorded worktree disappears, change to a surviving checkout and run `remove <name>`. Orphan recovery skips `pre_remove`, preserves a surviving branch even with force, and retires only validated owned resources and stale Git metadata. It does not globally prune Git registrations or delete replacement directories. `add` can retire metadata-only orphans only when registration and runtime resources are absent. Missing engine access or a live inaccessible tmux server blocks recovery rather than proving absence. `open` cannot recreate a missing worktree; complete removal, then re-add.

### Verification

```sh
go test ./internal/workmux -count=1
WORKMUX_UPSTREAM_BIN=/absolute/path/to/workmux go test ./internal/workmux -run '^TestUpstreamCompatibility$' -count=1 -v
WORKMUX_CONTAINER_TEST=1 WORKMUX_CONTAINER_IMAGE=localhost/cli-workmux:fedora44 go test ./internal/workmux -run '^TestContainerIntegration$' -count=1 -v
WORKMUX_SANDBOX_COMMAND_TEST=1 go test ./internal/workmux -run '^TestSandboxCommandsIntegration$' -count=1 -v -timeout 25m
```

The optional upstream binary must be the native reference build above. Differential tests use isolated Git/tmux fixtures and temporary homes, not user configuration or provider credentials. They cover selected shell, split, file, config, base/merge, reopen, and manual-worktree cases, not all upstream flags. Local tests also cover lifecycle phases, ownership, unknown commands, sandbox selection, and recovery.

The sandbox command integration test is separately opt-in. It downloads and builds the embedded image using a temporary HOME, empty registry auth, temporary Podman storage, and a unique test-only tag. It checks init, build, fresh shell persistence, merged PTY output, exec exit status, and survival of the original container. The integration runner retains `-it` for shell and exec; only the long-lived fixture container is started with `-d` instead of `-it`. That fixture does not verify interactive pane startup. The test requires working rootless Podman, network access, and an environment where Podman accepts these terminal options; it fails rather than stripping them for CI. It does not use live user workspaces or provider credentials. Separate subprocess tests exercise the actual CLI entry point with a fake Podman executable to verify SIGINT/SIGTERM forwarding, exit codes, fresh cleanup, and exec survival, without claiming real PTY behavior.

On SELinux-enforcing hosts, add `WORKMUX_CONTAINER_RELABEL_TEST=1` to relabel only temporary bind fixtures. Native engine storage and runtime namespaces stay outside that tree. The test unsets inherited routing variables rather than assigning empty values, verifies its private storage paths before building, and removes subordinate-UID VFS files through `podman unshare` during cleanup. Production routing and mount labeling are unchanged.

Real tmux tests run when available and skip under `-short`; explicit upstream tests require tmux and reject `-short`. Container tests need an explicitly built image. They capture its OpenCode version dynamically and validate health plus resolved config for fresh guest-owned config, as well as read-only host config and a local plugin. They use fake credentials without provider requests. On enforcing SELinux hosts, `WORKMUX_CONTAINER_RELABEL_TEST=1` allows relabeling only the test fixture. Bash/native shell tests exist; fish, zsh, and nu execution is not claimed when tools are unavailable. See [verification details](internal/workmux/upstream.md#image-and-verification).
