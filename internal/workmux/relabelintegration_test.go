package workmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSharedRelabelIntegration(t *testing.T) {
	if os.Getenv("WORKMUX_SHARED_RELABEL_TEST") != "1" {
		t.Skip("set WORKMUX_SHARED_RELABEL_TEST=1, WORKMUX_CONTAINER_ARCHIVE and WORKMUX_CONTAINER_IMAGE for isolated enforcing-SELinux tests")
	}
	sandboxTestNonroot(t)
	enforcing, err := os.ReadFile("/sys/fs/selinux/enforce")
	if err != nil || strings.TrimSpace(string(enforcing)) != "1" {
		t.Skip("requires native enforcing SELinux")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("requires Podman")
	}
	archive, image := os.Getenv("WORKMUX_CONTAINER_ARCHIVE"), os.Getenv("WORKMUX_CONTAINER_IMAGE")
	if !filepath.IsAbs(archive) || image == "" {
		t.Fatal("provide an absolute image archive path and its image name; this test does not build or pull")
	}
	c, w, _ := sandboxTestFixture(t)
	base, storage := filepath.Dir(w.Root), t.TempDir()
	for _, key := range sandboxEngineEnvKeys {
		if key != "PATH" {
			sandboxTestUnsetenv(t, key)
		}
	}
	t.Setenv("HOME", c.HomeDir)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR"} {
		path := filepath.Join(storage, key)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(key, path)
	}
	storageConfig := filepath.Join(storage, "storage.conf")
	sandboxTestWrite(t, storageConfig, "[storage]\ndriver = \"vfs\"\nrunroot = "+strconv.Quote(filepath.Join(storage, "runroot"))+"\ngraphroot = "+strconv.Quote(filepath.Join(storage, "graphroot"))+"\n")
	t.Setenv("CONTAINERS_STORAGE_CONF", storageConfig)
	auth := filepath.Join(storage, "auth.json")
	sandboxTestWrite(t, auth, "{\"auths\":{}}\n")
	t.Setenv("REGISTRY_AUTH_FILE", auth)
	c.Runner = sandboxIntegrationRunner{home: c.HomeDir, base: base}
	w.Config.Sandbox.Image = image
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	engine := sandboxCurrentEngine()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		// Register before loading, including partial failures. Later container
		// cleanups run first; remove subordinate-UID layers before TempDir cleanup.
		// Leave runtime namespace mountpoints outside these storage directories.
		if out, err := c.run(cleanup, engine, "unshare", "rm", "-rf", "--", filepath.Join(storage, "graphroot"), filepath.Join(storage, "runroot")); err != nil {
			t.Errorf("remove isolated Podman storage: %v\n%s", err, out)
		}
	})
	if out, err := c.run(ctx, engine, "load", "--input", archive); err != nil {
		t.Fatalf("load isolated test image: %v\n%s", err, out)
	}
	label := func(path string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "stat", "-c", "%C", "--", path).CombinedOutput()
		if err != nil {
			t.Fatalf("inspect label %s: %v: %s", path, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	outside := filepath.Join(base, "outside")
	sandboxTestWrite(t, outside, "outside\n")
	outsideBefore := label(outside)
	if err := os.Symlink(outside, filepath.Join(w.Path, "outside-link")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "outside-hardlink")
	if err := os.Link(filepath.Join(w.Path, "tracked"), alias); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "move-source")
	sandboxTestWrite(t, moved, "old label\n")
	movedBefore := label(moved)
	data := filepath.Join(c.HomeDir, ".local", "share", "opencode")
	sandboxTestWrite(t, filepath.Join(data, "auth.json"), "{}\n")
	paths := []string{w.Root, w.Path, w.CommonDir, filepath.Join(w.Root, "tracked"), alias, data, filepath.Join(c.HomeDir, ".config", "opencode")}
	for _, path := range paths {
		t.Logf("before %s: %s", path, label(path))
	}
	start := func() string {
		t.Helper()
		args, err := c.runArgs(ctx, engine, w, "sleep 120", nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		for i, arg := range args {
			if arg != "--mount" {
				continue
			}
			source, _, _ := strings.Cut(strings.TrimPrefix(args[i+1], "type=bind,source="), ",target=")
			if !slices.Contains(paths, source) {
				paths = append(paths, source)
				t.Logf("before mount %s: %s", source, label(source))
			}
		}
		// Only detach the production run arguments; do not alter any mount.
		args = slices.Insert(args, 1, "--detach")
		out, err := c.run(ctx, engine, args...)
		if err != nil {
			t.Fatalf("start: %v\n%s", err, out)
		}
		id := strings.TrimSpace(string(out))
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if out, err := c.run(cleanup, engine, "rm", "--force", "--ignore", id); err != nil {
				t.Errorf("remove isolated container: %v\n%s", err, out)
			}
		})
		return id
	}
	probe := func(id, command string) {
		t.Helper()
		if out, err := c.run(ctx, engine, "exec", id, "bash", "-c", command); err != nil {
			t.Fatalf("probe: %v\n%s", err, out)
		}
	}
	first := start()
	sharedProbe := `set -eu; git status --porcelain >/dev/null; printf guest > guest-write; git add guest-write; printf shared > /tmp/.local/share/opencode/shared; test "$(cat /tmp/.local/share/opencode/shared)" = shared`
	probe(first, sharedProbe)
	second := start()
	probe(second, sharedProbe)
	probe(first, sharedProbe)
	for _, id := range []string{first, second} {
		probe(id, `set -eu; test -r `+shellQuote(filepath.Join(w.Root, "tracked"), "sh")+`; if printf forbidden > `+shellQuote(filepath.Join(w.Root, "tracked"), "sh")+`; then exit 1; fi; if printf forbidden > `+shellQuote(filepath.Join(w.CommonDir, "config"), "sh")+`; then exit 1; fi; if printf forbidden > /tmp/.config/opencode/.gitignore; then exit 1; fi`)
	}
	if got := label(outside); got != outsideBefore {
		t.Fatalf("symlink target relabeled: %s -> %s", outsideBefore, got)
	}
	if got := label(alias); got != label(filepath.Join(w.Path, "tracked")) || !strings.Contains(got, ":container_file_t:") {
		t.Fatalf("outside hardlink must share the relabeled inode: %s", got)
	}
	created := filepath.Join(w.Path, "host-created")
	sandboxTestWrite(t, created, "host\n")
	probe(first, `test "$(cat host-created)" = host`)
	t.Logf("host-created inherited label: %s", label(created))
	if err := os.Rename(moved, filepath.Join(w.Path, "host-moved")); err != nil {
		t.Fatal(err)
	}
	if got := label(filepath.Join(w.Path, "host-moved")); got != movedBefore {
		t.Fatalf("rename unexpectedly changed label: %s -> %s", movedBefore, got)
	}
	out, moveErr := c.run(ctx, engine, "exec", first, "cat", filepath.Join(w.Path, "host-moved"))
	t.Logf("moved file retains %s; access depends on host policy: %v, %s", movedBefore, moveErr, out)
	for _, id := range []string{first, second} {
		if out, err := c.run(ctx, engine, "stop", "--time", "0", id); err != nil {
			t.Fatalf("stop: %v\n%s", err, out)
		}
	}
	for _, path := range paths {
		got := label(path)
		t.Logf("after exit %s: %s", path, got)
		if !strings.Contains(got, ":container_file_t:") {
			t.Errorf("shared label did not persist: %s: %s", path, got)
		}
	}
}
