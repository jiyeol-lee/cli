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

Top-level keys are `panes`, `files`, `layouts`, `post_create`, `pre_merge`, `pre_remove`, and `sandbox`. Sandbox fields are `enabled`, `image`, and `target`; obsolete runtime/container settings are rejected. There is no `agent` field or `<agent>` placeholder. Use a literal command such as `opencode`.

Repository lists replace global lists. Exact `"<global>"` entries splice the global hook or file list into a repository list, not pane commands. `[]` clears a list; `panes: []` leaves the initial tmux shell, and `panes: [{}]` explicitly requests one shell. Layouts merge by name with whole-layout replacement. Sandbox fields inherit individually, including explicit `enabled: false`.

Without pane configuration, two panes open: the first is a focused host shell; the second runs `clear` and splits horizontally. `horizontal` maps to tmux `-h`, side by side. `vertical` maps to `-v`, top and bottom. Every later pane requires `split`; it splits the previous pane unless `target` names an earlier zero-based pane index. The first pane cannot specify split, size, or percentage. Later panes may use `size` from 0 to 65535 cells or `percentage` from 1 to 100, not both. Explicit `size: 0` is accepted. `name` is accepted but ignored by tmux. Last focus wins; at most one pane may zoom, which also marks it focused.

Panes start with tmux's default shell. Configured commands use a login-shell handshake, then native command sending rather than a command followed by a replacement shell. Quitting an application returns to that same host shell without rerunning its startup files. Bootstrap may invoke an extra shell, so initialization is not promised to run exactly once during add. Host commands such as `exit` or `exec` retain their normal effects. Final shell exit closes the pane normally unless your own `remain-on-exit` setting keeps it visible; Workmux does not force that setting.

### Files and host hooks

Patterns select paths in the main checkout and support globs including `**`. Absolute patterns must remain lexically inside that checkout; `..` components are rejected. Copies overwrite existing files, merge directories, and preserve regular-file permissions. A directly selected source symlink is followed, while symlinks encountered recursively retain their target text. User-selected source links can resolve outside the checkout. Review patterns and secrets before sharing them.

`files.symlink` explicitly replaces the destination with a relative link to the source in the main checkout. Destination-parent checks prevent following unrelated destination symlinks. Special copy sources and unmatched patterns are skipped without warnings. File setup is not limited to absent or ignored files and may overwrite tracked destinations.

Hooks always run on the host with `bash -c` in the worktree, regardless of sandbox settings. They run sequentially and stop on failure. `post_create` follows file setup and precedes panes; `pre_merge` precedes merging; `pre_remove` precedes cleanup. Unset `pre_remove` uses the upstream Node cleanup script when the main checkout contains a supported npm, pnpm, or Yarn lockfile. `pre_remove: []` disables that default.

Hooks receive `WM_HANDLE`, `WM_WORKTREE_PATH`, `WM_PROJECT_ROOT`, `WM_CONFIG_DIR`, `WM_BRANCH_NAME`, `WM_TARGET_BRANCH`, and alias `WORKMUX_HANDLE`. The target is empty outside a selected merge. Host hooks with terminal input can read it. Cancellation does not guarantee that detached background jobs stop.

### Sandbox setup and access

Build the image explicitly from this checkout. Workmux uses Podman only and respects its native connection and storage environment, including remote connection settings. It does not force `--remote=false`, reject those variables, build images, or pull missing images. Use the engine/image store that will run your panes.

```sh
podman info
podman build --no-cache -f internal/workmux/Containerfile -t localhost/cli-workmux:fedora44 internal/workmux
```

The [Containerfile](internal/workmux/Containerfile) uses Fedora 44 and the official `https://opencode.ai/install` script to install latest OpenCode. It runs Bash with `HOME=/root` and `--no-modify-path`, installs the binary into `/usr/local/bin`, then cleans up. There is no version pin or architecture case table. `.containerignore` excludes host build-context files. The build does not modify host configuration. `--no-cache` reruns the installer to fetch latest; cached builds can retain an older binary. There is no automatic runtime update. The image includes common shell tools, not `cli`, Go, or project-specific toolchains.

With sandboxing enabled, `target: agent` is the default. Recognized executable stems are `claude`, `gemini`, `agy`, `opencode`, `codex`, `pi`, `omp`, `kiro-cli`, `vibe`, and `grok`. Recognition supports assignment/`env` prefixes and executable symlink resolution, not arbitrary shell wrappers. Ordinary `nvim`, development commands, and empty shell panes stay on the host. `target: all` also sandboxes nonempty non-agent commands, including default `clear`; omitted and empty commands still leave host shells.

Each selected command runs in its own `podman run --rm -it` session. Quitting it removes its container writable layer and returns to the existing host shell. There is no persistent shared container, guest-shell fallback, or sandbox lifecycle hook. Keep files in bind-mounted paths if they must survive. A failed sandbox launch does not run the application on the host.

| Host directory | Guest directory | Access |
| --- | --- | --- |
| `$XDG_DATA_HOME/opencode`, default `~/.local/share/opencode` | `/tmp/.local/share/opencode` | Read-write |
| `$XDG_CONFIG_HOME/opencode`, default `~/.config/opencode` | `/tmp/.config/opencode` | Read-only, if present |

These automatic mounts honor absolute XDG overrides. Missing data directories may be created. Missing host config is not created or mounted; the guest can initialize its own config. No host `.gitignore` is required by Workmux. Older OpenCode builds have failed with existing empty read-only config, but that is not a claim that latest always needs preinitialized host files. Shared credentials are accessible to the guest; sharing does not perform login or validate providers. Host state and cache are not mounted.

The guest uses `HOME=/tmp`, XDG paths under `/tmp`, and the host's numeric UID:GID with `--userns=keep-id`. Workmux does not automatically mount the host home, engine/tmux sockets, SSH keys or agent, or forward arbitrary host API keys. It supplies effective Git name/email identity. The worktree is writable; common Git metadata uses read-only mounts with selected writable data/admin overlays. Git modules and includes use protected snapshots, not a fully writable `.git` directory.

This is not branch-level or strict host-execution isolation. Shared refs can change other branches. Host hooks, builds, filters, merge drivers, and plugins may execute agent-written content later. Networking uses Podman's default. SELinux labels and ownership remain unchanged, without automatic relabeling or `label=disable`; arrange suitable policy for reviewed bind paths on enforcing hosts. Real macOS and remote Podman have not been exercised; native connection argv support is not a tested remote deployment.

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
```

The optional upstream binary must be the native reference build above. Differential tests use isolated Git/tmux fixtures and temporary homes, not user configuration or provider credentials. They cover selected shell, split, file, config, base/merge, reopen, and manual-worktree cases, not all upstream flags. Local tests also cover lifecycle phases, ownership, unknown commands, sandbox selection, and recovery.

Real tmux tests run when available and skip under `-short`; explicit upstream tests require tmux and reject `-short`. Container tests need an explicitly built image. They capture its OpenCode version dynamically and validate health plus resolved config for fresh guest-owned config, as well as read-only host config and a local plugin. They use fake credentials without provider requests. On enforcing SELinux hosts, `WORKMUX_CONTAINER_RELABEL_TEST=1` allows relabeling only the test fixture. Bash/native shell tests exist; fish, zsh, and nu execution is not claimed when tools are unavailable. See [verification details](internal/workmux/upstream.md#image-and-verification).
