package workmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type sandboxIntegrationRunner struct{ home, base string }

func (runner sandboxIntegrationRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	if p.Name == "git" {
		p.Env = append(p.Env, "HOME="+runner.home, "XDG_CONFIG_HOME="+filepath.Join(runner.home, ".config"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	podman := sandboxTestPodmanProcess(p)
	if podman.Name == "podman" && len(podman.Args) != 0 && podman.Args[0] == "run" {
		for i, arg := range podman.Args {
			if arg != "--mount" {
				continue
			}
			value := podman.Args[i+1]
			source, _, _ := strings.Cut(strings.TrimPrefix(value, "type=bind,source="), ",target=")
			if runner.base == "" || !sandboxWithin(runner.base, source) {
				return nil, fmt.Errorf("test refuses non-fixture mount %q", source)
			}
		}
	}
	return (ExecRunner{}).Run(ctx, p)
}

func TestContainerIntegration(t *testing.T) {
	if os.Getenv("WORKMUX_CONTAINER_TEST") == "" {
		t.Skip("set WORKMUX_CONTAINER_TEST=1 and WORKMUX_CONTAINER_IMAGE to an existing Podman test image")
	}
	if os.Getenv("WORKMUX_CONTAINER_TEST") != "1" {
		t.Fatal("WORKMUX_CONTAINER_TEST must be 1")
	}
	sandboxTestNonroot(t)
	image := os.Getenv("WORKMUX_CONTAINER_IMAGE")
	if image == "" {
		t.Fatal("WORKMUX_CONTAINER_IMAGE must name an explicitly built test image")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	c, w, _ := sandboxTestFixture(t)
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: filepath.Dir(w.Root)}
	w.Config.Sandbox.Image = image
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := c.Remove(ctx, w); err != nil {
			t.Errorf("remove test sandbox sessions: %v", err)
		}
	})
	hook := filepath.Join(w.CommonDir, "hooks", "pre-commit")
	sandboxTestWrite(t, hook, "#!/bin/bash\nexit 0\n")
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	data, config := filepath.Join(c.HomeDir, ".local", "share", "opencode"), filepath.Join(c.HomeDir, ".config", "opencode")
	sandboxTestWrite(t, filepath.Join(data, "auth.json"), "{}\n")
	sandboxTestWrite(t, filepath.Join(data, "fake-credential"), "not a real credential\n")
	sandboxOpenCodeFixture(t, config)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	for alias, target := range map[string]string{"config-alias": filepath.Join(w.CommonDir, "config"), "hook-alias": hook, "main-alias": filepath.Join(w.Root, "tracked")} {
		if err := os.Symlink(target, filepath.Join(w.Path, alias)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Ensure(ctx, w); err != nil {
		t.Fatal(err)
	}
	if present, err := c.Exists(ctx, w); err != nil || present {
		t.Fatalf("preflight created a persistent sandbox: %v, %v", present, err)
	}
	run := func(command string, env ...string) (string, error) {
		var stdout, stderr bytes.Buffer
		err := c.Exec(ctx, w, command, env, nil, &stdout, &stderr)
		return stdout.String() + stderr.String(), err
	}
	var versionOutput, versionErrors bytes.Buffer
	err = c.Exec(ctx, w, "opencode --version", []string{"OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_AUTOUPDATE=1"}, nil, &versionOutput, &versionErrors)
	version := strings.TrimSpace(versionOutput.String())
	if err != nil || !regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)*$`).MatchString(version) {
		t.Fatalf("read installed OpenCode version: %q, %v\n%s", version, err, &versionErrors)
	}
	t.Logf("Installed OpenCode version: %s", version)
	t.Run("pane argv exits without guest shell", func(t *testing.T) {
		argv, err := c.PaneCommand(ctx, w, "printf pane-ok")
		if err != nil {
			t.Fatal(err)
		}
		index := slices.Index(argv, "-it")
		if index < 0 {
			t.Fatalf("missing PTY: %q", argv)
		}
		argv[index] = "-i"
		// Model a tmux login shell that inherited a different connection and store.
		t.Setenv("CONTAINER_HOST", "ssh://unavailable-workmux-test.invalid")
		t.Setenv("CONTAINER_CONNECTION", "unavailable-workmux-test")
		t.Setenv("CONTAINERS_STORAGE_CONF", filepath.Join(c.HomeDir, "missing-storage.conf"))
		t.Setenv("HOME", filepath.Join(c.HomeDir, "late-server-home"))
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(c.HomeDir, "late-server-config"))
		t.Setenv("XDG_DATA_HOME", filepath.Join(c.HomeDir, "late-server-data"))
		output, err := c.Runner.Run(ctx, Process{Name: argv[0], Args: argv[1:], Dir: "/", Stdin: strings.NewReader("printf unexpected-fallback\n")})
		if err != nil || string(output) != "pane-ok" {
			t.Fatalf("pane command: %v\n%s", err, output)
		}
	})
	for _, command := range []string{"true", "false", "exit 7", "exec true", "exec /missing-workmux-executable"} {
		t.Run("finite status "+command, func(t *testing.T) {
			status := 0
			switch command {
			case "false":
				status = 1
			case "exit 7":
				status = 7
			case "exec /missing-workmux-executable":
				status = 127
			}
			var stdout, stderr bytes.Buffer
			err := c.Exec(ctx, w, command, nil, strings.NewReader("printf unexpected-shell\n"), &stdout, &stderr)
			var exited *exec.ExitError
			if status == 0 && err != nil || status != 0 && (!errors.As(err, &exited) || exited.ExitCode() != status) || stdout.Len() != 0 {
				t.Fatalf("lost status %d or started shell: %v, %q, %s", status, err, &stdout, &stderr)
			}
		})
	}
	output, err := run(`set -eu
test "$(id -u)" = "$EXPECTED_UID"
test "$(id -g)" = "$EXPECTED_GID"
test "$HOME" = /tmp
test "$XDG_DATA_HOME" = /tmp/.local/share
test "$XDG_CONFIG_HOME" = /tmp/.config
test "$XDG_STATE_HOME" = /tmp/.local/state
test ! -t 0
test ! -t 1
test -f "$XDG_DATA_HOME/opencode/fake-credential"
printf guest > "$XDG_DATA_HOME/opencode/guest-write"
printf persistent > "$XDG_STATE_HOME/opencode/guest-write"
printf ephemeral > /tmp/session-marker
printf changed > tracked
git add tracked
git -c commit.gpgsign=false commit -qm sandbox
`, "EXPECTED_UID="+strconv.Itoa(os.Getuid()), "EXPECTED_GID="+strconv.Itoa(os.Getgid()))
	if err != nil {
		t.Fatalf("guest image, identity, writes or commit: %v\n%s", err, output)
	}
	if output, err := run(`test ! -e /tmp/session-marker && test "$(cat tracked)" = changed && test "$(cat "$XDG_STATE_HOME/opencode/guest-write")" = persistent`); err != nil {
		t.Fatalf("sessions share writable layers or lost worktree: %v\n%s", err, output)
	}
	for _, target := range []string{
		filepath.Join(w.CommonDir, "config"), filepath.Join(identity.Admin, "config.worktree"), filepath.Join(identity.Admin, "config"),
		identity.Pointer, filepath.Join(identity.Admin, "gitdir"), filepath.Join(identity.Admin, "commondir"), hook,
		filepath.Join(w.Path, "config-alias"), filepath.Join(w.Path, "hook-alias"), filepath.Join(w.Path, "main-alias"),
		filepath.Join(w.CommonDir, "objects", "info", "new-policy"), filepath.Join(identity.Admin, "hooks", "new-hook"),
		"/tmp/.config/opencode/opencode.json", "/tmp/.config/opencode/workmux-plugin.mjs",
	} {
		output, err := run(`printf tampered > "$TARGET"`, "TARGET="+target)
		if err == nil || !strings.Contains(output, "Read-only file system") && !strings.Contains(output, "Permission denied") {
			t.Fatalf("protected write %s: %v\n%s", target, err, output)
		}
	}
	for _, command := range []string{`git config user.name Tampered`, `rm -- .git`, `ln -- "$POLICY" hardlink-alias`} {
		if output, err := run(command, "POLICY="+filepath.Join(w.CommonDir, "config")); err == nil {
			t.Fatalf("Git policy mutation succeeded: %s\n%s", command, output)
		}
	}
	t.Run("initialized read-only config and plugin", func(t *testing.T) { sandboxOpenCodeInitialization(t, ctx, c, w, data, config, version) })
	t.Run("existing empty read-only config", func(t *testing.T) {
		empty := filepath.Join(c.HomeDir, "empty-config", "opencode")
		if err := os.MkdirAll(empty, 0700); err != nil {
			t.Fatal(err)
		}
		before, err := os.Stat(empty)
		if err != nil {
			t.Fatal(err)
		}
		copy := *c
		copy.Getenv = func(key string) string {
			if key == "XDG_CONFIG_HOME" {
				return filepath.Dir(empty)
			}
			return ""
		}
		if err := copy.Check(ctx, w.Config.Sandbox); err != nil {
			t.Fatalf("CLI added a config prerequisite: %v", err)
		}
		var probeErrors bytes.Buffer
		if err := copy.Exec(ctx, w, `printf forbidden > "$XDG_CONFIG_HOME/opencode/write-probe"`, nil, nil, nil, &probeErrors); err == nil || !strings.Contains(probeErrors.String(), "Read-only file system") {
			t.Fatalf("empty host config is not read-only: %v\n%s", err, &probeErrors)
		}
		stdout, stderr, err := sandboxOpenCodeServe(ctx, &copy, w, false)
		if err == nil {
			if _, err := sandboxOpenCodeResponse(stdout, version); err != nil {
				t.Fatalf("empty RO config initialization response: %v\n%s\n%s", err, stdout, stderr)
			}
			t.Logf("OpenCode %s initialized with empty read-only host config", version)
		} else if sandboxOpenCodeReadonlyFailure(stdout, stderr, err, version) {
			t.Logf("OpenCode %s exposes the known read-only .gitignore initialization error:\n%s\n%s", version, stdout, stderr)
		} else {
			t.Fatalf("unexpected empty RO config startup failure: %v\n%s\n%s", err, stdout, stderr)
		}
		entries, err := os.ReadDir(empty)
		if err != nil || len(entries) != 0 {
			t.Fatalf("OpenCode or CLI modified empty host config: %v, %v", entries, err)
		}
		after, err := os.Stat(empty)
		if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
			t.Fatalf("empty host config metadata changed: %v", err)
		}
	})
	t.Run("missing host config gets guest-owned initialization", func(t *testing.T) {
		missing := filepath.Join(c.HomeDir, "missing-config")
		copy := *c
		copy.Getenv = func(key string) string {
			if key == "XDG_CONFIG_HOME" {
				return missing
			}
			return ""
		}
		stdout, stderr, err := sandboxOpenCodeServe(ctx, &copy, w, false)
		if err != nil {
			t.Fatalf("guest-owned config initialization: %v\n%s\n%s", err, stdout, stderr)
		}
		if _, err := sandboxOpenCodeResponse(stdout, version); err != nil {
			t.Fatalf("guest-owned config response: %v\n%s\n%s", err, stdout, stderr)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatalf("created missing host config: %v", err)
		}
	})
	t.Run("submodule Git writes and metadata protections", func(t *testing.T) {
		module, admin := sandboxTestSubmodule(t, w)
		output, err := run(`set -eu
cd "$MODULE"
printf changed > module-file
git add module-file
git -c commit.gpgsign=false commit -qm module-guest
`, "MODULE="+module)
		if err != nil {
			t.Fatalf("submodule commit: %v\n%s", err, output)
		}
		for _, target := range []string{filepath.Join(module, ".git"), filepath.Join(admin, "config"), filepath.Join(admin, "hooks", "new-hook"), filepath.Join(admin, "objects", "info", "new-policy")} {
			if output, err := run(`printf tampered > "$TARGET"`, "TARGET="+target); err == nil {
				t.Fatalf("submodule policy writable: %s\n%s", target, output)
			}
		}
	})
	t.Run("stop all independent sessions", func(t *testing.T) {
		done := make(chan error, 2)
		for i := range 2 {
			marker := "running-" + strconv.Itoa(i)
			go func() {
				var output bytes.Buffer
				done <- c.Exec(ctx, w, `printf ready > "$MARKER"; exec sleep 60`, []string{"MARKER=" + marker}, nil, &output, &output)
			}()
			for {
				if _, err := os.Stat(filepath.Join(w.Path, marker)); err == nil {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		found, err := c.owned(ctx, w)
		if err != nil || len(found) != 2 || found[0].ID == found[1].ID {
			t.Fatalf("independent owned sessions = %v, %v", found, err)
		}
		if err := c.Stop(ctx, w); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			<-done
		}
		if present, err := c.Exists(ctx, w); err != nil || present {
			t.Fatalf("--rm sessions remain after stop: %v, %v", present, err)
		}
	})
	if got, err := os.ReadFile(filepath.Join(data, "guest-write")); err != nil || string(got) != "guest" {
		t.Fatalf("shared fake data write: %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(w.Root, "tracked")); err != nil || string(got) != "base\n" {
		t.Fatalf("host main worktree changed: %q %v", got, err)
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
        stage: "config-hook", directory, config, data, readOnly, username: resolved.username
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
	sandboxTestWrite(t, filepath.Join(config, ".gitignore"), sandboxOpenCodeIgnore)
}

func sandboxOpenCodeServe(parent context.Context, c *Containers, w Workspace, plugin bool) ([]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	env := []string{"OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_AUTOUPDATE=1", "EXPECT_PLUGIN=" + strconv.FormatBool(plugin)}
	if !plugin {
		env = append(env, `OPENCODE_CONFIG_CONTENT={"autoupdate":false,"enabled_providers":[],"plugin":[]}`)
	}
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
if [ "$EXPECT_PLUGIN" = true ]; then
  test -f "$XDG_DATA_HOME/opencode/workmux-plugin-initialized.json"
fi
`, env, nil, &stdout, &stderr)
	return stdout.Bytes(), stderr.Bytes(), err
}

func sandboxOpenCodeHealth(stdout []byte, version string) (*json.Decoder, error) {
	decoder := json.NewDecoder(bytes.NewReader(stdout))
	var health struct {
		Healthy bool
		Version string
	}
	if err := decoder.Decode(&health); err != nil {
		return nil, fmt.Errorf("decode OpenCode health: %w", err)
	}
	if !health.Healthy || health.Version != version {
		return nil, fmt.Errorf("OpenCode health = %+v; want healthy installed version %q", health, version)
	}
	return decoder, nil
}

type sandboxOpenCodeResolved struct {
	Username         string
	EnabledProviders []string `json:"enabled_providers"`
}

func sandboxOpenCodeResponse(stdout []byte, version string) (sandboxOpenCodeResolved, error) {
	var resolved sandboxOpenCodeResolved
	decoder, err := sandboxOpenCodeHealth(stdout, version)
	if err != nil {
		return resolved, err
	}
	if err := decoder.Decode(&resolved); err != nil {
		return resolved, fmt.Errorf("decode OpenCode config: %w", err)
	}
	if resolved.EnabledProviders == nil || len(resolved.EnabledProviders) != 0 {
		return resolved, fmt.Errorf("OpenCode config must explicitly disable all providers")
	}
	return resolved, nil
}

func sandboxOpenCodeReadonlyFailure(stdout, stderr []byte, err error, version string) bool {
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != 22 {
		return false
	}
	decoder, err := sandboxOpenCodeHealth(stdout, version)
	if err != nil {
		return false
	}
	var response struct {
		Name string
		Data struct{ Message, Ref string }
	}
	if err := decoder.Decode(&response); err != nil {
		return false
	}
	if response.Name != "UnknownError" {
		return false
	}
	known := "EROFS: read-only file system, open '/tmp/.config/opencode/.gitignore'"
	if strings.Contains(response.Data.Message, known) {
		return true
	}
	if response.Data.Ref == "" || strings.ContainsAny(response.Data.Ref, " \t\r\n") {
		return false
	}
	for line := range strings.SplitSeq(string(stderr), "\n") {
		if strings.Contains(line, "level=ERROR ") && strings.Contains(line, " ref="+response.Data.Ref+" ") && strings.Contains(line, known) {
			return true
		}
	}
	return false
}

func TestSandboxOpenCodeResponseValidation(t *testing.T) {
	for _, test := range []struct {
		name, response string
		valid          bool
	}{
		{"initialized", `{"healthy":true,"version":"9.8.7"} {"username":"workmux-plugin-initialized","enabled_providers":[]}`, true},
		{"fresh", `{"healthy":true,"version":"9.8.7"} {"enabled_providers":[]}`, true},
		{"wrong version", `{"healthy":true,"version":"9.8.6"} {"enabled_providers":[]}`, false},
		{"unhealthy", `{"healthy":false,"version":"9.8.7"} {"enabled_providers":[]}`, false},
		{"missing config", `{"healthy":true,"version":"9.8.7"}`, false},
		{"missing providers", `{"healthy":true,"version":"9.8.7"} {}`, false},
		{"enabled provider", `{"healthy":true,"version":"9.8.7"} {"enabled_providers":["unexpected"]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := sandboxOpenCodeResponse([]byte(test.response), "9.8.7"); (err == nil) != test.valid {
				t.Fatalf("response validation = %v", err)
			}
		})
	}
}

func TestSandboxOpenCodeReadonlyFailureClassification(t *testing.T) {
	httpFailure := exec.CommandContext(t.Context(), "/bin/bash", "-c", "exit 22").Run()
	if httpFailure == nil {
		t.Fatal("missing HTTP failure fixture")
	}
	health := `{"healthy":true,"version":"9.8.7"}`
	response := health + `{"name":"UnknownError","data":{"message":"Unexpected server error. Check server logs for details.","ref":"err_fixture"}}`
	known := "EROFS: read-only file system, open '/tmp/.config/opencode/.gitignore'"
	log := "timestamp=fixture level=ERROR message=failed ref=err_fixture error=\"" + known + "\""
	for _, test := range []struct {
		name, stdout, stderr string
		err                  error
		valid                bool
	}{
		{"referenced known error", response, log, httpFailure, true},
		{"direct known error", health + `{"name":"UnknownError","data":{"message":"` + known + `"}}`, "", httpFailure, true},
		{"generic server failure", response, "", httpFailure, false},
		{"unrelated error reference", response, strings.ReplaceAll(log, "err_fixture", "err_other"), httpFailure, false},
		{"model network failure", response, strings.ReplaceAll(log, known, "model network request failed"), httpFailure, false},
		{"different readonly path", response, strings.ReplaceAll(log, "/tmp/.config/opencode/.gitignore", "/tmp/.local/share/opencode/auth.json"), httpFailure, false},
		{"permission failure", response, strings.ReplaceAll(log, "EROFS: read-only file system", "EACCES: permission denied"), httpFailure, false},
		{"timeout with earlier known log", response, log, context.DeadlineExceeded, false},
		{"wrong version", strings.ReplaceAll(response, "9.8.7", "9.8.6"), log, httpFailure, false},
		{"no health", `{}`, log, httpFailure, false},
		{"successful exit with stale log", response, log, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if valid := sandboxOpenCodeReadonlyFailure([]byte(test.stdout), []byte(test.stderr), test.err, "9.8.7"); valid != test.valid {
				t.Fatalf("known error accepted = %v", valid)
			}
		})
	}
}

func sandboxOpenCodeInitialization(t *testing.T, ctx context.Context, c *Containers, w Workspace, data, config, version string) {
	t.Helper()
	stdout, stderr, err := sandboxOpenCodeServe(ctx, c, w, true)
	if err != nil {
		t.Fatalf("OpenCode RO config/plugin initialization: %v\n%s\n%s", err, stdout, stderr)
	}
	resolved, err := sandboxOpenCodeResponse(stdout, version)
	if err != nil || resolved.Username != "workmux-plugin-initialized" {
		t.Fatalf("plugin-initialized config: %+v, %v\n%s", resolved, err, stderr)
	}
	marker, err := os.ReadFile(filepath.Join(data, "workmux-plugin-initialized.json"))
	if err != nil {
		t.Fatal(err)
	}
	var initialized struct {
		Stage, Directory, Config, Data, Username string
		ReadOnly                                 bool
	}
	if err := json.Unmarshal(marker, &initialized); err != nil || initialized.Stage != "config-hook" || initialized.Directory != w.Path || initialized.Config != "/tmp/.config/opencode" || initialized.Data != "/tmp/.local/share/opencode" || !initialized.ReadOnly || initialized.Username != resolved.Username {
		t.Fatalf("plugin marker: %s, %v", marker, err)
	}
	for name, expected := range map[string]string{"opencode.json": sandboxOpenCodeConfig, "workmux-plugin.mjs": sandboxOpenCodePlugin, ".gitignore": sandboxOpenCodeIgnore} {
		actual, err := os.ReadFile(filepath.Join(config, name))
		if err != nil || string(actual) != expected {
			t.Fatalf("host config changed: %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config, "plugin-write-probe")); !os.IsNotExist(err) {
		t.Fatalf("plugin wrote host config: %v", err)
	}
}
