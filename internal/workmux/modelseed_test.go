package workmux

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelSeedCopiesExactBytes(t *testing.T) {
	arbitrary := []byte("not JSON: 'quoted' \"double\" \\ $HOME $(exit 99)\n\n")
	for i := 0; i < 256; i++ {
		arbitrary = append(arbitrary, byte(i))
	}
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"arbitrary", arbitrary},
		{"limit", bytes.Repeat([]byte{255}, modelSeedLimit)},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Containers{HomeDir: t.TempDir(), StateDir: t.TempDir()}
			source := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
			writeModelSeedFixture(t, source, test.data, 0600)
			writeModelSeedFixture(t, filepath.Join(c.StateDir, "opencode", "model.json"), []byte("wrong source"), 0600)
			var warnings bytes.Buffer
			if got, want := c.seedModelCommand("true", &warnings), modelSeedCommand("true", test.data, modelSeedDestination); got != want {
				t.Fatal("seed did not use the approved HomeDir source")
			}
			if warnings.Len() != 0 {
				t.Fatalf("warnings = %q", &warnings)
			}
			destination := filepath.Join(t.TempDir(), "guest 'quoted'\n", "opencode", "model.json")
			writeModelSeedFixture(t, destination, []byte("old guest state"), 0666)
			if err := os.Chmod(filepath.Dir(destination), 0755); err != nil {
				t.Fatal(err)
			}
			command := "cat -- " + shellQuote(destination, "bash")
			stdout, stderr, err := runModelSeedWrapper(t, modelSeedCommand(command, test.data, destination), "", nil)
			if err != nil || stdout != string(test.data) || stderr != "" {
				t.Fatalf("run = %q, %q, %v", stdout, stderr, err)
			}
			assertModelSeedBytes(t, destination, test.data)
			for _, test := range []struct {
				path string
				mode os.FileMode
			}{{destination, 0600}, {filepath.Dir(destination), 0700}} {
				info, err := os.Stat(test.path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != test.mode {
					t.Fatalf("%s mode = %o, want %o", test.path, info.Mode().Perm(), test.mode)
				}
			}
			writeModelSeedFixture(t, destination, []byte("guest mutation"), 0600)
			assertModelSeedBytes(t, source, test.data)
		})
	}
}

func TestModelSeedRereadsSource(t *testing.T) {
	c := &Containers{HomeDir: t.TempDir()}
	path := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
	var warnings bytes.Buffer
	for _, data := range []string{"first snapshot", "second snapshot\n'"} {
		writeModelSeedFixture(t, path, []byte(data), 0600)
		if got, want := c.seedModelCommand("exit 7", &warnings), modelSeedCommand("exit 7", []byte(data), modelSeedDestination); got != want {
			t.Fatalf("seed did not read current bytes %q", data)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := c.seedModelCommand("exit 7", &warnings); got != "exit 7" || warnings.Len() == 0 {
		t.Fatalf("removed source result = %q, warnings = %q", got, &warnings)
	}
}

func TestModelSeedHostFailuresContinue(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T, string)
		warning string
	}{
		{"missing", func(t *testing.T, path string) {}, "inspect"},
		{"symlink", func(t *testing.T, path string) {
			writeModelSeedFixture(t, path+".target", []byte("host bytes"), 0600)
			if err := os.Symlink(path+".target", path); err != nil {
				t.Fatal(err)
			}
		}, "symlinks are not allowed"},
		{"directory", func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
		}, "not a regular file"},
		{"oversize", func(t *testing.T, path string) {
			writeModelSeedFixture(t, path, bytes.Repeat([]byte{'x'}, modelSeedLimit+1), 0600)
		}, "exceeds 64 KiB"},
		{"unreadable", func(t *testing.T, path string) {
			if os.Geteuid() == 0 {
				t.Skip("root can read mode 000 files")
			}
			writeModelSeedFixture(t, path, []byte("private"), 0000)
		}, "read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := &Containers{HomeDir: t.TempDir()}
			path := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
			test.prepare(t, path)
			command := "printf 'still running'; exit 17"
			var warnings bytes.Buffer
			if got := c.seedModelCommand(command, &warnings); got != command {
				t.Fatalf("command = %q, want unchanged %q", got, command)
			}
			if !strings.Contains(warnings.String(), "warning: sandbox model seed:") || !strings.Contains(warnings.String(), path) || !strings.Contains(warnings.String(), test.warning) {
				t.Fatalf("warning = %q, want path and %q", &warnings, test.warning)
			}
			if got := c.seedModelCommand(command, nil); got != command {
				t.Fatalf("nil stderr command = %q", got)
			}
		})
	}
}

func TestModelSeedCommandBudget(t *testing.T) {
	c := &Containers{HomeDir: t.TempDir()}
	data := bytes.Repeat([]byte{'x'}, modelSeedLimit)
	writeModelSeedFixture(t, filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json"), data, 0600)
	for _, test := range []struct {
		name    string
		command string
	}{
		{"long command", ": #" + strings.Repeat("x", 50<<10)},
		{"heavy quoting", ": #" + strings.Repeat("'", 10<<10)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var warnings bytes.Buffer
			if got := c.seedModelCommand(test.command, &warnings); got != test.command {
				t.Fatal("oversized startup wrapper did not return the original command")
			}
			if !strings.Contains(warnings.String(), "warning: sandbox model seed: startup command exceeds 120 KiB") {
				t.Fatalf("warning = %q", &warnings)
			}
			if got := c.seedModelCommand(test.command, nil); got != test.command {
				t.Fatal("nil stderr did not retain the original command")
			}
			if stdout, stderr, err := runModelSeedWrapper(t, test.command, "", nil); err != nil || stdout != "" || stderr != "" {
				t.Fatalf("original command = %q, %q, %v", stdout, stderr, err)
			}
		})
	}
	// The budget includes the exec argument's terminating NUL.
	overhead := len(modelSeedCommand("", data, modelSeedDestination))
	command := ": #" + strings.Repeat("x", modelSeedCommandLimit-overhead-1-3)
	var warnings bytes.Buffer
	seeded := c.seedModelCommand(command, &warnings)
	if seeded == command || len(seeded)+1 != modelSeedCommandLimit || warnings.Len() != 0 {
		t.Fatalf("at-limit wrapper length = %d, warning = %q", len(seeded)+1, &warnings)
	}
	command += "x"
	if got := c.seedModelCommand(command, &warnings); got != command || warnings.Len() == 0 {
		t.Fatalf("over-limit fallback length = %d, warning = %q", len(got), &warnings)
	}
}

func TestModelSeedRejectsGuestAncestorSymlinks(t *testing.T) {
	components := []string{"guest", ".local", "state", "opencode"}
	for index, component := range components {
		t.Run(component, func(t *testing.T) {
			root, target := t.TempDir(), t.TempDir()
			destination := filepath.Join(root, filepath.Join(components...), "model.json")
			link := filepath.Join(root, filepath.Join(components[:index+1]...))
			targetFile := filepath.Join(target, filepath.Join(components[index+1:]...), "model.json")
			original := []byte("host-like model state must remain intact")
			writeModelSeedFixture(t, targetFile, original, 0640)
			for directory := filepath.Dir(targetFile); ; directory = filepath.Dir(directory) {
				if err := os.Chmod(directory, 0751); err != nil {
					t.Fatal(err)
				}
				if directory == target {
					break
				}
			}
			if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, err := runModelSeedWrapper(t, modelSeedCommand("printf continued", []byte("seed bytes"), destination), "", nil)
			if err != nil || stdout != "continued" || !strings.Contains(stderr, "warning: sandbox model seed: copy to "+destination+" failed") {
				t.Fatalf("run = %q, %q, %v", stdout, stderr, err)
			}
			assertModelSeedBytes(t, targetFile, original)
			for path := targetFile; ; path = filepath.Dir(path) {
				want := os.FileMode(0751)
				if path == targetFile {
					want = 0640
				}
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != want {
					t.Fatalf("%s mode = %o, want unchanged %o", path, info.Mode().Perm(), want)
				}
				if path == target {
					break
				}
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("ancestor link = %q, %v", got, err)
			}
		})
	}
}

func TestModelSeedPreparationFailurePreservesGuestFile(t *testing.T) {
	for _, executable := range []string{"mkdir", "chmod"} {
		t.Run(executable, func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "opencode", "model.json")
			original := []byte("existing guest state")
			writeModelSeedFixture(t, destination, original, 0640)
			if err := os.Chmod(filepath.Dir(destination), 0751); err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(root, "bin")
			writeModelSeedFixture(t, filepath.Join(bin, executable), []byte("#!/bin/bash\nexit 1\n"), 0700)
			env := append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			stdout, stderr, err := runModelSeedWrapper(t, modelSeedCommand("printf continued", []byte("seed bytes"), destination), "", env)
			if err != nil || stdout != "continued" || !strings.Contains(stderr, "warning: sandbox model seed:") {
				t.Fatalf("run = %q, %q, %v", stdout, stderr, err)
			}
			assertModelSeedBytes(t, destination, original)
			for _, test := range []struct {
				path string
				mode os.FileMode
			}{{destination, 0640}, {filepath.Dir(destination), 0751}} {
				info, err := os.Stat(test.path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != test.mode {
					t.Fatalf("%s mode = %o, want unchanged %o", test.path, info.Mode().Perm(), test.mode)
				}
			}
		})
	}
}

func TestModelSeedReplacesGuestLeafSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "host-like", "model.json")
	original := []byte("host-like state")
	writeModelSeedFixture(t, target, original, 0640)
	destination := filepath.Join(root, "guest", "model.json")
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, destination); err != nil {
		t.Fatal(err)
	}
	data := []byte("seed bytes")
	stdout, stderr, err := runModelSeedWrapper(t, modelSeedCommand("true", data, destination), "", nil)
	if err != nil || stdout != "" || stderr != "" {
		t.Fatalf("run = %q, %q, %v", stdout, stderr, err)
	}
	assertModelSeedBytes(t, destination, data)
	assertModelSeedBytes(t, target, original)
	info, err := os.Lstat(destination)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("destination = %v, %v", info, err)
	}
}

func TestModelSeedGuestFailuresContinue(t *testing.T) {
	for _, name := range []string{"directory creation", "destination directory", "partial decode", "partial write"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			destination := filepath.Join(root, "opencode", "model.json")
			var env []string
			switch name {
			case "directory creation":
				writeModelSeedFixture(t, filepath.Dir(destination), []byte("blocks mkdir"), 0600)
			case "destination directory":
				writeModelSeedFixture(t, filepath.Join(destination, "keep"), []byte("existing directory content"), 0640)
			case "partial decode":
				bin := filepath.Join(root, "bin")
				writeModelSeedFixture(t, filepath.Join(bin, "base64"), []byte("#!/bin/bash\nprintf partial\nexit 1\n"), 0700)
				env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			case "partial write":
				decoder, err := exec.LookPath("base64")
				if err != nil {
					t.Fatal(err)
				}
				bin := filepath.Join(root, "bin")
				writeModelSeedFixture(t, filepath.Join(bin, "base64"), []byte("#!/bin/bash\nulimit -c 0\nulimit -f 1\nexec "+shellQuote(decoder, "bash")+" \"$@\"\n"), 0700)
				env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			}
			data := bytes.Repeat([]byte("host snapshot"), 1024)
			stdout, stderr, err := runModelSeedWrapper(t, modelSeedCommand("IFS= read -r line; printf '%s' \"$line\"; exit 23", data, destination), "original stdin\n", env)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 23 || stdout != "original stdin" {
				t.Fatalf("run = %q, %q, %v", stdout, stderr, err)
			}
			if !strings.Contains(stderr, "warning: sandbox model seed: copy to "+destination+" failed") {
				t.Fatalf("stderr = %q", stderr)
			}
			if name == "partial decode" || name == "partial write" {
				if _, err := os.Lstat(destination); !os.IsNotExist(err) {
					t.Fatalf("partial guest file remains: %v", err)
				}
			}
			if name == "destination directory" {
				assertModelSeedBytes(t, filepath.Join(destination, "keep"), []byte("existing directory content"))
			}
		})
	}
}

func TestModelSeedPreservesBashCommand(t *testing.T) {
	for _, command := range []string{
		"if IFS= read -r line; then printf '%s\\n' \"$line\"; fi; printf '%s\\n' 'quotes: '\"'\"'single'\"'\"' and \"double\"'; exit 37",
		"value=$(printf 'one\\ntwo'); { printf '%s\\n' \"$value\"; }; printf diagnostic >&2; exit 9",
		"umask; set -o | grep pipefail; cat; false",
		"for value in a b; do printf '%s' \"$value\"; done\nprintf '\\n'; exit 0",
	} {
		t.Run(command, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "opencode", "model.json")
			wantOut, wantErr, wantResult := runModelSeedWrapper(t, command, "stdin 'quoted'\nnext line\n", nil)
			gotOut, gotErr, gotResult := runModelSeedWrapper(t, modelSeedCommand(command, []byte("snapshot"), destination), "stdin 'quoted'\nnext line\n", nil)
			if gotOut != wantOut || gotErr != wantErr || modelSeedExitCode(t, gotResult) != modelSeedExitCode(t, wantResult) {
				t.Fatalf("wrapped = %q, %q, %v; original = %q, %q, %v", gotOut, gotErr, gotResult, wantOut, wantErr, wantResult)
			}
		})
	}
}

func writeModelSeedFixture(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func assertModelSeedBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s bytes differ: got %d bytes, want %d", path, len(got), len(want))
	}
}

func runModelSeedWrapper(t *testing.T, command, stdin string, env []string) (string, string, error) {
	t.Helper()
	process := exec.CommandContext(t.Context(), "bash", "-c", command)
	process.Stdin = strings.NewReader(stdin)
	process.Env = env
	var stdout, stderr bytes.Buffer
	process.Stdout, process.Stderr = &stdout, &stderr
	err := process.Run()
	return stdout.String(), stderr.String(), err
}

func modelSeedExitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatal(err)
	}
	return exit.ExitCode()
}
