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
	"strings"
	"testing"
)

func TestApplyFilesCopyAndSymlink(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, ".env", "SECRET=local\n", 0644)
	writeProvisionFixture(t, root, "settings/a.json", "a", 0644)
	writeProvisionFixture(t, root, "settings/b.json", "b", 0644)
	writeProvisionFixture(t, root, "settings/ignore.txt", "ignored", 0644)
	writeProvisionFixture(t, root, "scripts/run", "#!/bin/sh\n", 0755)
	writeProvisionFixture(t, root, "scripts/nested/data", "nested", 0666)
	if err := os.Mkdir(filepath.Join(root, "scripts", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	writeProvisionFixture(t, root, "shared/cache/value", "shared", 0600)
	writeProvisionFixture(t, root, "credentials", "linked secret", 0600)
	writeProvisionFixture(t, root, ".git/config", "main metadata", 0600)
	writeProvisionFixture(t, destination, ".git", "gitdir: elsewhere\n", 0600)
	writeProvisionFixture(t, destination, "settings/tracked.json", "tracked", 0644)
	sourceBefore := provisionSnapshot(t, root)
	var stderr bytes.Buffer
	err := ApplyFiles(context.Background(), root, destination, FilesConfig{
		Copy:    []string{".env", "settings/[ab].json", "scripts"},
		Symlink: []string{"shared/cache", "credentials"},
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected warnings: %s", &stderr)
	}
	for _, test := range []struct {
		name string
		data string
		mode fs.FileMode
	}{
		{".env", "SECRET=local\n", 0600},
		{"settings/a.json", "a", 0600},
		{"settings/b.json", "b", 0600},
		{"scripts/run", "#!/bin/sh\n", 0700},
		{"scripts/nested/data", "nested", 0600},
		{"settings/tracked.json", "tracked", 0644},
		{".git", "gitdir: elsewhere\n", 0600},
	} {
		path := filepath.Join(destination, test.name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != test.data || info.Mode().Perm() != test.mode {
			t.Errorf("%s = %q mode %o, want %q mode %o", test.name, data, info.Mode().Perm(), test.data, test.mode)
		}
	}
	for _, name := range []string{"scripts", "scripts/empty", "scripts/nested", "shared"} {
		info, err := os.Stat(filepath.Join(destination, name))
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatalf("directory %s: info = %v, error = %v", name, info, err)
		}
	}
	for _, name := range []string{"shared/cache", "credentials"} {
		target, err := os.Readlink(filepath.Join(destination, name))
		if err != nil {
			t.Fatal(err)
		}
		if target != filepath.Join(root, name) {
			t.Fatalf("symlink %q -> %q", name, target)
		}
	}
	if _, err := os.Stat(filepath.Join(destination, "settings", "ignore.txt")); !os.IsNotExist(err) {
		t.Fatalf("nonmatching file was copied: %v", err)
	}
	if after := provisionSnapshot(t, root); !reflect.DeepEqual(sourceBefore, after) {
		t.Fatalf("source tree changed: before %#v, after %#v", sourceBefore, after)
	}
}

func TestApplyFilesGlobs(t *testing.T) {
	for _, test := range []struct {
		name    string
		pattern string
		want    []string
	}{
		{"dotfiles", ".env*", []string{".env", ".env.local"}},
		{"question mark", "config?.ini", []string{"config1.ini", "config2.ini"}},
		{"character class", "config[12].ini", []string{"config1.ini", "config2.ini"}},
		{"negated class", "config[^2].ini", []string{"config1.ini"}},
		{"recursive", "**/*.local", []string{".env.local", "a/dev.local", "a/b/deep.local"}},
		{"recursive zero directories", "**/.env", []string{".env", "a/.env"}},
		{"recursive segments", "**/**/.env", []string{".env", "a/.env"}},
		{"directory star", "a/*", []string{"a/.env", "a/dev.local", "a/b/deep.local"}},
		{"directory recursive", "a/**", []string{"a/.env", "a/dev.local", "a/b/deep.local"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			for _, name := range []string{".env", ".env.local", "config1.ini", "config2.ini", "a/.env", "a/dev.local", "a/b/deep.local", ".git/hidden.local"} {
				writeProvisionFixture(t, root, name, name, 0600)
			}
			if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{test.pattern}}, nil); err != nil {
				t.Fatal(err)
			}
			got := make(map[string]string)
			for name, value := range provisionSnapshot(t, destination) {
				if !strings.HasPrefix(value, "d") {
					got[name] = value
				}
			}
			if len(got) != len(test.want) {
				t.Fatalf("copied files = %#v, want %v", got, test.want)
			}
			for _, name := range test.want {
				data, err := os.ReadFile(filepath.Join(destination, name))
				if err != nil || string(data) != name {
					t.Errorf("%q contents = %q, error = %v", name, data, err)
				}
			}
		})
	}
}

func TestApplyFilesUnmatchedWarnings(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "present", "copied", 0600)
	var stderr bytes.Buffer
	err := ApplyFiles(context.Background(), root, destination, FilesConfig{
		Copy:    []string{"present", "missing*.env"},
		Symlink: []string{"absent/[ab]"},
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"warning", "files.copy", `"missing*.env"`, "files.symlink", `"absent/[ab]"`, "matched no files"} {
		if !strings.Contains(stderr.String(), text) {
			t.Errorf("warning lacks %q: %s", text, &stderr)
		}
	}
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"missing"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestApplyFilesRejectsPatternsBeforeMutation(t *testing.T) {
	for _, pattern := range []string{"", " ", "/absolute", "../outside", "a/../outside", "./file", "a//file", "a/", `.\file`, "file\nname", "file\x00name", ".git", "a/.git/config", "a/.GIT", "[", "missing/[", "<global>", "<agent>"} {
		t.Run(fmt.Sprintf("%q", pattern), func(t *testing.T) {
			for _, symlink := range []bool{false, true} {
				root, destination := provisionTestRoots(t)
				writeProvisionFixture(t, root, "new/valid", "valid", 0600)
				files := FilesConfig{Copy: []string{"new/valid", pattern}}
				if symlink {
					files = FilesConfig{Copy: []string{"new/valid"}, Symlink: []string{pattern}}
				}
				before := provisionSnapshot(t, destination)
				if err := ApplyFiles(context.Background(), root, destination, files, nil); err == nil {
					t.Fatal("invalid pattern accepted")
				}
				assertProvisionUnchanged(t, destination, before)
			}
		})
	}
}

func TestApplyFilesRejectsOverlaps(t *testing.T) {
	for _, test := range []struct {
		name  string
		files FilesConfig
	}{
		{"copy and link", FilesConfig{Copy: []string{"new/valid", "private/*"}, Symlink: []string{"private/value"}}},
		{"same copy", FilesConfig{Copy: []string{"new/valid", "private/value", "private/*"}}},
		{"same symlink", FilesConfig{Copy: []string{"new/valid"}, Symlink: []string{"private/value", "private/*"}}},
		{"copy parent", FilesConfig{Copy: []string{"new/valid", "private"}, Symlink: []string{"private/sub/value"}}},
		{"symlink parent", FilesConfig{Copy: []string{"new/valid", "private/sub/value"}, Symlink: []string{"private"}}},
		{"copy prefix", FilesConfig{Copy: []string{"new/valid", "private", "private/sub/value"}}},
		{"separated sorted prefix", FilesConfig{Copy: []string{"new/valid", "private", "private-other", "private/sub/value"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			for _, name := range []string{"new/valid", "private/value", "private/sub/value", "private-other"} {
				writeProvisionFixture(t, root, name, name, 0600)
			}
			before := provisionSnapshot(t, destination)
			if err := ApplyFiles(context.Background(), root, destination, test.files, nil); err == nil || !strings.Contains(err.Error(), "overlapping") {
				t.Fatalf("overlap error = %v", err)
			}
			assertProvisionUnchanged(t, destination, before)
		})
	}
}

func TestApplyFilesRejectsDestinationCollisions(t *testing.T) {
	for _, kind := range []string{"file", "directory", "dangling symlink", "parent file", "parent external symlink", "parent internal symlink"} {
		t.Run(kind, func(t *testing.T) {
			for _, symlink := range []bool{false, true} {
				root, destination := provisionTestRoots(t)
				outside := t.TempDir()
				writeProvisionFixture(t, outside, "keep", "outside", 0600)
				writeProvisionFixture(t, root, "new/valid", "valid", 0600)
				name := "private"
				if strings.HasPrefix(kind, "parent ") {
					name = "private/value"
				}
				writeProvisionFixture(t, root, name, "source", 0600)
				path := filepath.Join(destination, "private")
				var err error
				switch kind {
				case "file", "parent file":
					writeProvisionFixture(t, destination, "private", "tracked destination", 0644)
				case "directory":
					err = os.Mkdir(path, 0700)
				case "dangling symlink":
					err = os.Symlink(filepath.Join(outside, "missing"), path)
				case "parent external symlink":
					err = os.Symlink(outside, path)
				case "parent internal symlink":
					if err := os.Mkdir(filepath.Join(destination, "real"), 0700); err != nil {
						t.Fatal(err)
					}
					err = os.Symlink("real", path)
				}
				if err != nil {
					t.Fatal(err)
				}
				before := provisionSnapshot(t, destination)
				outsideBefore := provisionSnapshot(t, outside)
				files := FilesConfig{Copy: []string{"new/valid", name}}
				if symlink {
					files = FilesConfig{Copy: []string{"new/valid"}, Symlink: []string{name}}
				}
				if err := ApplyFiles(context.Background(), root, destination, files, nil); err == nil {
					t.Fatal("destination collision accepted")
				}
				assertProvisionUnchanged(t, destination, before)
				assertProvisionUnchanged(t, outside, outsideBefore)
			}
		})
	}
}

func TestApplyFilesDoesNotMergeExistingDirectory(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	writeProvisionFixture(t, root, "private/untracked", "local", 0600)
	writeProvisionFixture(t, destination, "private/tracked", "tracked", 0644)
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"new/valid", "private"}}, nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing directory error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesRejectsSourceLinks(t *testing.T) {
	for _, kind := range []string{"internal file", "absolute internal file", "internal directory", "external file", "external directory", "dangling", "cycle", "parent", "recursive internal", "recursive external", "glob recursion"} {
		t.Run(kind, func(t *testing.T) {
			for _, symlink := range []bool{false, true} {
				root, destination := provisionTestRoots(t)
				outside := t.TempDir()
				writeProvisionFixture(t, root, "new/valid", "valid", 0600)
				writeProvisionFixture(t, root, "real/data", "internal", 0600)
				writeProvisionFixture(t, outside, "data", "external", 0600)
				link, target, pattern := "link", "real/data", "link"
				switch kind {
				case "absolute internal file":
					target = filepath.Join(root, "real", "data")
				case "internal directory":
					target = "real"
				case "external file":
					target = filepath.Join(outside, "data")
				case "external directory":
					target = outside
				case "dangling":
					target = "missing"
				case "cycle":
					target = "link"
				case "parent":
					target, pattern = outside, "link/data"
				case "recursive internal":
					link, target, pattern = "real/link", "data", "real"
				case "recursive external":
					link, target, pattern = "real/link", outside, "real"
				case "glob recursion":
					link, target, pattern = "real/link", outside, "real/**/data"
				}
				if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
					t.Fatal(err)
				}
				files := FilesConfig{Copy: []string{"new/valid", pattern}}
				if symlink {
					files = FilesConfig{Copy: []string{"new/valid"}, Symlink: []string{pattern}}
				}
				before := provisionSnapshot(t, destination)
				outsideBefore := provisionSnapshot(t, outside)
				if err := ApplyFiles(context.Background(), root, destination, files, nil); err == nil || !strings.Contains(err.Error(), "symlink") {
					t.Fatalf("source link error = %v", err)
				}
				assertProvisionUnchanged(t, destination, before)
				assertProvisionUnchanged(t, outside, outsideBefore)
			}
		})
	}
}

func TestApplyFilesRejectsGitRecursively(t *testing.T) {
	for _, test := range []struct{ name, pattern string }{
		{"private/child/.git/config", "private"},
		{"private/child/.git", "private"},
		{"private/child/.GIT/config", "private"},
		{".git", ".g?t"},
		{".git/config", "*"},
		{"private/.git/config", "private/**"},
	} {
		t.Run(test.name+"_"+test.pattern, func(t *testing.T) {
			for _, symlink := range []bool{false, true} {
				root, destination := provisionTestRoots(t)
				writeProvisionFixture(t, root, test.name, "metadata", 0600)
				writeProvisionFixture(t, root, "valid", "valid", 0600)
				files := FilesConfig{Copy: []string{test.pattern}}
				if symlink {
					files = FilesConfig{Symlink: []string{test.pattern}}
				}
				before := provisionSnapshot(t, destination)
				if err := ApplyFiles(context.Background(), root, destination, files, nil); err == nil || !strings.Contains(err.Error(), ".git") {
					t.Fatalf("recursive .git error = %v", err)
				}
				assertProvisionUnchanged(t, destination, before)
			}
		})
	}
}

func TestApplyFilesRejectsSpecialFiles(t *testing.T) {
	root, destination := provisionTestRoots(t)
	listener, err := net.Listen("unix", filepath.Join(root, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	for _, files := range []FilesConfig{
		{Copy: []string{"new/valid", "socket"}},
		{Copy: []string{"new/valid"}, Symlink: []string{"socket"}},
	} {
		before := provisionSnapshot(t, destination)
		if err := ApplyFiles(context.Background(), root, destination, files, nil); err == nil || !strings.Contains(err.Error(), "regular file or directory") {
			t.Fatalf("special file error = %v", err)
		}
		assertProvisionUnchanged(t, destination, before)
	}
}

func TestApplyFilesUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 0000 files")
	}
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	path := writeProvisionFixture(t, root, "private/value", "secret", 0600)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0600) })
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"new/valid", "private"}}, nil); err == nil || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable file error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesRootConfinement(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, ".env", "secret", 0600)
	alias := filepath.Join(t.TempDir(), "source alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFiles(context.Background(), alias, destination, FilesConfig{Symlink: []string{".env"}}, nil); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(destination, ".env"))
	if err != nil || link != filepath.Join(root, ".env") {
		t.Fatalf("canonical link = %q, error = %v", link, err)
	}
	destinationAlias := filepath.Join(t.TempDir(), "destination alias")
	if err := os.Symlink(destination, destinationAlias); err != nil {
		t.Fatal(err)
	}
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destinationAlias, FilesConfig{Copy: []string{".env"}}, nil); err == nil || !strings.Contains(err.Error(), "symlinked parents") {
		t.Fatalf("destination alias error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
	for _, symlink := range []bool{false, true} {
		nested := filepath.Join(root, "worktrees", "linked")
		if err := os.MkdirAll(nested, 0700); err != nil {
			t.Fatal(err)
		}
		files := FilesConfig{Copy: []string{"worktrees"}}
		if symlink {
			files = FilesConfig{Symlink: []string{"worktrees"}}
		}
		before := provisionSnapshot(t, nested)
		if err := ApplyFiles(context.Background(), root, nested, files, nil); err == nil || !strings.Contains(err.Error(), "contains the destination") {
			t.Fatalf("recursive destination error = %v", err)
		}
		assertProvisionUnchanged(t, nested, before)
	}
}

func TestApplyFilesInvalidRoots(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "value", "value", 0600)
	before := provisionSnapshot(t, destination)
	for _, test := range []struct{ root, destination string }{
		{"", destination},
		{root, ""},
		{filepath.Join(root, "missing"), destination},
		{root, filepath.Join(destination, "missing")},
		{filepath.Join(root, "value"), destination},
	} {
		if err := ApplyFiles(context.Background(), test.root, test.destination, FilesConfig{Copy: []string{"value"}}, nil); err == nil {
			t.Errorf("accepted invalid roots: %#v", test)
		}
		assertProvisionUnchanged(t, destination, before)
	}
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesCanceled(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(ctx, root, destination, FilesConfig{Copy: []string{"new/valid"}}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled error = %v", err)
	}
	if err := ApplyFiles(ctx, root, destination, FilesConfig{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled empty files error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	writer := provisionTestWriter{write: func(data []byte) (int, error) {
		cancel()
		return len(data), nil
	}}
	if err := ApplyFiles(ctx, root, destination, FilesConfig{Copy: []string{"missing", "new/valid"}}, writer); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled during preflight error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesWarningFailure(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	want := fmt.Errorf("warning writer failed")
	writer := provisionTestWriter{write: func([]byte) (int, error) { return 0, want }}
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"new/valid", "missing"}}, writer); !errors.Is(err, want) {
		t.Fatalf("warning failure = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestProvisionReaderCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := provisionReader{ctx: ctx, reader: strings.NewReader("data")}
	buffer := make([]byte, 2)
	if n, err := reader.Read(buffer); n != 2 || err != nil {
		t.Fatalf("first read = %d, %v", n, err)
	}
	cancel()
	if n, err := reader.Read(buffer); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read = %d, %v", n, err)
	}
}

func TestProvisionRefusesChangesAfterPreflight(t *testing.T) {
	for _, change := range []string{"destination file", "destination parent link", "source link", "source replacement"} {
		t.Run(change, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			outside := t.TempDir()
			writeProvisionFixture(t, outside, "value", "outside", 0600)
			path := writeProvisionFixture(t, root, "private/value", "source", 0600)
			source, sourcePath, err := openProvisionRoot(root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			target, _, err := openProvisionRoot(destination, true)
			if err != nil {
				t.Fatal(err)
			}
			defer target.Close()
			plan, err := scanProvisionSource(context.Background(), source, "private/value")
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightProvisionTarget(context.Background(), target, "private/value"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "destination file":
				writeProvisionFixture(t, destination, "private/value", "concurrent", 0600)
			case "destination parent link":
				err = os.Symlink(outside, filepath.Join(destination, "private"))
			case "source link", "source replacement":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if change == "source link" {
					err = os.Symlink(filepath.Join(outside, "value"), path)
				} else {
					writeProvisionFixture(t, root, "private/value", "replacement", 0600)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			before := provisionSnapshot(t, destination)
			outsideBefore := provisionSnapshot(t, outside)
			if err := applyProvisionEntry(context.Background(), source, target, sourcePath, plan[0]); err == nil {
				t.Fatal("accepted a changed source or destination")
			}
			assertProvisionUnchanged(t, destination, before)
			assertProvisionUnchanged(t, outside, outsideBefore)
		})
	}
}

func provisionTestRoots(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root, destination := filepath.Join(base, "main worktree"), filepath.Join(base, "linked worktree")
	for _, path := range []string{root, destination} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root, destination
}

func writeProvisionFixture(t *testing.T, root, name, data string, mode fs.FileMode) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func provisionSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := info.Mode().String()
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += ":" + string(data)
		} else if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += ":" + target
		}
		snapshot[name] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertProvisionUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	if after := provisionSnapshot(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("preflight mutated %s: before %#v, after %#v", root, before, after)
	}
}

type provisionTestWriter struct {
	write func([]byte) (int, error)
}

func (writer provisionTestWriter) Write(data []byte) (int, error) {
	return writer.write(data)
}
