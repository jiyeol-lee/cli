package workmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func sandboxTestPlan(t *testing.T, c *Containers, w Workspace, image string) sandboxMountPlan {
	t.Helper()
	plan, err := c.mountPlan(context.Background(), w, image, true)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestSandboxProtectedMounts(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	sandboxTestWrite(t, filepath.Join(w.CommonDir, "hooks", "pre-commit"), "#!/bin/sh\nexit 0\n")
	if err := os.MkdirAll(filepath.Join(w.CommonDir, "rr-cache"), 0700); err != nil {
		t.Fatal(err)
	}
	plan := sandboxTestPlan(t, c, w, engine.image)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		w.Path: false, w.Root: true, w.CommonDir: true,
		filepath.Join(w.Path, ".git"):         true,
		filepath.Join(w.CommonDir, "objects"): false, filepath.Join(w.CommonDir, "refs"): false,
		filepath.Join(w.CommonDir, "logs"): false, filepath.Join(w.CommonDir, "rr-cache"): false,
		filepath.Join(w.CommonDir, "objects", "info"): true, filepath.Join(w.CommonDir, "config"): true,
		identity.Admin: false, filepath.Join(identity.Admin, "gitdir"): true, filepath.Join(identity.Admin, "commondir"): true,
		filepath.Join(identity.Admin, "config.worktree"): true, filepath.Join(identity.Admin, "config"): true,
		filepath.Join(identity.Admin, "hooks"): true, filepath.Join(identity.Admin, "info"): true,
		filepath.Join(identity.Admin, "objects", "info"): true, filepath.Join(identity.Admin, "modules"): true,
		filepath.Join(identity.Admin, "worktrees"): true,
		"/tmp/.local/share/opencode":               false, "/tmp/.config/opencode": true,
	}
	if len(plan.Mounts) != len(want) {
		t.Fatalf("mount count = %d, want %d: %+v", len(plan.Mounts), len(want), plan.Mounts)
	}
	for i, mount := range plan.Mounts {
		ro, exists := want[mount.Target]
		if !exists || mount.ReadOnly != ro {
			t.Fatalf("unexpected mount: %+v", mount)
		}
		if mount.Snapshot {
			if !sandboxWithin(c.StateDir, mount.Source) || mount.Source == mount.Target || !mount.ReadOnly {
				t.Fatalf("config is not a private read-only snapshot: %+v", mount)
			}
		} else if mount.Target != "/tmp/.local/share/opencode" && mount.Target != "/tmp/.config/opencode" && mount.Source != mount.Target {
			t.Fatalf("repository mount moved path: %+v", mount)
		}
		for _, later := range plan.Mounts[i+1:] {
			if sandboxWithin(later.Target, mount.Target) {
				t.Fatalf("parent mount must precede child: %s before %s", mount.Target, later.Target)
			}
		}
	}
	if err := plan.snapshots(true); err != nil {
		t.Fatal(err)
	}
	for _, mount := range plan.Mounts {
		if !mount.Snapshot {
			continue
		}
		info, err := os.Stat(mount.Source)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("snapshot mode: %v %v", info, err)
		}
		parent, err := os.Stat(filepath.Dir(mount.Source))
		if err != nil || parent.Mode().Perm() != 0700 {
			t.Fatalf("snapshot parent mode: %v %v", parent, err)
		}
		before := info.Sys().(*syscall.Stat_t).Ino
		if err := plan.snapshots(true); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(mount.Source)
		if err != nil || after.Sys().(*syscall.Stat_t).Ino != before || after.ModTime() != info.ModTime() {
			t.Fatalf("snapshot inode was rewritten: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(identity.Admin, "config.worktree")); err != nil {
		t.Fatal("missing host placeholder for config.worktree", err)
	}
}

func TestSandboxCredentialsAndIdentity(t *testing.T) {
	for _, xdg := range []bool{false, true} {
		t.Run(map[bool]string{false: "defaults", true: "XDG"}[xdg], func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			t.Setenv("INHERITED_API_KEY", "must-not-be-forwarded")
			t.Setenv("GIT_AUTHOR_NAME", "must-not-be-forwarded")
			data, config := filepath.Join(c.HomeDir, ".local", "share"), filepath.Join(c.HomeDir, ".config")
			if xdg {
				data, config = filepath.Join(c.HomeDir, "custom data"), filepath.Join(c.HomeDir, "custom config")
				sandboxTestWrite(t, filepath.Join(config, "opencode", ".gitignore"), "")
				c.Getenv = func(key string) string {
					return map[string]string{"XDG_DATA_HOME": data, "XDG_CONFIG_HOME": config}[key]
				}
			}
			plan := sandboxTestPlan(t, c, w, engine.image)
			for _, mount := range plan.Mounts {
				switch mount.Target {
				case "/tmp/.local/share/opencode":
					if mount.Source != filepath.Join(data, "opencode") || mount.ReadOnly {
						t.Fatalf("data mount = %+v", mount)
					}
				case "/tmp/.config/opencode":
					if mount.Source != filepath.Join(config, "opencode") || !mount.ReadOnly {
						t.Fatalf("config mount = %+v", mount)
					}
				}
				if mount.Source == c.HomeDir || strings.Contains(mount.Source, ".gitconfig") || mount.Source == data || mount.Source == config {
					t.Fatalf("broad credential mount: %+v", mount)
				}
			}
			for _, path := range []string{filepath.Join(data, "opencode"), filepath.Join(config, "opencode")} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("credential directory permissions: %v %v", info, err)
				}
			}
			want := []string{
				"HOME=/tmp", "XDG_DATA_HOME=/tmp/.local/share", "XDG_CONFIG_HOME=/tmp/.config",
				"XDG_STATE_HOME=/tmp/.local/state", "XDG_CACHE_HOME=/tmp/.cache", "PATH=/tmp/.local/bin:/usr/local/bin:/usr/bin:/bin",
				"GIT_AUTHOR_NAME=Sandbox Test", "GIT_COMMITTER_NAME=Sandbox Test", "GIT_AUTHOR_EMAIL=sandbox@example.invalid", "GIT_COMMITTER_EMAIL=sandbox@example.invalid",
			}
			if !slices.Equal(plan.Env, want) {
				t.Fatalf("guest environment = %q, want %q", plan.Env, want)
			}
		})
	}
}

func TestSandboxRelativePointers(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, pointer := range []struct{ path, prefix, target string }{
		{identity.Pointer, "gitdir: ", identity.Admin},
		{filepath.Join(identity.Admin, "commondir"), "", w.CommonDir},
		{filepath.Join(identity.Admin, "gitdir"), "", identity.Pointer},
	} {
		relative, err := filepath.Rel(filepath.Dir(pointer.path), pointer.target)
		if err != nil {
			t.Fatal(err)
		}
		sandboxTestWrite(t, pointer.path, pointer.prefix+relative+"\n")
	}
	plan := sandboxTestPlan(t, c, w, engine.image)
	for _, mount := range plan.Mounts {
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Source) != mount.Source {
			t.Fatalf("noncanonical mount from relative pointer: %+v", mount)
		}
	}
}

func TestSandboxSeparateCommonDirectory(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(filepath.Dir(w.Root), "separate-git")
	if err := os.Rename(w.CommonDir, common); err != nil {
		t.Fatal(err)
	}
	w.CommonDir = common
	sandboxTestWrite(t, filepath.Join(w.Root, ".git"), "gitdir: "+common+"\n")
	sandboxTestWrite(t, identity.Pointer, "gitdir: "+filepath.Join(common, "worktrees", filepath.Base(identity.Admin))+"\n")
	plan := sandboxTestPlan(t, c, w, engine.image)
	for _, path := range []string{w.Root, common} {
		if !slices.ContainsFunc(plan.Mounts, func(m sandboxMount) bool { return m.Target == path && m.Source == path && m.ReadOnly }) {
			t.Fatalf("explicit main/common root not read-only: %s", path)
		}
	}
	for _, mount := range plan.Mounts {
		if mount.Target == filepath.Dir(common) {
			t.Fatal("separate gitdir exposed a guessed parent worktree")
		}
	}
}

func TestSandboxPointerDoesNotCleanBeforeResolving(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(filepath.Dir(w.Root), "other", "child")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(w.CommonDir, "worktrees", "redirect")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	value := link + "/../" + filepath.Base(identity.Admin)
	sandboxTestWrite(t, identity.Pointer, "gitdir: "+value+"\n")
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err == nil {
		t.Fatal("lexical cleaning bypassed actual Git pointer resolution")
	}
}

func TestSandboxRejectsLinkedPolicyAndInvalidIdentity(t *testing.T) {
	for _, test := range []struct{ path, kind string }{
		{"pointer", "symlink"}, {"pointer", "hardlink"}, {"pointer", "multiline"}, {"pointer", "main"},
		{"commondir", "symlink"}, {"commondir", "hardlink"}, {"commondir", "multiline"},
		{"gitdir", "symlink"}, {"gitdir", "hardlink"}, {"gitdir", "backlink"},
		{"config", "symlink"}, {"config", "hardlink"},
		{"config.worktree", "symlink"}, {"config.worktree", "hardlink"},
		{"hook", "symlink"}, {"hook", "hardlink"}, {"hook", "dangling"},
		{"hooks", "directory symlink"}, {"objects", "directory symlink"}, {"refs", "directory symlink"},
		{"objects/info", "directory symlink"},
	} {
		t.Run(test.path+" "+test.kind, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			identity, err := discoverSandboxGit(w.Path, w.CommonDir)
			if err != nil {
				t.Fatal(err)
			}
			paths := map[string]string{
				"pointer": identity.Pointer, "commondir": filepath.Join(identity.Admin, "commondir"), "gitdir": filepath.Join(identity.Admin, "gitdir"),
				"config": filepath.Join(w.CommonDir, "config"), "config.worktree": filepath.Join(identity.Admin, "config.worktree"),
				"hook": filepath.Join(w.CommonDir, "hooks", "pre-commit"), "hooks": filepath.Join(w.CommonDir, "hooks"),
				"objects": filepath.Join(w.CommonDir, "objects"), "refs": filepath.Join(w.CommonDir, "refs"), "objects/info": filepath.Join(w.CommonDir, "objects", "info"),
			}
			path := paths[test.path]
			if test.path == "config.worktree" || test.path == "hook" {
				sandboxTestWrite(t, path, "")
			}
			alias := filepath.Join(w.Path, "policy-alias")
			switch test.kind {
			case "symlink", "dangling":
				if err := os.Rename(path, alias); err != nil {
					t.Fatal(err)
				}
				if test.kind == "dangling" {
					alias += "-missing"
				}
				if err := os.Symlink(alias, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(path, alias); err != nil {
					t.Fatal(err)
				}
			case "multiline":
				sandboxTestWrite(t, path, "gitdir: "+identity.Admin+"\nsecond line\n")
			case "main":
				sandboxTestWrite(t, path, "gitdir: "+w.CommonDir+"\n")
			case "backlink":
				sandboxTestWrite(t, alias, "unrelated")
				sandboxTestWrite(t, path, alias+"\n")
			case "directory symlink":
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(alias, path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.mountPlan(context.Background(), w, engine.image, true); err == nil {
				t.Fatalf("accepted %s %s alias bypass", test.path, test.kind)
			}
		})
	}
}

func TestSandboxConfigIncludes(t *testing.T) {
	for _, layout := range []string{"include", "conditional include", "glob include", "missing include", "symlink include", "hardlink include", "nested include"} {
		t.Run(layout, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			key, value := "include.path", filepath.Join(w.Path, "included")
			if layout == "conditional include" {
				key = "includeIf.gitdir:/never-matches/.path"
			}
			if layout == "glob include" {
				value += "*"
			}
			if layout != "missing include" {
				sandboxTestWrite(t, value, "[user]\nname = Included\n")
			}
			if layout == "nested include" {
				sandboxTestWrite(t, filepath.Join(w.Path, "second"), "[user]\nemail = included@example.invalid\n")
				sandboxTestGit(t, w.Path, "config", "--file", value, "include.path", "second")
			}
			if layout == "symlink include" || layout == "hardlink include" {
				target := value + "-target"
				if err := os.Rename(value, target); err != nil {
					t.Fatal(err)
				}
				link := os.Link
				if layout == "symlink include" {
					link = os.Symlink
				}
				if err := link(target, value); err != nil {
					t.Fatal(err)
				}
			}
			sandboxTestGit(t, w.Root, "config", "--file", filepath.Join(w.CommonDir, "config"), "--add", key, value)
			plan, err := c.mountPlan(t.Context(), w, engine.image, true)
			invalid := strings.HasPrefix(layout, "glob") || strings.HasPrefix(layout, "missing") || strings.HasPrefix(layout, "symlink") || strings.HasPrefix(layout, "hardlink")
			if (err != nil) != invalid {
				t.Fatalf("include %s: %v", layout, err)
			}
			if invalid {
				return
			}
			protected := false
			for _, mount := range plan.Mounts {
				if mount.Target == value && mount.ReadOnly {
					protected = true
				}
				if mount.Snapshot && (bytes.Contains(mount.Content, []byte("[include")) || bytes.Contains(mount.Content, []byte("Included"))) {
					t.Fatalf("snapshot retained include: %s", mount.Content)
				}
			}
			if !protected {
				t.Fatal("host include in writable worktree is unprotected")
			}
		})
	}
}

func TestSandboxConfigSerialization(t *testing.T) {
	entries := []sandboxConfigEntry{
		{Key: "core.bare", Value: "false", HasValue: true},
		{Key: "core.worktree", Value: "/outside", HasValue: true},
		{Key: "include.path", Value: "missing", HasValue: true},
		{Key: "includeIf.gitdir:/outside/.path", Value: "missing", HasValue: true},
		{Key: "feature.bool"},
		{Key: "remote.Case.Sensitive.fetch", Value: "+refs/heads/*:refs/remotes/Origin/*", HasValue: true},
		{Key: "remote.Case.Sensitive.fetch", Value: "  second\tvalue\nwith \"quotes\", #comments; and \\slashes\b ", HasValue: true},
		{Key: `alias.a"b\c.name`, Value: "", HasValue: true},
		{Key: "test.carriage", Value: "a\rb", HasValue: true},
	}
	data, err := serializeSandboxConfig(entries)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snapshot")
	sandboxTestWrite(t, path, string(data))
	output := sandboxTestGit(t, filepath.Dir(path), "config", "--file", path, "--null", "--list", "--no-includes")
	actual, err := parseSandboxConfig(output)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]sandboxConfigEntry{entries[0]}, entries[4:]...)
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("snapshot round trip\ngot  %#v\nwant %#v\nserialized:\n%s", actual, want, data)
	}
	for _, malformed := range [][]byte{[]byte("user.name\nunterminated"), []byte("badkey\x00")} {
		if _, err := parseSandboxConfig(malformed); err == nil {
			t.Fatalf("accepted malformed config stream %q", malformed)
		}
	}
}

func TestSandboxExecutablePolicyAndSymlinkAliases(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	hooks := filepath.Join(w.Path, ".hooks")
	monitor := filepath.Join(w.Path, "monitor")
	marker := filepath.Join(w.Path, "executed-on-host")
	sandboxTestWrite(t, filepath.Join(hooks, "pre-commit"), "#!/bin/sh\nexit 0\n")
	sandboxTestWrite(t, monitor, "#!/bin/sh\ntouch '"+marker+"'\n")
	if err := os.Chmod(monitor, 0700); err != nil {
		t.Fatal(err)
	}
	sandboxTestGit(t, w.Root, "config", "core.hooksPath", ".hooks")
	sandboxTestGit(t, w.Root, "config", "core.fsmonitor", monitor)
	if err := os.Symlink(filepath.Join(w.CommonDir, "config"), filepath.Join(w.Path, "config-alias")); err != nil {
		t.Fatal(err)
	}
	plan := sandboxTestPlan(t, c, w, engine.image)
	for _, protected := range []string{hooks, monitor, filepath.Join(w.CommonDir, "config")} {
		found := false
		for _, mount := range plan.Mounts {
			if mount.Target == protected && mount.ReadOnly {
				found = true
			}
		}
		if !found {
			t.Fatalf("executable or symlink destination was not protected: %s", protected)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository executable ran while inspecting config: %v", err)
	}
	if err := os.Link(monitor, filepath.Join(w.Path, "monitor-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err == nil {
		t.Fatal("hardlink alias bypassed executable policy protection")
	}
}

func TestSandboxFingerprintAllowsCommits(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	before := sandboxTestPlan(t, c, w, engine.image)
	sandboxTestWrite(t, filepath.Join(w.Path, "tracked"), "changed\n")
	sandboxTestGit(t, w.Path, "add", "tracked")
	sandboxTestGit(t, w.Path, "-c", "commit.gpgsign=false", "commit", "-qm", "guest-like commit")
	after := sandboxTestPlan(t, c, w, engine.image)
	if before.Fingerprint != after.Fingerprint {
		t.Fatalf("ordinary Git data change invalidated protection: %s != %s", before.Fingerprint, after.Fingerprint)
	}
}

func TestSandboxRejectsCredentialAndStateAliases(t *testing.T) {
	for _, name := range []string{"data in repo", "config in repo", "state in repo", "relative XDG", "symlink data", "state in data", "repo over guest data", "home mount"} {
		t.Run(name, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			values := make(map[string]string)
			switch name {
			case "data in repo":
				values["XDG_DATA_HOME"] = w.Path
			case "config in repo":
				values["XDG_CONFIG_HOME"] = w.Path
			case "state in repo":
				c.StateDir = filepath.Join(w.Path, "private")
			case "relative XDG":
				values["XDG_DATA_HOME"] = "relative"
			case "symlink data":
				path := filepath.Join(c.HomeDir, "data")
				if err := os.Symlink(w.CommonDir, path); err != nil {
					t.Fatal(err)
				}
				values["XDG_DATA_HOME"] = path
			case "state in data":
				c.StateDir = filepath.Join(c.HomeDir, ".local", "share", "opencode", "private")
			case "repo over guest data":
				w.Root = "/tmp/.local"
			case "home mount":
				c.HomeDir = w.Root
			}
			c.Getenv = func(key string) string { return values[key] }
			if _, err := c.mountPlan(context.Background(), w, engine.image, true); err == nil {
				t.Fatal("unsafe overlap or path accepted")
			}
		})
	}
}

func TestSandboxMountPathInjection(t *testing.T) {
	for _, path := range []string{"relative", "/", "/tmp/a/../b", "/tmp/a,readonly=false", "/tmp/a\nother", "/tmp/a\r", "/tmp/a\x00", "/tmp/\"quoted\"", "/tmp/a\t"} {
		if err := sandboxMountPath(path); err == nil {
			t.Fatalf("unsafe mount path accepted: %q", path)
		}
	}
	for _, path := range []string{"/tmp/a b", "/tmp/a'b", "/tmp/dollar$semicolon;"} {
		if err := sandboxMountPath(path); err != nil {
			t.Fatalf("safe argv path rejected: %v", err)
		}
	}
}

func TestSandboxDoesNotScanWorktreeSockets(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	listener, err := net.Listen("unix", filepath.Join(w.Path, "docker.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err != nil {
		t.Fatalf("blanket socket scan differs from upstream: %v", err)
	}
}

func TestSandboxSnapshotCannotBeLinkedOrRewritten(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	plan := sandboxTestPlan(t, c, w, engine.image)
	if err := plan.snapshots(true); err != nil {
		t.Fatal(err)
	}
	var snapshot sandboxMount
	for _, mount := range plan.Mounts {
		if mount.Snapshot {
			snapshot = mount
			break
		}
	}
	if err := os.Link(snapshot.Source, filepath.Join(w.Path, "snapshot-alias")); err != nil {
		t.Fatal(err)
	}
	if err := plan.snapshots(false); err == nil {
		t.Fatal("snapshot hardlink accepted")
	}
	if err := os.Remove(filepath.Join(w.Path, "snapshot-alias")); err != nil {
		t.Fatal(err)
	}
	sandboxTestWrite(t, snapshot.Source, "host change")
	if err := plan.snapshots(true); err == nil {
		t.Fatal("changed snapshot was overwritten")
	}
	data, err := os.ReadFile(snapshot.Source)
	if err != nil || !bytes.Equal(data, []byte("host change")) {
		t.Fatalf("snapshot changed: %q %v", data, err)
	}
	if err := filepath.WalkDir(c.StateDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSandboxDoesNotScanWorktreeHardlinks(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	sentinel := filepath.Join(filepath.Dir(w.Root), "outside-sentinel")
	sandboxTestWrite(t, sentinel, "must remain outside")
	alias := filepath.Join(w.Path, "alias")
	if err := os.Link(sentinel, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err != nil {
		t.Fatalf("non-policy hardlink rejected: %v", err)
	}
	first, err := os.Stat(sentinel)
	if err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(alias)
	if err != nil || !os.SameFile(first, second) {
		t.Fatalf("hardlink was silently replaced: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "must remain outside" {
		t.Fatalf("outside sentinel was modified: %q %v", data, err)
	}
}

func TestSandboxLocalCloneObjectAliases(t *testing.T) {
	for _, noHardlinks := range []bool{false, true} {
		t.Run(map[bool]string{false: "local clone shares inodes", true: "no-hardlinks clone"}[noHardlinks], func(t *testing.T) {
			c, source, engine := sandboxTestFixture(t)
			base := filepath.Dir(source.Root)
			clone := filepath.Join(base, "clone")
			args := []string{"clone", "-q", "--local"}
			if noHardlinks {
				args = append(args, "--no-hardlinks")
			}
			args = append(args, source.Root, clone)
			sandboxTestGit(t, base, args...)
			w := source
			w.Root, w.CommonDir = clone, filepath.Join(clone, ".git")
			w.Path = filepath.Join(base, "clone-linked")
			sandboxTestGit(t, clone, "worktree", "add", "-qb", "sandbox-clone", w.Path)
			oid := strings.TrimSpace(string(sandboxTestGit(t, source.Root, "rev-parse", "HEAD:tracked")))
			first, err := os.Stat(filepath.Join(source.CommonDir, "objects", oid[:2], oid[2:]))
			if err != nil {
				t.Fatal(err)
			}
			second, err := os.Stat(filepath.Join(w.CommonDir, "objects", oid[:2], oid[2:]))
			if err != nil {
				t.Fatal(err)
			}
			if os.SameFile(first, second) == noHardlinks {
				t.Fatal("test clone did not establish the expected object inode relationship")
			}
			_, err = c.mountPlan(context.Background(), w, engine.image, true)
			if err != nil {
				t.Fatalf("ordinary clone objects rejected: %v", err)
			}
		})
	}
}

func TestSandboxDataLinks(t *testing.T) {
	for _, kind := range []string{"internal", "escape", "absolute", "dangling", "cycle", "hardlink", "pipe", "socket"} {
		t.Run(kind, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			root := filepath.Join(c.HomeDir, ".local", "share", "opencode")
			sandboxTestWrite(t, filepath.Join(root, "target"), "data")
			dir := filepath.Join(root, "nested")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(dir, "link")
			value := "../target"
			switch kind {
			case "escape", "hardlink":
				outside := filepath.Join(filepath.Dir(w.Root), "outside")
				sandboxTestWrite(t, outside, "outside")
				if kind == "hardlink" {
					if err := os.Link(outside, link); err != nil {
						t.Fatal(err)
					}
				} else {
					var err error
					value, err = filepath.Rel(dir, outside)
					if err != nil {
						t.Fatal(err)
					}
				}
			case "absolute":
				value = filepath.Join(root, "target")
			case "dangling":
				value = "missing"
			case "cycle":
				value = "link"
			case "pipe":
				if err := syscall.Mkfifo(link, 0600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				listener, err := net.Listen("unix", link)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := listener.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			if kind != "hardlink" && kind != "pipe" && kind != "socket" {
				if err := os.Symlink(value, link); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.mountPlan(context.Background(), w, engine.image, true)
			if err != nil {
				t.Fatalf("data layout %s: %v", kind, err)
			}
		})
	}
}

func TestSandboxWalkCancellation(t *testing.T) {
	root := t.TempDir()
	sandboxTestWrite(t, filepath.Join(root, "nested", "file"), "data")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	visits := 0
	err := sandboxWalk(ctx, root, func(string, fs.DirEntry) error {
		visits++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || visits != 1 {
		t.Fatalf("walk ignored cancellation: visits=%d error=%v", visits, err)
	}
	c, w, engine := sandboxTestFixture(t)
	if _, err := c.mountPlan(ctx, w, engine.image, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("mount preparation ignored cancellation: %v", err)
	}
	if _, err := os.Stat(c.StateDir); !os.IsNotExist(err) {
		t.Fatalf("canceled preparation wrote state: %v", err)
	}
}

func TestSandboxConfiguredDelegateLimits(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	script := filepath.Join(w.Path, "delegate.sh")
	sandboxTestWrite(t, script, "#!/bin/sh\nexit 0\n")
	sandboxTestGit(t, w.Root, "config", "filter.local.clean", "sh delegate.sh")
	sandboxTestGit(t, w.Root, "config", "merge.local.driver", "sh delegate.sh %O %A %B")
	plan := sandboxTestPlan(t, c, w, engine.image)
	for _, mount := range plan.Mounts {
		if mount.Target == script {
			t.Fatal("test no longer describes the documented first-token delegate limitation")
		}
		if mount.Target == filepath.Join(w.CommonDir, "config") {
			if !bytes.Contains(mount.Content, []byte(`clean = "sh delegate.sh"`)) || !bytes.Contains(mount.Content, []byte(`driver = "sh delegate.sh %O %A %B"`)) {
				t.Fatalf("host-configured filters or drivers were silently disabled: %s", mount.Content)
			}
		}
	}
	sandboxTestGit(t, w.Root, "config", "filter.local.clean", "./missing-program --argument")
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err != nil {
		t.Fatalf("upstream skips nonexistent executable policy targets: %v", err)
	}
	sandboxTestGit(t, w.Root, "config", "filter.local.clean", "git-lfs clean -- %f")
	if _, err := c.mountPlan(context.Background(), w, engine.image, true); err != nil {
		t.Fatalf("ordinary PATH delegate was disabled: %v", err)
	}
}

func TestSandboxOptionalOpenCodeConfig(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "empty existing"}[existing], func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			config := filepath.Join(c.HomeDir, ".config", "opencode")
			if err := os.RemoveAll(config); err != nil {
				t.Fatal(err)
			}
			if existing {
				if err := os.Mkdir(config, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Check(t.Context(), w.Config.Sandbox); err != nil {
				t.Fatal(err)
			}
			plan := sandboxTestPlan(t, c, w, engine.image)
			mounted := false
			for _, mount := range plan.Mounts {
				if mount.Target == "/tmp/.config/opencode" {
					mounted = true
					if mount.Source != config || !mount.ReadOnly {
						t.Fatalf("config mount = %+v", mount)
					}
				}
			}
			if mounted != existing {
				t.Fatalf("config mount present = %v", mounted)
			}
			if _, err := os.Stat(filepath.Join(config, ".gitignore")); !os.IsNotExist(err) {
				t.Fatalf("created user metadata: %v", err)
			}
			if !existing {
				if _, err := os.Stat(config); !os.IsNotExist(err) {
					t.Fatalf("created missing host config: %v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(c.HomeDir, ".local", "share", "opencode")); err != nil {
				t.Fatalf("missing shared data: %v", err)
			}
		})
	}
}

func sandboxTestSubmodule(t *testing.T, w Workspace) (string, string) {
	t.Helper()
	source := filepath.Join(filepath.Dir(w.Root), "module-source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	sandboxTestGit(t, source, "init", "-q", "--template=", "--initial-branch=main")
	sandboxTestGit(t, source, "config", "user.name", "Module Test")
	sandboxTestGit(t, source, "config", "user.email", "module@example.invalid")
	sandboxTestWrite(t, filepath.Join(source, "module-file"), "module\n")
	sandboxTestGit(t, source, "add", "module-file")
	sandboxTestGit(t, source, "-c", "commit.gpgsign=false", "commit", "-qm", "module")
	sandboxTestGit(t, w.Path, "-c", "protocol.file.allow=always", "submodule", "add", "--name", "config", source, "vendor/module")
	worktree := filepath.Join(w.Path, "vendor", "module")
	admin := strings.TrimSpace(string(sandboxTestGit(t, worktree, "rev-parse", "--absolute-git-dir")))
	return worktree, admin
}

func TestSandboxSubmoduleMetadata(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	worktree, admin := sandboxTestSubmodule(t, w)
	plan := sandboxTestPlan(t, c, w, engine.image)
	want := map[string]bool{filepath.Join(worktree, ".git"): true, admin: false}
	for _, name := range []string{"config", "config.worktree", "hooks", "info", "objects/info", "modules", "worktrees"} {
		want[filepath.Join(admin, name)] = true
	}
	for _, mount := range plan.Mounts {
		if ro, ok := want[mount.Target]; ok {
			if mount.ReadOnly != ro {
				t.Fatalf("submodule mount = %+v", mount)
			}
			delete(want, mount.Target)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing submodule protection: %v", want)
	}
}

func TestSandboxSubmoduleConfigNamespaces(t *testing.T) {
	for _, location := range []string{"common", "current admin"} {
		t.Run(location, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			identity, err := discoverSandboxGit(w.Path, w.CommonDir)
			if err != nil {
				t.Fatal(err)
			}
			parent := identity.Common
			if location == "current admin" {
				parent = identity.Admin
			}
			namespace := filepath.Join(parent, "modules")
			roots := []string{
				filepath.Join(namespace, "config"),
				filepath.Join(namespace, "vendor", "config"),
				filepath.Join(namespace, "config", "modules", "config"),
				filepath.Join(namespace, "config", "modules", "config", "modules", "config"),
			}
			for i, root := range roots {
				sandboxTestWrite(t, filepath.Join(root, "config"), "[core]\n\tbare = false\n")
				sandboxTestWrite(t, filepath.Join(root, "HEAD"), "ref: refs/heads/main\n")
				for _, name := range []string{"objects", "refs"} {
					if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
						t.Fatal(err)
					}
				}
				sandboxTestWrite(t, filepath.Join(w.Path, fmt.Sprintf("module-%d", i), ".git"), "gitdir: "+root+"\n")
			}
			plan := sandboxTestPlan(t, c, w, engine.image)
			for _, root := range roots {
				for _, target := range []string{root, filepath.Join(root, "config"), filepath.Join(root, "objects"), filepath.Join(root, "refs"), filepath.Join(root, "hooks"), filepath.Join(root, "objects", "info")} {
					var effective *sandboxMount
					for i := range plan.Mounts {
						mount := &plan.Mounts[i]
						if sandboxWithin(mount.Target, target) && (effective == nil || len(mount.Target) > len(effective.Target)) {
							effective = mount
						}
					}
					ro := target == filepath.Join(root, "config") || target == filepath.Join(root, "hooks") || target == filepath.Join(root, "objects", "info")
					if effective == nil || effective.ReadOnly != ro {
						t.Fatalf("wrong effective mount at %s: %+v", target, effective)
					}
					if target == filepath.Join(root, "config") && (!effective.Snapshot || effective.Source == target) {
						t.Fatalf("module config not snapshotted: %+v", effective)
					}
				}
			}
			for _, mount := range plan.Mounts {
				if (mount.Target == namespace || mount.Target == filepath.Join(namespace, "vendor") || strings.HasSuffix(mount.Target, "/modules")) && !mount.ReadOnly {
					t.Fatalf("module namespace mistaken for Git root: %+v", mount)
				}
			}
		})
	}
}

func TestSandboxSubmoduleRootTypes(t *testing.T) {
	for _, layout := range []string{"HEAD directory", "objects file", "HEAD and objects", "config symlink", "HEAD symlink", "objects symlink", "namespace symlink"} {
		t.Run(layout, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			root := filepath.Join(w.CommonDir, "modules", "candidate")
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
			sandboxTestWrite(t, filepath.Join(root, "HEAD"), "ref: refs/heads/main\n")
			if err := os.Mkdir(filepath.Join(root, "objects"), 0700); err != nil {
				t.Fatal(err)
			}
			switch layout {
			case "HEAD directory":
				if err := os.Remove(filepath.Join(root, "HEAD")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(root, "HEAD"), 0700); err != nil {
					t.Fatal(err)
				}
			case "objects file":
				if err := os.Remove(filepath.Join(root, "objects")); err != nil {
					t.Fatal(err)
				}
				sandboxTestWrite(t, filepath.Join(root, "objects"), "not a directory")
			case "config symlink", "HEAD symlink", "objects symlink", "namespace symlink":
				name := strings.TrimSuffix(layout, " symlink")
				path := filepath.Join(root, name)
				if name == "namespace" {
					path = root
				}
				target := filepath.Join(filepath.Dir(w.Root), "outside-metadata")
				if name == "config" {
					sandboxTestWrite(t, target, "[core]\nbare = false\n")
				} else if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			plan, err := c.mountPlan(t.Context(), w, engine.image, true)
			if strings.HasSuffix(layout, "symlink") {
				if err == nil {
					t.Fatal("unsafe metadata symlink accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			mounted := slices.ContainsFunc(plan.Mounts, func(m sandboxMount) bool { return m.Target == root && !m.ReadOnly })
			if mounted != (layout == "HEAD and objects") {
				t.Fatalf("type-based Git root detection = %v", mounted)
			}
			if !mounted {
				if _, err := os.Stat(filepath.Join(root, "config")); !os.IsNotExist(err) {
					t.Fatalf("namespace mutated as a Git root: %v", err)
				}
			}
		})
	}
}

func TestSandboxIncludedExecutablePolicy(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	include := filepath.Join(w.Path, "included-policy")
	program := filepath.Join(w.Path, "host-monitor")
	sandboxTestWrite(t, program, "#!/bin/sh\nexit 0\n")
	sandboxTestWrite(t, include, "[core]\nfsmonitor = "+program+"\n")
	sandboxTestGit(t, w.Root, "config", "include.path", include)
	plan := sandboxTestPlan(t, c, w, engine.image)
	for _, path := range []string{include, program} {
		if !slices.ContainsFunc(plan.Mounts, func(m sandboxMount) bool { return m.Target == path && m.ReadOnly }) {
			t.Fatalf("included policy is writable: %s", path)
		}
	}
	if err := os.Link(program, filepath.Join(w.Path, "monitor-alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.mountPlan(t.Context(), w, engine.image, true); err == nil {
		t.Fatal("included executable hardlink bypass accepted")
	}
}

func TestSandboxPolicyPathsResolveBeforeCleaning(t *testing.T) {
	for _, kind := range []string{"include", "monitor"} {
		t.Run(kind, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			outside := filepath.Join(filepath.Dir(w.Root), "outside-policy")
			if err := os.MkdirAll(filepath.Join(outside, "child"), 0700); err != nil {
				t.Fatal(err)
			}
			sandboxTestWrite(t, filepath.Join(outside, "policy"), "[user]\nname = Actual\n")
			sandboxTestWrite(t, filepath.Join(w.Path, "policy"), "[user]\nname = Decoy\n")
			link := filepath.Join(w.Path, "redirect")
			if err := os.Symlink(filepath.Join(outside, "child"), link); err != nil {
				t.Fatal(err)
			}
			key := "include.path"
			if kind == "monitor" {
				key = "core.fsmonitor"
			}
			sandboxTestGit(t, w.Root, "config", key, link+"/../policy")
			if _, err := c.mountPlan(t.Context(), w, engine.image, true); err == nil {
				t.Fatal("lexical cleaning protected a different policy than Git reads")
			}
		})
	}
}
