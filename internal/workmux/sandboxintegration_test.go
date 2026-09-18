package workmux

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type sandboxIntegrationLog struct{ t *testing.T }

func (log sandboxIntegrationLog) Write(data []byte) (int, error) {
	log.t.Logf("%s", data)
	return len(data), nil
}

func sandboxTestUnsetenv(t *testing.T, key string) {
	t.Helper()
	value, present := os.LookupEnv(key)
	t.Cleanup(func() {
		var err error
		if present {
			err = os.Setenv(key, value)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			t.Errorf("restore %s: %v", key, err)
		}
	})
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxTestUnsetenv(t *testing.T) {
	for _, key := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION"} {
		for _, value := range []string{"absent", "", "fixture-connection"} {
			t.Run(key+"/"+value, func(t *testing.T) {
				t.Setenv(key, value)
				if value == "absent" {
					if err := os.Unsetenv(key); err != nil {
						t.Fatal(err)
					}
				}
				t.Run("unset", func(t *testing.T) {
					sandboxTestUnsetenv(t, key)
					if _, present := os.LookupEnv(key); present {
						t.Fatal("routing variable remains present")
					}
				})
				got, present := os.LookupEnv(key)
				if present != (value != "absent") || present && got != value {
					t.Fatalf("routing variable not restored: %q, present=%t", got, present)
				}
			})
		}
	}
}

func TestSandboxCommandsIntegration(t *testing.T) {
	if os.Getenv("WORKMUX_SANDBOX_COMMAND_TEST") != "1" {
		t.Skip("set WORKMUX_SANDBOX_COMMAND_TEST=1 to build and run the embedded image in temporary Podman storage")
	}
	sandboxTestNonroot(t)
	c, w, _ := sandboxTestFixture(t)
	base := filepath.Dir(w.Root)
	engineBase := t.TempDir()
	for _, key := range sandboxEngineEnvKeys {
		if key != "PATH" {
			sandboxTestUnsetenv(t, key)
		}
	}
	t.Setenv("HOME", c.HomeDir)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		// Keep native Podman state and network namespaces outside bind sources.
		path := filepath.Join(engineBase, key)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	storage := filepath.Join(base, "storage.conf")
	sandboxTestWrite(t, storage, "[storage]\ndriver = \"vfs\"\nrunroot = "+strconv.Quote(filepath.Join(engineBase, "runroot"))+"\ngraphroot = "+strconv.Quote(filepath.Join(engineBase, "graphroot"))+"\n")
	t.Setenv("CONTAINERS_STORAGE_CONF", storage)
	auth := filepath.Join(base, "registry-auth.json")
	sandboxTestWrite(t, auth, "{\"auths\":{}}\n")
	t.Setenv("REGISTRY_AUTH_FILE", auth)
	sandboxTestWrite(t, filepath.Join(c.HomeDir, ".local", "share", "opencode", "auth.json"), "{}\n")
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: base}
	app := sandboxCommandApp(c, w)
	var output, stderr bytes.Buffer
	app.Stdout, app.Stderr = io.MultiWriter(&output, sandboxIntegrationLog{t}), io.MultiWriter(&stderr, sandboxIntegrationLog{t})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Minute)
	defer cancel()
	if err := app.Run(ctx, Command{Kind: "init"}); err != nil {
		t.Fatal(err)
	}
	image := "localhost/cli-workmux-command-test:" + strings.ToLower(rand.Text())
	sandboxTestWrite(t, filepath.Join(app.ConfigDir, "main.yaml"), "sandbox:\n  image: "+image+"\n")
	w.Config.Sandbox.Image = image
	built := false
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if built {
			if err := c.Remove(ctx, w); err != nil {
				t.Errorf("remove fixture containers: %v", err)
			}
			if _, err := c.run(ctx, sandboxCurrentEngine(), "image", "rm", image); err != nil {
				t.Errorf("remove test-only image: %v", err)
			}
		}
		// VFS layers contain subordinate-UID files. Remove only this test's
		// storage directories inside its rootless namespace before TempDir cleanup.
		// Leave runtime namespace mountpoints to cleanup outside that namespace.
		if _, err := c.run(ctx, sandboxCurrentEngine(), "unshare", "rm", "-rf", "--", filepath.Join(engineBase, "graphroot"), filepath.Join(engineBase, "runroot")); err != nil {
			t.Errorf("remove isolated Podman storage: %v", err)
		}
	})
	info, err := c.run(ctx, sandboxCurrentEngine(), "info", "--format", "{{json .Store}}")
	if err != nil {
		t.Fatal(err)
	}
	var store struct{ GraphRoot, RunRoot string }
	if err := json.Unmarshal(info, &store); err != nil {
		t.Fatal(err)
	}
	if store.GraphRoot != filepath.Join(engineBase, "graphroot") || store.RunRoot != filepath.Join(engineBase, "runroot") {
		t.Fatalf("refusing non-test Podman storage: %s", info)
	}
	t.Logf("isolated Podman storage: %s", info)
	if err := app.Run(ctx, Command{Kind: "sandbox", Name: "build"}); err != nil {
		t.Fatalf("build: %v\n%s", err, &stderr)
	}
	built = true
	output.Reset()
	stderr.Reset()
	// Shell calls retain upstream -it. The container PTY merges its stdout and
	// stderr; these are not two independent child streams when redirected.
	if err := app.Run(ctx, Command{Kind: "sandbox", Name: "shell", ShellCommand: []string{"printf 'shell-ok\\n'; printf 'child-stderr-token\\n' >&2; printf persist > shell-file"}}); err != nil {
		t.Fatalf("fresh shell: %v\n%s", err, &stderr)
	}
	if !strings.Contains(output.String(), "shell-ok") {
		t.Fatal(output.String())
	}
	if !strings.Contains(output.String(), "child-stderr-token") || strings.Contains(stderr.String(), "child-stderr-token") {
		t.Fatalf("expected merged container PTY output; stdout=%q stderr=%q", output.String(), stderr.String())
	}
	if data, err := os.ReadFile(filepath.Join(w.Path, "shell-file")); err != nil || string(data) != "persist" {
		t.Fatalf("bind file: %q %v", data, err)
	}
	if found, err := c.owned(ctx, w); err != nil || len(found) != 0 {
		t.Fatalf("fresh session survived: %v %v", found, err)
	}
	argv, err := c.PaneCommand(ctx, w, "sleep 300")
	if err != nil {
		t.Fatal(err)
	}
	// Only the long-lived fixture is detached. Fresh shell and exec above/below
	// retain -it; this does not test an interactive pane launch through tmux.
	index := slices.Index(argv, "-it")
	if index < 0 {
		t.Fatal("missing interactive run option")
	}
	argv[index] = "-d"
	if _, err := c.Runner.Run(ctx, Process{Name: argv[0], Args: argv[1:], Dir: "/"}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := app.Run(ctx, Command{Kind: "sandbox", Name: "shell", Exec: true, ShellCommand: []string{"printf exec-ok"}}); err != nil {
		t.Fatalf("exec: %v\n%s", err, &stderr)
	}
	if !strings.Contains(output.String(), "exec-ok") {
		t.Fatal(output.String())
	}
	err = app.Run(ctx, Command{Kind: "sandbox", Name: "shell", Exec: true, ShellCommand: []string{"exit 23"}})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("exit status: %v", err)
	}
	if found, err := c.owned(ctx, w); err != nil || len(found) != 1 || !found[0].State.Running {
		t.Fatalf("exec terminated original container: %v %v", found, err)
	}
}
