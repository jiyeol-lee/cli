package workmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type sandboxCommandRunner func(context.Context, Process) ([]byte, error)

func (run sandboxCommandRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	return run(ctx, p)
}

func TestSandboxCommandParser(t *testing.T) {
	for _, test := range []struct {
		args              []string
		valid, exec, help bool
		words             []string
	}{
		{args: []string{"init"}, valid: true},
		{args: []string{"init", "--help"}, valid: true, help: true},
		{args: []string{"init", "repo"}},
		{args: []string{"init", "--force"}},
		{args: []string{"sandbox"}},
		{args: []string{"sandbox", "build"}, valid: true},
		{args: []string{"sandbox", "build", "--no-cache"}},
		{args: []string{"sandbox", "build", "--", "file"}},
		{args: []string{"sandbox", "pull"}},
		{args: []string{"sandbox", "shell"}, valid: true},
		{args: []string{"sandbox", "shell", "-e"}, valid: true, exec: true},
		{args: []string{"sandbox", "shell", "--exec", "--", "printf", "'%s'", "'two words'"}, valid: true, exec: true, words: []string{"printf", "'%s'", "'two words'"}},
		{args: []string{"sandbox", "shell", "--", "--exec", "--help"}, valid: true, words: []string{"--exec", "--help"}},
		{args: []string{"sandbox", "shell", "--exec=true"}},
		{args: []string{"sandbox", "shell", "-e", "--exec"}},
		{args: []string{"sandbox", "shell", "bash"}},
		{args: []string{"sandbox", "shell", "--", "bad\x00command"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			got, err := ParseCommand(test.args)
			if (err == nil) != test.valid {
				t.Fatalf("parse = %+v, %v", got, err)
			}
			if test.valid && (got.Exec != test.exec || got.Help != test.help || !reflect.DeepEqual(got.ShellCommand, test.words)) {
				t.Fatalf("parse = %+v", got)
			}
		})
	}
}

func sandboxCommandApp(c *Containers, w Workspace) App {
	return App{Runner: c.Runner, Sandbox: c, HomeDir: c.HomeDir, StateDir: c.StateDir,
		ConfigDir: filepath.Join(c.HomeDir, ".config", "cli", "workmux"), Getwd: func() (string, error) { return w.Path, nil }}
}

func TestWorkmuxInit(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	var output bytes.Buffer
	app.Stdout = &output
	if err := app.Run(t.Context(), Command{Kind: "init"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(app.ConfigDir, "main.yaml")
	data, err := os.ReadFile(path)
	if err != nil || string(data) != initExample {
		t.Fatalf("init = %s, %v", data, err)
	}
	if output.String() != path+"\n" {
		t.Fatal(output.String())
	}
	for _, path := range []string{app.ConfigDir, filepath.Dir(app.ConfigDir), filepath.Join(app.ConfigDir, "main.yaml")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if !info.IsDir() {
			want = 0600
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	for _, root := range []string{w.Root, w.Path} {
		app.Getwd = func() (string, error) { return root, nil }
		if err := app.Run(t.Context(), Command{Kind: "init"}); err == nil {
			t.Fatal("overwrote existing config")
		}
		if _, err := os.Lstat(filepath.Join(root, ".workmux.yaml")); !os.IsNotExist(err) {
			t.Fatal("created checkout config")
		}
	}
	if len(sandboxEngineCalls(engine)) != 0 {
		t.Fatal("init called Podman")
	}
	if _, err := os.Lstat(app.StateDir); !os.IsNotExist(err) {
		t.Fatal("init created lifecycle state")
	}
	config, err := LoadConfigForRepo(app.ConfigDir, w.Root)
	if err != nil || config.Sandbox.Enabled || !reflect.DeepEqual(config.Panes, defaultPanes()) || config.PostCreate != nil {
		t.Fatalf("init enabled configuration: %+v, %v", config, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(c.HomeDir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if err := app.Run(t.Context(), Command{Kind: "init"}); err == nil {
		t.Fatal("overwrote dangling symlink")
	}
	if _, err := os.Stat(filepath.Join(c.HomeDir, "missing")); !os.IsNotExist(err) {
		t.Fatal("followed config symlink")
	}
}

func TestWorkmuxInitNames(t *testing.T) {
	seen := map[string]bool{"config.yaml": true}
	for _, name := range []string{"config", "repo-config", "repo-repo-config", "normal", "with space"} {
		file, err := repoConfigName(name)
		if err != nil || seen[file] {
			t.Fatalf("name collision %s: %s, %v", name, file, err)
		}
		seen[file] = true
	}
	for _, name := range []string{"", ".", "..", "../config", "a/b", "a\\b", " name", "name\n"} {
		if _, err := repoConfigName(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestWorkmuxInitSafePaths(t *testing.T) {
	for _, kind := range []string{"symlink directory", "public directory", "reserved name"} {
		t.Run(kind, func(t *testing.T) {
			c, w, _ := sandboxTestFixture(t)
			app := sandboxCommandApp(c, w)
			app.ConfigDir = filepath.Join(c.HomeDir, "config-dir")
			switch kind {
			case "reserved name":
				root := filepath.Join(filepath.Dir(w.Root), "config")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				sandboxTestGit(t, root, "init", "-q", "--template=", "--initial-branch=main")
				app.Getwd = func() (string, error) { return root, nil }
				sandboxTestWrite(t, filepath.Join(app.ConfigDir, "config.yaml"), "# keep global\n")
			case "public directory":
				if err := os.Mkdir(app.ConfigDir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(app.ConfigDir, 0755); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Symlink(c.HomeDir, app.ConfigDir); err != nil {
					t.Fatal(err)
				}
			}
			err := app.Run(t.Context(), Command{Kind: "init"})
			if kind != "reserved name" {
				if err == nil {
					t.Fatal("accepted unsafe config directory")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(app.ConfigDir, "repo-config.yaml")); err != nil || string(data) != initExample {
				t.Fatalf("reserved init: %s %v", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(app.ConfigDir, "config.yaml")); err != nil || string(data) != "# keep global\n" {
				t.Fatal("init overwrote global config")
			}
		})
	}
}

func TestSandboxBuildContext(t *testing.T) {
	for _, failure := range []string{"", "error", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stdout, stderr bytes.Buffer
			stdin := strings.NewReader("input")
			var dir string
			expected := fmt.Errorf("build failed")
			t.Setenv("CONTAINER_CONNECTION", "native-test-connection")
			runner := sandboxCommandRunner(func(ctx context.Context, raw Process) ([]byte, error) {
				if !slices.Contains(raw.Args, "CONTAINER_CONNECTION=native-test-connection") {
					t.Fatal("lost native connection")
				}
				p := sandboxTestPodmanProcess(raw)
				dir = p.Args[len(p.Args)-1]
				if !slices.Equal(p.Args, []string{"build", "--tag", "localhost/test:unit", "--file", filepath.Join(dir, "Containerfile"), dir}) || p.Dir != "/" {
					t.Fatalf("build = %+v", p)
				}
				files, err := os.ReadDir(dir)
				if err != nil || len(files) != 1 || files[0].Name() != "Containerfile" {
					t.Fatalf("context contains unexpected files: %v %v", files, err)
				}
				data, err := os.ReadFile(filepath.Join(dir, "Containerfile"))
				if err != nil || !bytes.Equal(data, sandboxContainerfile) {
					t.Fatal("not embedded Containerfile")
				}
				info, err := os.Stat(dir)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("context permissions: %v %v", info, err)
				}
				info, err = os.Stat(filepath.Join(dir, "Containerfile"))
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("file permissions: %v %v", info, err)
				}
				if p.Stdin != stdin || p.Stdout != &stdout || p.Stderr != &stderr {
					t.Fatal("I/O not injected")
				}
				if _, err := io.Copy(p.Stdout, p.Stdin); err != nil {
					return nil, err
				}
				if _, err := io.WriteString(p.Stderr, "diagnostic"); err != nil {
					return nil, err
				}
				if failure == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				if failure == "error" {
					return nil, expected
				}
				return nil, nil
			})
			err := buildSandbox(ctx, runner, "localhost/test:unit", stdin, &stdout, &stderr)
			if failure == "" && err != nil || failure == "error" && !errors.Is(err, expected) || failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("build = %v", err)
			}
			if stdout.String() != "input" || stderr.String() != "diagnostic" {
				t.Fatal("missing stream output")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("build context survived")
			}
		})
	}
}

func TestSandboxBuildConfiguration(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	sandboxTestWrite(t, filepath.Join(app.ConfigDir, "config.yaml"), "sandbox:\n  image: localhost/test:global\n")
	sandboxTestWrite(t, filepath.Join(app.ConfigDir, "main.yaml"), "sandbox:\n  image: localhost/test:repo\n")
	sandboxTestWrite(t, filepath.Join(w.Path, "Containerfile"), "DO NOT READ THIS")
	var tag string
	app.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" {
			if podman.Args[0] != "build" {
				t.Fatalf("unexpected Podman call: %v", podman.Args)
			}
			tag = podman.Args[2]
			return nil, nil
		}
		return engine.Run(ctx, p)
	})
	for _, test := range []struct{ cwd, tag string }{{w.Root, "localhost/test:repo"}, {w.Path, "localhost/test:repo"}, {c.HomeDir, "localhost/test:global"}} {
		app.Getwd = func() (string, error) { return test.cwd, nil }
		if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "build"}); err != nil {
			t.Fatal(err)
		}
		if tag != test.tag {
			t.Fatalf("build tag = %s, want %s", tag, test.tag)
		}
	}
	if _, err := os.Stat(app.StateDir); !os.IsNotExist(err) {
		t.Fatal("build created lifecycle state")
	}
}

func TestSandboxShellFresh(t *testing.T) {
	sandboxTestNonroot(t)
	for _, failure := range []string{"", "exit", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			w.Config.Sandbox.Enabled = false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var stdout, stderr bytes.Buffer
			stdin := strings.NewReader("input")
			expected := fmt.Errorf("exit status 17")
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				podman := sandboxTestPodmanProcess(p)
				if podman.Name == "podman" && podman.Args[0] == "run" {
					if !slices.Equal(podman.Args[:3], []string{"run", "--rm", "-it"}) || !slices.Equal(podman.Args[len(podman.Args)-3:], []string{"bash", "-c", "printf '%s' 'two words'"}) {
						t.Fatalf("shell argv = %q", podman.Args)
					}
					if !p.Foreground || p.Stdin != stdin || p.Stdout != &stdout || p.Stderr != &stderr {
						t.Fatal("shell I/O not injected")
					}
					sandboxTestSession(t, engine, podman.Args)
					if _, err := io.Copy(p.Stdout, p.Stdin); err != nil {
						return nil, err
					}
					sandboxTestWrite(t, filepath.Join(w.Path, "persistent"), "keep")
					if failure == "cancel" {
						cancel()
						return nil, ctx.Err()
					}
					if failure == "exit" {
						return nil, expected
					}
					return nil, nil
				}
				return engine.Run(ctx, p)
			})
			err := c.Shell(ctx, w, false, []string{"printf", "'%s'", "'two words'"}, stdin, &stdout, &stderr)
			if failure == "" && err != nil || failure == "exit" && !errors.Is(err, expected) || failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("shell = %v", err)
			}
			if stdout.String() != "input" || len(engine.sessions) != 0 {
				t.Fatal("shell did not stream or left a live container")
			}
			if data, err := os.ReadFile(filepath.Join(w.Path, "persistent")); err != nil || string(data) != "keep" {
				t.Fatal("cleanup removed bind files")
			}
		})
	}
}

func TestSandboxShellExec(t *testing.T) {
	sandboxTestNonroot(t)
	for _, change := range []string{"", "multiple", "missing", "stale", "wrong repository", "endpoint", "unrecorded endpoint", "legacy", "exit"} {
		t.Run(change, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			argv, err := c.PaneCommand(t.Context(), w, "opencode")
			if err != nil {
				t.Fatal(err)
			}
			first := sandboxTestSession(t, engine, argv)
			var other *sandboxInspection
			switch change {
			case "multiple":
				argv, err := c.PaneCommand(t.Context(), w, "opencode")
				if err != nil {
					t.Fatal(err)
				}
				other = sandboxTestSession(t, engine, argv)
			case "missing":
				engine.sessions = nil
			case "stale":
				first.State.Running = false
			case "wrong repository":
				first.Config.Labels[sandboxLabel+"repository"] = "foreign"
				engine.listing = first.Name
			case "endpoint":
				engine.info = strings.Replace(engine.info, "/store", "/other-store", 1)
			case "unrecorded endpoint":
				delete(first.Config.Labels, sandboxLabel+"endpoint")
			case "legacy":
				w.Container = "cli-workmux-legacy"
			}
			engine.fail["image inspect"] = fmt.Errorf("image removed")
			var output, notices bytes.Buffer
			stdin := strings.NewReader("exec input")
			called := false
			expected := fmt.Errorf("exit status 23")
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				podman := sandboxTestPodmanProcess(p)
				if podman.Name == "podman" && podman.Args[0] == "exec" {
					called = true
					if !slices.Equal(podman.Args, []string{"exec", "-it", first.ID, "bash", "-c", "bash"}) {
						t.Fatalf("exec argv = %q", podman.Args)
					}
					if p.Stdin != stdin || p.Stdout != &output || p.Stderr != &notices {
						t.Fatal("exec I/O not injected")
					}
					if _, err := io.Copy(p.Stdout, p.Stdin); err != nil {
						return nil, err
					}
					if change == "exit" {
						return nil, expected
					}
					return nil, nil
				}
				return engine.Run(ctx, p)
			})
			engine.calls = nil
			err = c.Shell(t.Context(), w, true, nil, stdin, &output, &notices)
			valid := change == "" || change == "multiple" || change == "exit"
			if called != valid || valid && change != "exit" && err != nil || !valid && err == nil || change == "exit" && !errors.Is(err, expected) {
				t.Fatalf("exec = %v, called=%v", err, called)
			}
			if valid && (output.String() != "exec input" || !first.State.Running || len(engine.sessions) == 0) {
				t.Fatal("exec failed to stream or terminated original session")
			}
			if other != nil && (!strings.Contains(notices.String(), first.Name) || !strings.Contains(notices.String(), other.Name)) {
				t.Fatalf("missing selection notice: %s", &notices)
			}
			for _, p := range sandboxEngineCalls(engine) {
				if p.Args[0] == "image" || p.Args[0] == "rm" || p.Args[0] == "stop" || p.Args[0] == "run" || slices.Contains(p.Args, "--security-opt") {
					t.Fatalf("exec affected original session: %+v", p)
				}
			}
		})
	}
}

func TestSandboxShellLinkedRootOnly(t *testing.T) {
	c, w, _ := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	subdir := filepath.Join(w.Path, "subdir")
	if err := os.Mkdir(subdir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{w.Root, subdir} {
		app.Getwd = func() (string, error) { return dir, nil }
		if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "shell"}); err == nil || !strings.Contains(err.Error(), "root of a linked") {
			t.Fatalf("shell at %s = %v", dir, err)
		}
	}
	if _, err := os.Stat(app.StateDir); !os.IsNotExist(err) {
		t.Fatal("rejected shell created state")
	}
}

func TestSandboxReadyOpenRecovery(t *testing.T) {
	sandboxTestNonroot(t)
	f := newHostFixture(t, "panes:\n  - command: opencode\nsandbox:\n  enabled: true\npost_create: [setup-once]\nfiles:\n  copy: [.env]\n")
	hostWrite(t, filepath.Join(f.root, ".env"), "initial")
	engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo, home: f.home, fail: map[string]error{"image inspect": fmt.Errorf("missing image")}}
	f.app.Sandbox = &Containers{Runner: engine, HomeDir: f.home, StateDir: f.state, Getenv: func(string) string { return "" }}
	if err := f.run(t, "add", "topic"); err == nil || !strings.Contains(err.Error(), "cli workmux sandbox build") {
		t.Fatalf("missing image error = %v", err)
	}
	state := f.load(t, "topic")
	if state.Stage != "ready" {
		t.Fatalf("setup stage = %s", state.Stage)
	}
	hostWrite(t, filepath.Join(state.Path, ".env"), "keep work")
	f.events = nil
	delete(engine.fail, "image inspect")
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal(err)
	}
	for _, event := range f.events {
		if strings.HasPrefix(event, "hook:") || event == "git:worktree add" {
			t.Fatalf("recovery reran setup: %v", f.events)
		}
	}
	if data, err := os.ReadFile(filepath.Join(state.Path, ".env")); err != nil || string(data) != "keep work" {
		t.Fatal("open overwrote files")
	}
	for _, p := range sandboxEngineCalls(engine) {
		if p.Args[0] == "build" || p.Args[0] == "pull" {
			t.Fatal("automatic image provisioning")
		}
	}
}

func TestSandboxShellPlainWorktree(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	var run bool
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" && podman.Args[0] == "run" {
			run = true
			if podman.Args[len(podman.Args)-1] != "bash" {
				t.Fatal("default shell is not Bash")
			}
		}
		return engine.Run(ctx, p)
	})
	if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "shell"}); err != nil {
		t.Fatal(err)
	}
	if !run {
		t.Fatal("plain worktree did not run")
	}
	if _, err := os.Stat(workspacePath(w.Root, w.Handle)); !os.IsNotExist(err) {
		t.Fatal("shell created a managed worktree directory")
	}
	state, err := readState(filepath.Join(app.StateDir, w.RepoID, w.ID+".json"))
	if err != nil || !state.SandboxUsed {
		t.Fatalf("shell did not retain lifecycle ownership: %+v %v", state, err)
	}
}

func TestSandboxIntegrationRunnerPreservesTTY(t *testing.T) {
	base := t.TempDir()
	bin := filepath.Join(base, "bin")
	sandboxTestPodmanBinary(t, bin, "", "printf '%s\\n' \"$@\"")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("HOME", base)
	runner := sandboxIntegrationRunner{home: base, base: base}
	for _, args := range [][]string{
		{"run", "--rm", "-it", "fixture-image", "bash", "-c", "printf test"},
		{"exec", "-it", "fixture-container", "bash", "-c", "printf test"},
	} {
		p := sandboxCurrentEngine().process(args...)
		p.Foreground = true
		out, err := runner.Run(t.Context(), p)
		if err != nil || string(out) != strings.Join(args, "\n")+"\n" {
			t.Fatalf("integration runner rewrote terminal arguments: %q %v", out, err)
		}
	}
}
