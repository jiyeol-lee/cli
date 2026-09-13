package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type sandboxIntegrationRunner struct {
	home string
}

func (runner sandboxIntegrationRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	if p.Name == "git" {
		p.Env = append(p.Env, "HOME="+runner.home, "XDG_CONFIG_HOME="+filepath.Join(runner.home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	return (ExecRunner{}).Run(ctx, p)
}

func TestContainerIntegration(t *testing.T) {
	enabled := os.Getenv("WORKMUX_CONTAINER_TEST")
	if enabled == "" {
		t.Skip("set WORKMUX_CONTAINER_TEST=1 and WORKMUX_CONTAINER_IMAGE to an existing local Podman test image")
	}
	if enabled != "1" && enabled != "podman" {
		t.Fatal("WORKMUX_CONTAINER_TEST must be 1")
	}
	sandboxTestNonroot(t)
	image := os.Getenv("WORKMUX_CONTAINER_IMAGE")
	if image == "" {
		t.Fatal("WORKMUX_CONTAINER_IMAGE must name an explicitly built local image")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, w, _ := sandboxTestFixture(t)
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir}
	// Ignore only XDG paths for credential selection, not engine connection settings.
	c.Getenv = func(key string) string {
		if key == "XDG_DATA_HOME" || key == "XDG_CONFIG_HOME" {
			return ""
		}
		return os.Getenv(key)
	}
	w.Config.Sandbox.Image = image
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(w.Path)))
	w.ID = "integration-" + key[:20]
	w.Container = "cli-workmux-" + w.ID
	hook := filepath.Join(w.CommonDir, "hooks", "pre-commit")
	sandboxTestWrite(t, hook, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(c.HomeDir, ".local", "share", "opencode")
	config := filepath.Join(c.HomeDir, ".config", "opencode")
	sandboxTestWrite(t, filepath.Join(data, "fake-credential"), "not a real credential\n")
	sandboxTestWrite(t, filepath.Join(data, "auth.json"), "{}\n")
	sandboxOpenCodeFixture(t, config)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	for alias, target := range map[string]string{
		"config-alias": filepath.Join(w.CommonDir, "config"), "hook-alias": hook, "main-alias": filepath.Join(w.Root, "tracked"),
	} {
		if err := os.Symlink(target, filepath.Join(w.Path, alias)); err != nil {
			t.Fatal(err)
		}
	}
	imageID, err := c.checkImage(ctx, w.Config.Sandbox)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := c.mountPlan(ctx, w, imageID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.snapshots(true); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WORKMUX_CONTAINER_RELABEL_TEST") == "1" {
		// This directory was created by this test, never a user's repository or home.
		base := filepath.Dir(w.Root)
		output, err := exec.CommandContext(ctx, "chcon", "-R", "-t", "container_file_t", "--", base).CombinedOutput()
		if err != nil {
			t.Fatalf("label isolated test directory: %v: %s", err, output)
		}
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.Remove(cleanup, w); err != nil {
			t.Errorf("remove test container %s: %v", w.Container, err)
		}
	})
	sentinel := filepath.Join(filepath.Dir(w.Root), "outside-sentinel")
	sandboxTestWrite(t, sentinel, "outside")
	for _, dir := range []string{w.Path, filepath.Join(data, "nested")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "preexisting-hardlink")
		if err := os.Link(sentinel, alias); err != nil {
			t.Fatal(err)
		}
		if err := c.Ensure(ctx, w); err == nil || !strings.Contains(err.Error(), "--no-hardlinks") {
			t.Fatalf("preexisting host alias was not refused before create: %v", err)
		}
		if _, _, found, err := c.lookup(ctx, w); err != nil || found != nil {
			t.Fatalf("unsafe source created a container: %v", err)
		}
		if content, err := os.ReadFile(sentinel); err != nil || string(content) != "outside" {
			t.Fatalf("outside sentinel changed: %q %v", content, err)
		}
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Ensure(ctx, w); err != nil {
		t.Fatalf("start sandbox; enforcing SELinux hosts need pre-labeled test directories: %v", err)
	}
	_, _, first, err := c.lookup(ctx, w)
	if err != nil || first == nil {
		t.Fatalf("inspect test container: %v", err)
	}
	pane := c.PaneCommand(w, "printf pane-endpoint-ok")
	if index := slices.Index(pane, "-it"); index >= 0 {
		// Exercise the exact endpoint/environment prefix without requiring a terminal.
		pane[index] = "-i"
	} else {
		t.Fatalf("no sandbox pane argv: %v", pane)
	}
	t.Run("pane pins the later tmux environment", func(t *testing.T) {
		t.Setenv("CONTAINER_HOST", "ssh://must-not-connect.invalid")
		t.Setenv("CONTAINER_CONNECTION", "must-not-connect")
		t.Setenv("CONTAINERS_STORAGE_CONF", filepath.Join(filepath.Dir(w.Root), "missing-storage.conf"))
		t.Setenv("STORAGE_DRIVER", "must-not-use")
		t.Setenv("HOME", filepath.Join(filepath.Dir(w.Root), "different-home"))
		t.Setenv("XDG_DATA_HOME", filepath.Join(filepath.Dir(w.Root), "different-data"))
		output, err := (ExecRunner{}).Run(ctx, Process{Name: pane[0], Args: pane[1:], Dir: "/"})
		if err != nil || string(output) != "pane-endpoint-ok" {
			t.Fatalf("pane inherited a changed endpoint or storage selection: %v\n%s", err, output)
		}
	})
	t.Run("cleanup refuses changed routing", func(t *testing.T) {
		t.Setenv("CONTAINER_HOST", "ssh://must-not-connect.invalid")
		for _, action := range []func(context.Context, Workspace) error{c.Stop, c.Remove} {
			if err := action(ctx, w); err == nil || !strings.Contains(err.Error(), "CONTAINER_HOST") {
				t.Fatalf("cleanup treated a different endpoint as absent: %v", err)
			}
		}
		if got := c.PaneCommand(w, ""); !slices.Equal(got, []string{"/usr/bin/false"}) {
			t.Fatalf("pane accepted changed routing: %v", got)
		}
	})
	run := func(command string, env ...string) (string, error) {
		var output, errors bytes.Buffer
		err := c.Exec(ctx, w, command, env, nil, &output, &errors)
		return output.String() + errors.String(), err
	}
	output, err := run(`set -eu
test "$(id -u)" = "$EXPECTED_UID"
test "$(id -g)" = "$EXPECTED_GID"
test "$HOME" = /tmp
test "$XDG_DATA_HOME" = /tmp/.local/share
test "$XDG_CONFIG_HOME" = /tmp/.config
test "$XDG_STATE_HOME" = /tmp/.local/state
test "$XDG_CACHE_HOME" = /tmp/.cache
test ! -t 0
test ! -t 1
grep -Eq '^CapEff:[[:space:]]+0+$' /proc/self/status
grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/self/status
test -f "$XDG_DATA_HOME/opencode/fake-credential"
test -f "$XDG_CONFIG_HOME/opencode/opencode.json"
printf guest > "$XDG_DATA_HOME/opencode/guest-write"
mkdir -p "$XDG_STATE_HOME/guest" "$XDG_CACHE_HOME/guest"
printf persistent > /tmp/persistent-marker
opencode --version
printf changed > tracked
git add tracked
git -c commit.gpgsign=false commit -qm sandbox
`, "EXPECTED_UID="+strconv.Itoa(os.Getuid()), "EXPECTED_GID="+strconv.Itoa(os.Getgid()))
	if err != nil || !strings.Contains(output, "1.18.30") {
		t.Fatalf("guest writes, image, identity or commit failed: %v\n%s", err, output)
	}
	t.Run("opencode fresh read-only config is refused by preflight", func(t *testing.T) {
		sandboxOpenCodeFreshConfig(t, ctx, c, w, data, config)
	})
	t.Run("opencode prepared read-only config initializes local plugin", func(t *testing.T) {
		sandboxOpenCodeInitialization(t, ctx, c, w, data, config)
	})
	for _, target := range []string{
		filepath.Join(w.CommonDir, "config"), filepath.Join(identity.Admin, "config.worktree"),
		filepath.Join(identity.Admin, "config"), identity.Pointer, filepath.Join(identity.Admin, "gitdir"),
		filepath.Join(identity.Admin, "commondir"), hook, filepath.Join(w.Path, "config-alias"),
		filepath.Join(w.Path, "hook-alias"), filepath.Join(w.Path, "main-alias"),
		filepath.Join(w.CommonDir, "objects", "info", "new-policy"), filepath.Join(identity.Admin, "hooks", "new-hook"),
		"/tmp/.config/opencode/opencode.json", "/tmp/.config/opencode/workmux-plugin.mjs",
	} {
		output, err := run(`printf tampered > "$TARGET"`, "TARGET="+target)
		if err == nil || !strings.Contains(output, "Read-only file system") && !strings.Contains(output, "Permission denied") {
			t.Fatalf("protected write %s: %v\n%s", target, err, output)
		}
	}
	for _, command := range []string{
		`git config user.name Tampered`,
		`rm -- .git`,
		`ln -- "$POLICY" hardlink-alias`,
	} {
		if output, err := run(command, "POLICY="+filepath.Join(w.CommonDir, "config")); err == nil {
			t.Fatalf("Git policy mutation succeeded: %s\n%s", command, output)
		}
	}
	if err := c.Stop(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := c.Ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	_, _, second, err := c.lookup(ctx, w)
	if err != nil || second == nil || second.ID != first.ID {
		t.Fatalf("container was not persistent: %v", err)
	}
	if output, err := run(`test "$(cat /tmp/persistent-marker)" = persistent`); err != nil {
		t.Fatalf("container home was not persistent: %v\n%s", err, output)
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "tracked")); err != nil || string(data) != "changed" {
		t.Fatalf("host worktree did not receive source change: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(data, "guest-write")); err != nil || string(data) != "guest" {
		t.Fatalf("fake OpenCode data was not writable: %q %v", data, err)
	}
	if head := string(sandboxTestGit(t, w.Path, "rev-parse", "HEAD")); head != string(sandboxTestGit(t, w.Root, "rev-parse", "refs/heads/topic")) {
		t.Fatal("guest commit did not update shared refs")
	}
	t.Run("deleted caller cwd", func(t *testing.T) {
		deleted := filepath.Join(filepath.Dir(w.Root), "deleted-cwd")
		if err := os.Mkdir(deleted, 0700); err != nil {
			t.Fatal(err)
		}
		t.Chdir(deleted)
		if err := os.Remove(deleted); err != nil {
			t.Fatal(err)
		}
		if err := c.Stop(ctx, w); err != nil {
			t.Fatal(err)
		}
		if err := c.Ensure(ctx, w); err != nil {
			t.Fatal(err)
		}
		if output, err := run("printf cwd-ok"); err != nil || output != "cwd-ok" {
			t.Fatalf("exec from a deleted cwd: %v\n%s", err, output)
		}
		if err := c.Remove(ctx, w); err != nil {
			t.Fatal(err)
		}
	})
	if err := c.Remove(ctx, w); err != nil {
		t.Fatalf("remove was not idempotent: %v", err)
	}
}

const sandboxOpenCodeConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "autoupdate": false,
  "enabled_providers": [],
  "username": "workmux-readonly-config",
  "plugin": ["file:///tmp/.config/opencode/workmux-plugin.mjs"]
}
`

const sandboxOpenCodePlugin = `import { readFile, writeFile } from "node:fs/promises"
import { join } from "node:path"

export default async function ({ directory }) {
  const data = join(process.env.XDG_DATA_HOME, "opencode")
  const config = join(process.env.XDG_CONFIG_HOME, "opencode")
  const raw = JSON.parse(await readFile(join(config, "opencode.json"), "utf8"))
  if (raw.username !== "workmux-readonly-config") throw new Error("wrong config source")

  let readOnly = false
  try {
    await writeFile(join(config, "plugin-write-probe"), "must not be writable")
  } catch (error) {
    if (error.code !== "EROFS" && error.code !== "EACCES") throw error
    readOnly = true
  }
  if (!readOnly) throw new Error("config directory was mounted writable")

  return {
    config: async (resolved) => {
      if (resolved.username !== raw.username) throw new Error("global config was not loaded")
      if (!Array.isArray(resolved.enabled_providers) || resolved.enabled_providers.length !== 0) {
        throw new Error("test must not enable model providers")
      }
      resolved.username = "workmux-plugin-initialized"
      await writeFile(join(data, "workmux-plugin-initialized.json"), JSON.stringify({
        stage: "config-hook",
        directory,
        config,
        data,
        readOnly,
        username: resolved.username
      }), { mode: 0o600 })
    }
  }
}
`

const sandboxOpenCodeIgnore = "node_modules\npackage.json\nbun.lock\nbun.lockb\n"

func sandboxOpenCodeFixture(t *testing.T, config string) {
	t.Helper()
	sandboxTestWrite(t, filepath.Join(config, "opencode.json"), sandboxOpenCodeConfig)
	sandboxTestWrite(t, filepath.Join(config, "workmux-plugin.mjs"), sandboxOpenCodePlugin)
	// OpenCode 1.18.30 creates this on first instance bootstrap, which requires a writable config directory.
	sandboxTestWrite(t, filepath.Join(config, ".gitignore"), sandboxOpenCodeIgnore)
}

func sandboxOpenCodeFreshConfig(t *testing.T, ctx context.Context, c *Containers, w Workspace, data, config string) {
	t.Helper()
	if err := os.Remove(filepath.Join(config, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	// These are test-owned host fixtures, not a production workaround or a guest mount change.
	t.Cleanup(func() { sandboxOpenCodeFixture(t, config) })
	sandboxTestWrite(t, filepath.Join(config, "opencode.json"), `{"autoupdate":false,"enabled_providers":[],"plugin":[]}`)
	for _, err := range []error{c.Check(ctx, w.Config.Sandbox), c.Ensure(ctx, w)} {
		if err == nil || !strings.Contains(err.Error(), filepath.Join(config, ".gitignore")) || !strings.Contains(err.Error(), "initialize OpenCode once on the host") {
			t.Fatalf("fresh config was not rejected with actionable host setup instructions: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(config, ".gitignore")); !os.IsNotExist(err) {
		t.Fatalf("preflight wrote OpenCode startup metadata: %v", err)
	}
	if _, err := os.Stat(filepath.Join(data, "workmux-plugin-initialized.json")); !os.IsNotExist(err) {
		t.Fatalf("fresh-config failure unexpectedly initialized the plugin: %v", err)
	}
	t.Log("Check and Ensure refused uninitialized read-only config without starting OpenCode or creating .gitignore")
}

func sandboxOpenCodeServe(parent context.Context, c *Containers, w Workspace) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := c.Exec(ctx, w, `set -eu
log=$(mktemp)
health=$(mktemp)
opencode serve --hostname 127.0.0.1 --port 49193 --print-logs > "$log" 2>&1 &
server=$!
cleanup() {
  status=$?
  kill "$server" 2>/dev/null || true
  wait "$server" 2>/dev/null || true
  cat "$log" >&2
  rm -f "$log" "$health"
  exit "$status"
}
trap cleanup EXIT
ready=false
for attempt in $(seq 1 100)
do
  if curl --fail --silent --max-time 1 http://127.0.0.1:49193/global/health > "$health"
  then
    ready=true
    break
  fi
  kill -0 "$server"
  sleep 0.1
done
test "$ready" = true
cat "$health"
printf '\n'
curl --fail-with-body --silent --show-error --max-time 20 http://127.0.0.1:49193/config
printf '\n'
test -f "$XDG_DATA_HOME/opencode/workmux-plugin-initialized.json"
`, []string{"OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_AUTOUPDATE=1"}, nil, &stdout, &stderr)
	return stdout.Bytes(), stderr.Bytes(), err
}

func sandboxOpenCodeInitialization(t *testing.T, ctx context.Context, c *Containers, w Workspace, data, config string) {
	t.Helper()
	stdout, stderr, err := sandboxOpenCodeServe(ctx, c, w)
	if err != nil {
		t.Fatalf("OpenCode server/config/plugin initialization with production RO config mounts: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var health struct {
		Healthy bool
		Version string
	}
	if err := decoder.Decode(&health); err != nil || !health.Healthy || health.Version != "1.18.30" {
		t.Fatalf("OpenCode health response: %+v, error=%v\n%s", health, err, stderr)
	}
	var resolved struct {
		Username         string
		EnabledProviders []string `json:"enabled_providers"`
	}
	if err := decoder.Decode(&resolved); err != nil || resolved.Username != "workmux-plugin-initialized" || len(resolved.EnabledProviders) != 0 {
		t.Fatalf("OpenCode did not serve the plugin-initialized config: %+v, error=%v\n%s", resolved, err, stderr)
	}
	marker, err := os.ReadFile(filepath.Join(data, "workmux-plugin-initialized.json"))
	if err != nil {
		t.Fatal("plugin initialization did not write to the shared data directory", err)
	}
	var initialized struct {
		Stage, Directory, Config, Data, Username string
		ReadOnly                                 bool
	}
	if err := json.Unmarshal(marker, &initialized); err != nil || initialized.Stage != "config-hook" || initialized.Directory != w.Path || initialized.Config != "/tmp/.config/opencode" || initialized.Data != "/tmp/.local/share/opencode" || !initialized.ReadOnly || initialized.Username != resolved.Username {
		t.Fatalf("plugin initialization marker: %s, error=%v", marker, err)
	}
	for name, expected := range map[string]string{"opencode.json": sandboxOpenCodeConfig, "workmux-plugin.mjs": sandboxOpenCodePlugin, ".gitignore": sandboxOpenCodeIgnore} {
		actual, err := os.ReadFile(filepath.Join(config, name))
		if err != nil || string(actual) != expected {
			t.Fatalf("read-only OpenCode fixture changed: %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config, "plugin-write-probe")); !os.IsNotExist(err) {
		t.Fatalf("plugin could write through the read-only config mount: %v", err)
	}
}
