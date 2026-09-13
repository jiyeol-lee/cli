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
	"syscall"
	"testing"
)

func TestApplyFilesCopyAndSymlink(t *testing.T) {
	root, destination := provisionTestRoots(t)
	for _, file := range []struct {
		name, data string
		mode       fs.FileMode
	}{
		{".env", "SECRET=fixture\n", 0644}, {"settings/a.json", "a", 0640},
		{"settings/ignore.txt", "ignored", 0644}, {"scripts/run", "#!/bin/sh\n", 0755},
		{"scripts/nested/data", "nested", 0666}, {"shared/cache/value", "shared", 0600},
		{"credentials", "linked fixture", 0600},
	} {
		writeProvisionFixture(t, root, file.name, file.data, file.mode)
	}
	if err := os.Mkdir(filepath.Join(root, "scripts/empty"), 0755); err != nil {
		t.Fatal(err)
	}
	writeProvisionFixture(t, destination, ".git", "gitdir: elsewhere\n", 0600)
	writeProvisionFixture(t, destination, "settings/tracked.json", "tracked", 0644)
	before := provisionSnapshot(t, root)
	var stderr bytes.Buffer
	err := ApplyFiles(context.Background(), root, destination, FilesConfig{
		Copy: []string{".env", "settings/[ab].json", "scripts"}, Symlink: []string{"shared/cache", "credentials"},
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", &stderr)
	}
	for _, file := range []struct {
		name, data string
		mode       fs.FileMode
	}{
		{".env", "SECRET=fixture\n", 0644}, {"settings/a.json", "a", 0640},
		{"scripts/run", "#!/bin/sh\n", 0755}, {"scripts/nested/data", "nested", 0666},
		{"settings/tracked.json", "tracked", 0644}, {".git", "gitdir: elsewhere\n", 0600},
	} {
		assertProvisionFile(t, destination, file.name, file.data)
		info, err := os.Stat(filepath.Join(destination, file.name))
		if err != nil || info.Mode().Perm() != file.mode {
			t.Fatalf("%s mode = %v, error = %v", file.name, info, err)
		}
	}
	for _, name := range []string{"shared/cache", "credentials"} {
		assertProvisionLink(t, root, destination, name)
	}
	if _, err := os.Stat(filepath.Join(destination, "settings/ignore.txt")); !os.IsNotExist(err) {
		t.Fatalf("nonmatch copied: %v", err)
	}
	if info, err := os.Stat(filepath.Join(destination, "scripts/empty")); err != nil || !info.IsDir() {
		t.Fatalf("empty directory = %v, %v", info, err)
	}
	assertProvisionUnchanged(t, root, before)
}

func TestApplyFilesGlobs(t *testing.T) {
	for _, test := range []struct {
		pattern string
		want    []string
	}{
		{".env*", []string{".env", ".env.local"}},
		{"config?.ini", []string{"config1.ini", "config2.ini"}},
		{"config[12].ini", []string{"config1.ini", "config2.ini"}},
		{"config[!2].ini", []string{"config1.ini"}},
		{"**/*.local", []string{".env.local", "a/dev.local", "a/b/deep.local", ".git/hidden.local"}},
		{"**/.env", []string{".env", "a/.env"}},
		{"**/**/.env", []string{".env", "a/.env"}},
		{"a/*", []string{"a/.env", "a/dev.local", "a/b/deep.local"}},
		{"a/**", []string{"a/.env", "a/dev.local", "a/b/deep.local"}},
		{"./a//*.local", []string{"a/dev.local"}},
		{"a/*/", []string{"a/b/deep.local"}},
		{"config1.ini/", []string{}},
	} {
		t.Run(test.pattern, func(t *testing.T) {
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
				assertProvisionFile(t, destination, name, name)
			}
		})
	}
}

func TestApplyFilesCopyRepositoryRoot(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "private/value", "source", 0644)
	writeProvisionFixture(t, destination, "tracked", "keep", 0644)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{".", ""}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionFile(t, destination, "private/value", "source")
	assertProvisionFile(t, destination, "tracked", "keep")
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Symlink: []string{"."}}, nil); err == nil {
		t.Fatal("accepted replacement of destination root")
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesUnmatchedSkips(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "present", "copied", 0600)
	var stderr bytes.Buffer
	err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"missing", "present", "missing*.env"}, Symlink: []string{"absent/[ab]"}}, &stderr)
	if err != nil || stderr.Len() != 0 {
		t.Fatalf("unmatched = %v, stderr = %s", err, &stderr)
	}
	assertProvisionFile(t, destination, "present", "copied")
}

func TestApplyFilesRejectsPatternsBeforeMutation(t *testing.T) {
	for _, pattern := range []string{"/absolute", "../outside", "a/../outside", "file\x00name", "[", "missing/[", "a**b", "***"} {
		t.Run(fmt.Sprintf("%q", pattern), func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			writeProvisionFixture(t, root, "new/valid", "valid", 0600)
			before := provisionSnapshot(t, destination)
			if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"new/valid", pattern}}, nil); err == nil {
				t.Fatal("invalid pattern accepted")
			}
			assertProvisionUnchanged(t, destination, before)
		})
	}
}

func TestApplyFilesAllowsLiteralPaths(t *testing.T) {
	root, destination := provisionTestRoots(t)
	for _, name := range []string{" ", "file\nname", `back\slash`, "<global>", "<agent>", "a/.git/config"} {
		writeProvisionFixture(t, root, name, name, 0600)
		if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{name}}, nil); err != nil {
			t.Fatal(err)
		}
		assertProvisionFile(t, destination, name, name)
	}
}

func TestApplyFilesAbsolutePatternsStayInsideRoot(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "private/value", "source", 0600)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{filepath.Join(root, "private/*")}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionFile(t, destination, "private/value", "source")
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Symlink: []string{filepath.Join(root, "private/value")}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionLink(t, root, destination, "private/value")
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{filepath.Join(filepath.Dir(root), "*")}}, nil); err == nil {
		t.Fatal("accepted an absolute pattern outside root")
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesOverlappingSelections(t *testing.T) {
	for _, test := range []struct {
		name  string
		files FilesConfig
		link  string
	}{
		{"copy then link", FilesConfig{Copy: []string{"private/*"}, Symlink: []string{"private/value"}}, "private/value"},
		{"repeated copy", FilesConfig{Copy: []string{"private/value", "private/*", "private/value"}}, ""},
		{"repeated link", FilesConfig{Symlink: []string{"private/value", "private/*"}}, "private/value"},
		{"copy parent", FilesConfig{Copy: []string{"private"}, Symlink: []string{"private/sub/value"}}, "private/sub/value"},
		{"link parent", FilesConfig{Copy: []string{"private/sub/value"}, Symlink: []string{"private"}}, "private"},
		{"copy prefix", FilesConfig{Copy: []string{"private", "private/sub/value"}}, ""},
		{"child then parent link", FilesConfig{Symlink: []string{"private/sub/value", "private"}}, "private"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			for _, name := range []string{"private/value", "private/sub/value"} {
				writeProvisionFixture(t, root, name, name, 0600)
			}
			if err := ApplyFiles(context.Background(), root, destination, test.files, nil); err != nil {
				t.Fatal(err)
			}
			if test.link != "" {
				assertProvisionLink(t, root, destination, test.link)
			}
			assertProvisionFile(t, destination, "private/sub/value", "private/sub/value")
		})
	}
}

func TestApplyFilesReplacesDestinations(t *testing.T) {
	for _, kind := range []string{"file", "directory", "dangling symlink", "external symlink", "internal symlink", "hardlink"} {
		for _, symlink := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/link=%v", kind, symlink), func(t *testing.T) {
				root, destination := provisionTestRoots(t)
				outside := t.TempDir()
				writeProvisionFixture(t, outside, "keep", "outside", 0600)
				writeProvisionFixture(t, root, "private", "source", 0751)
				path := filepath.Join(destination, "private")
				var err error
				switch kind {
				case "file":
					writeProvisionFixture(t, destination, "private", "tracked", 0644)
				case "directory":
					writeProvisionFixture(t, destination, "private/tracked", "tracked", 0600)
				case "dangling symlink":
					err = os.Symlink(filepath.Join(outside, "missing"), path)
				case "external symlink":
					err = os.Symlink(outside, path)
				case "internal symlink":
					writeProvisionFixture(t, destination, "real", "keep", 0600)
					err = os.Symlink("real", path)
				case "hardlink":
					err = os.Link(filepath.Join(outside, "keep"), path)
				}
				if err != nil {
					t.Fatal(err)
				}
				before := provisionSnapshot(t, outside)
				files := FilesConfig{Copy: []string{"private"}}
				if symlink {
					files = FilesConfig{Symlink: []string{"private"}}
				}
				if err := ApplyFiles(context.Background(), root, destination, files, nil); err != nil {
					t.Fatal(err)
				}
				assertProvisionFile(t, destination, "private", "source")
				if symlink {
					assertProvisionLink(t, root, destination, "private")
				}
				if kind == "internal symlink" {
					assertProvisionFile(t, destination, "real", "keep")
				}
				assertProvisionUnchanged(t, outside, before)
			})
		}
	}
}

func TestApplyFilesMergesDirectoriesAndReplacesTypeConflicts(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "private/new", "local", 0640)
	writeProvisionFixture(t, root, "private/conflict/child", "child", 0750)
	writeProvisionFixture(t, root, "private/replace", "file", 0644)
	writeProvisionFixture(t, destination, "private/tracked", "tracked", 0644)
	writeProvisionFixture(t, destination, "private/conflict", "old file", 0644)
	writeProvisionFixture(t, destination, "private/replace/old", "old directory", 0644)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"private"}}, nil); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"private/new": "local", "private/conflict/child": "child", "private/replace": "file", "private/tracked": "tracked"} {
		assertProvisionFile(t, destination, name, data)
	}
}

func TestApplyFilesRecursiveSourceSymlinks(t *testing.T) {
	root, destination := provisionTestRoots(t)
	outside := t.TempDir()
	writeProvisionFixture(t, outside, "secret", "outside fixture", 0600)
	writeProvisionFixture(t, root, "private/data", "inside", 0600)
	links := map[string]string{"relative": "data", "absolute": filepath.Join(root, "private/data"), "external": outside, "dangling": "missing", "cycle": "cycle"}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(root, "private", name)); err != nil {
			t.Fatal(err)
		}
	}
	before := provisionSnapshot(t, outside)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"private"}}, nil); err != nil {
		t.Fatal(err)
	}
	for name, want := range links {
		got, err := os.Readlink(filepath.Join(destination, "private", name))
		if err != nil || got != want {
			t.Fatalf("%s target = %q, want %q, error = %v", name, got, want, err)
		}
	}
	assertProvisionUnchanged(t, outside, before)
}

func TestApplyFilesDirectSourceSymlinks(t *testing.T) {
	for _, pattern := range []string{"filelink", "dirlink", "dirlink/data", "dir*/data", "absolute", "absolute-dir/data"} {
		t.Run(pattern, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			writeProvisionFixture(t, root, "real/data", "internal", 0644)
			if err := os.Symlink("real/data", filepath.Join(root, "filelink")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("real", filepath.Join(root, "dirlink")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "real/data"), filepath.Join(root, "absolute")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "absolute-dir")); err != nil {
				t.Fatal(err)
			}
			if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{pattern}}, nil); err != nil {
				t.Fatal(err)
			}
			name := "dirlink/data"
			if pattern == "filelink" {
				name = "filelink"
			}
			if strings.HasPrefix(pattern, "absolute") {
				name = pattern
			}
			assertProvisionFile(t, destination, name, "internal")
			if info, err := os.Lstat(filepath.Join(destination, name)); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("direct copy followed source link incorrectly: %v, %v", info, err)
			}
		})
	}
}

func TestApplyFilesConfinement(t *testing.T) {
	for _, kind := range []string{"source external file", "source external parent", "destination external parent", "destination internal parent", "destination parent file"} {
		t.Run(kind, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			outside := t.TempDir()
			writeProvisionFixture(t, outside, "value", "outside", 0600)
			name := "private/value"
			var err error
			switch kind {
			case "source external file":
				name = "private"
				err = os.Symlink(filepath.Join(outside, "value"), filepath.Join(root, "private"))
			case "source external parent":
				err = os.Symlink(outside, filepath.Join(root, "private"))
			case "destination external parent":
				err = os.Symlink(outside, filepath.Join(destination, "private"))
			case "destination internal parent":
				writeProvisionFixture(t, destination, "real/value", "keep", 0600)
				err = os.Symlink("real", filepath.Join(destination, "private"))
			case "destination parent file":
				writeProvisionFixture(t, destination, "private", "keep", 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(kind, "destination") {
				writeProvisionFixture(t, root, name, "source", 0600)
			}
			before, outsideBefore := provisionSnapshot(t, destination), provisionSnapshot(t, outside)
			err = ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{name}}, nil)
			if strings.HasPrefix(kind, "source") {
				if err != nil {
					t.Fatalf("configured external source rejected: %v", err)
				}
				assertProvisionFile(t, destination, name, "outside")
			} else {
				if err == nil {
					t.Fatal("accepted unsafe destination parent")
				}
				assertProvisionUnchanged(t, destination, before)
			}
			assertProvisionUnchanged(t, outside, outsideBefore)
		})
	}
}

func TestApplyFilesConfiguredExternalSources(t *testing.T) {
	for _, pattern := range []string{".env", "relative.env", "shared/settings.json", "shared/*.json", "shared"} {
		t.Run(pattern, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			outside := t.TempDir()
			writeProvisionFixture(t, outside, "shared.env", "external environment fixture", 0640)
			writeProvisionFixture(t, outside, "settings.json", "external settings fixture", 0751)
			if err := os.Symlink(".", filepath.Join(outside, "cycle")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("missing", filepath.Join(outside, "dangling")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(outside, "shared.env"), filepath.Join(root, ".env")); err != nil {
				t.Fatal(err)
			}
			relative, err := filepath.Rel(root, filepath.Join(outside, "shared.env"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(relative, filepath.Join(root, "relative.env")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "shared")); err != nil {
				t.Fatal(err)
			}
			before := provisionSnapshot(t, outside)
			if err := ApplyFiles(t.Context(), root, destination, FilesConfig{Copy: []string{pattern}}, nil); err != nil {
				t.Fatal(err)
			}
			name, want, mode := "shared/settings.json", "external settings fixture", fs.FileMode(0751)
			if pattern == ".env" || pattern == "relative.env" {
				name, want, mode = pattern, "external environment fixture", 0640
			}
			assertProvisionFile(t, destination, name, want)
			info, err := os.Lstat(filepath.Join(destination, name))
			if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
				t.Fatalf("copied external file = %v, %v", info, err)
			}
			if pattern == "shared" {
				for name, want := range map[string]string{"cycle": ".", "dangling": "missing"} {
					link, err := os.Readlink(filepath.Join(destination, "shared", name))
					if err != nil || link != want {
						t.Fatalf("recursive symlink %s = %q, %v", name, link, err)
					}
				}
			}
			if err := ApplyFiles(t.Context(), root, destination, FilesConfig{Symlink: []string{name}}, nil); err != nil {
				t.Fatal(err)
			}
			assertProvisionLink(t, root, destination, name)
			assertProvisionFile(t, destination, name, want)
			assertProvisionUnchanged(t, outside, before)
		})
	}
}

func TestProvisionRefusesExternalSourceRetarget(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(fmt.Sprint(parent), func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			first, second := t.TempDir(), t.TempDir()
			writeProvisionFixture(t, first, "value", "first", 0600)
			writeProvisionFixture(t, second, "value", "second", 0600)
			name, firstTarget, secondTarget := "selected", filepath.Join(first, "value"), filepath.Join(second, "value")
			if parent {
				name, firstTarget, secondTarget = "selected/value", first, second
			}
			link := filepath.Join(root, "selected")
			if err := os.Symlink(firstTarget, link); err != nil {
				t.Fatal(err)
			}
			source, sourcePath, err := openProvisionRoot(root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			target, targetPath, err := openProvisionRoot(destination, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = target.Close() }()
			plan, err := scanProvisionSource(t.Context(), source, name)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(secondTarget, link); err != nil {
				t.Fatal(err)
			}
			before := provisionSnapshot(t, destination)
			if err := applyProvisionEntry(t.Context(), source, target, sourcePath, targetPath, plan[0]); err == nil {
				t.Fatal("accepted retargeted external source")
			}
			assertProvisionUnchanged(t, destination, before)
			assertProvisionFile(t, first, "value", "first")
			assertProvisionFile(t, second, "value", "second")
		})
	}
}

func TestApplyFilesGlobExternalDirectoryCycle(t *testing.T) {
	root, destination := provisionTestRoots(t)
	outside := t.TempDir()
	writeProvisionFixture(t, outside, "settings.json", "external fixture", 0600)
	if err := os.Symlink(".", filepath.Join(outside, "cycle")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFiles(t.Context(), root, destination, FilesConfig{Copy: []string{"shared/**/*.json"}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionFile(t, destination, "shared/settings.json", "external fixture")
	if _, err := os.Lstat(filepath.Join(destination, "shared/cycle")); !os.IsNotExist(err) {
		t.Fatalf("glob entered the recursive source symlink: %v", err)
	}
}

func TestApplyFilesSkipsSpecialFiles(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "private/valid", "valid", 0600)
	listener, err := net.Listen("unix", filepath.Join(root, "private/socket"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := syscall.Mkfifo(filepath.Join(root, "private/fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	writeProvisionFixture(t, destination, "private/socket", "keep", 0600)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"private", "private/fifo"}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionFile(t, destination, "private/valid", "valid")
	assertProvisionFile(t, destination, "private/socket", "keep")
	if _, err := os.Lstat(filepath.Join(destination, "private/fifo")); !os.IsNotExist(err) {
		t.Fatalf("copied FIFO: %v", err)
	}
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Symlink: []string{"private/socket"}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionLink(t, root, destination, "private/socket")
}

func TestApplyFilesUnreadableSource(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 0000 files")
	}
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	path := writeProvisionFixture(t, root, "private/value", "fixture", 0600)
	if err := os.Chmod(path, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
		}
	})
	before := provisionSnapshot(t, destination)
	if err := ApplyFiles(context.Background(), root, destination, FilesConfig{Copy: []string{"new/valid", "private"}}, nil); !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("unreadable file error = %v", err)
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestApplyFilesRootConfinement(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, ".env", "fixture", 0600)
	alias := filepath.Join(t.TempDir(), "source alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFiles(context.Background(), alias, destination, FilesConfig{Symlink: []string{".env"}}, nil); err != nil {
		t.Fatal(err)
	}
	assertProvisionLink(t, root, destination, ".env")
	destinationAlias := filepath.Join(t.TempDir(), "destination alias")
	if err := os.Symlink(destination, destinationAlias); err != nil {
		t.Fatal(err)
	}
	if err := ApplyFiles(context.Background(), root, destinationAlias, FilesConfig{Copy: []string{".env"}}, nil); err == nil {
		t.Fatal("accepted destination alias")
	}
	if err := ApplyFiles(context.Background(), root, root, FilesConfig{Copy: []string{".env"}}, nil); err == nil {
		t.Fatal("accepted identical roots")
	}
	nested := filepath.Join(root, "worktrees/linked")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, files := range []FilesConfig{{Copy: []string{"worktrees"}}, {Symlink: []string{"worktrees"}}} {
		if err := ApplyFiles(context.Background(), root, nested, files, nil); err == nil || !strings.Contains(err.Error(), "contains the destination") {
			t.Fatalf("recursive destination error = %v", err)
		}
	}
}

func TestApplyFilesCanceled(t *testing.T) {
	root, destination := provisionTestRoots(t)
	writeProvisionFixture(t, root, "new/valid", "valid", 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := provisionSnapshot(t, destination)
	for _, files := range []FilesConfig{{Copy: []string{"new/valid"}}, {}} {
		if err := ApplyFiles(ctx, root, destination, files, nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled error = %v", err)
		}
	}
	assertProvisionUnchanged(t, destination, before)
}

func TestProvisionRefusesSourceChanges(t *testing.T) {
	for _, change := range []string{"destination parent link", "source link", "source replacement"} {
		t.Run(change, func(t *testing.T) {
			root, destination := provisionTestRoots(t)
			outside := t.TempDir()
			writeProvisionFixture(t, outside, "value", "outside", 0600)
			path := writeProvisionFixture(t, root, "private/value", "source", 0600)
			source, sourcePath, err := openProvisionRoot(root, false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = source.Close() }()
			target, targetPath, err := openProvisionRoot(destination, true)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = target.Close() }()
			plan, err := scanProvisionSource(context.Background(), source, "private/value")
			if err != nil {
				t.Fatal(err)
			}
			if change == "destination parent link" {
				err = os.Symlink(outside, filepath.Join(destination, "private"))
			} else {
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
			before, outsideBefore := provisionSnapshot(t, destination), provisionSnapshot(t, outside)
			if err := applyProvisionEntry(context.Background(), source, target, sourcePath, targetPath, plan[0]); err == nil {
				t.Fatal("accepted changed source or unsafe destination parent")
			}
			assertProvisionUnchanged(t, destination, before)
			assertProvisionUnchanged(t, outside, outsideBefore)
		})
	}
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

func assertProvisionFile(t *testing.T, root, name, want string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil || string(data) != want {
		t.Fatalf("%s = %q, want %q, error = %v", name, data, want, err)
	}
}

func assertProvisionLink(t *testing.T, root, destination, name string) {
	t.Helper()
	want, err := filepath.Rel(filepath.Dir(filepath.Join(destination, name)), filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(destination, name))
	if err != nil || got != want || filepath.IsAbs(got) {
		t.Fatalf("symlink %s -> %q, want %q, error = %v", name, got, want, err)
	}
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
		t.Fatalf("mutated %s: before %#v, after %#v", root, before, after)
	}
}
