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
		args        []string
		valid, help bool
		noCache     bool
		words       []string
	}{
		{args: []string{"init"}, valid: true},
		{args: []string{"init", "--help"}, valid: true, help: true},
		{args: []string{"init", "-h", "--help"}},
		{args: []string{"init", "--help", "--help"}},
		{args: []string{"init", "repo"}},
		{args: []string{"init", "--unknown"}},
		{args: []string{"init", "--no-cache"}},
		{args: []string{"sandbox"}},
		{args: []string{"sandbox", "build"}, valid: true},
		{args: []string{"sandbox", "build", "--no-cache"}, valid: true, noCache: true},
		{args: []string{"sandbox", "build", "--no-cache", "--help"}, valid: true, noCache: true, help: true},
		{args: []string{"sandbox", "build", "--help", "--help"}},
		{args: []string{"sandbox", "run", "-h", "--help"}},
		{args: []string{"sandbox", "build", "--no-cache", "--no-cache"}},
		{args: []string{"sandbox", "build", "--no-cache=true"}},
		{args: []string{"sandbox", "build", "--unknown"}},
		{args: []string{"sandbox", "build", "--", "file"}},
		{args: []string{"sandbox", "pull"}},
		{args: []string{"sandbox", "run"}, valid: true},
		{args: []string{"sandbox", "run", "--"}, valid: true, words: []string{}},
		{args: []string{"sandbox", "run", "--no-cache"}},
		{args: []string{"sandbox", "run", "--unknown"}},
		{args: []string{"sandbox", "run", "bash"}},
		{args: []string{"sandbox", "run", "--", ""}},
		{args: []string{"sandbox", "run", "--", " "}},
		{args: []string{"sandbox", "run", "--", "bad\x00command"}},
		{args: []string{"sandbox", "run", "--help"}, valid: true, help: true},
		{args: []string{"sandbox", "run", "--", "printf", "'%s'", "'two words'"}, valid: true, words: []string{"printf", "'%s'", "'two words'"}},
		{args: []string{"sandbox", "run", "--", "printf '%s' 'two words'"}, valid: true, words: []string{"printf '%s' 'two words'"}},
		{args: []string{"sandbox", "run", "--", "--no-cache", "--help"}, valid: true, words: []string{"--no-cache", "--help"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			got, err := ParseCommand(test.args)
			if (err == nil) != test.valid {
				t.Fatalf("parse = %+v, %v", got, err)
			}
			if test.valid && (got.Help != test.help || got.NoCache != test.noCache || !reflect.DeepEqual(got.ShellCommand, test.words)) {
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
	for _, test := range []struct {
		failure string
		noCache bool
	}{{"", false}, {"", true}, {"error", false}, {"cancel", false}} {
		t.Run(fmt.Sprintf("%s/no-cache=%t", test.failure, test.noCache), func(t *testing.T) {
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
				want := []string{"build"}
				if test.noCache {
					want = append(want, "--no-cache")
				}
				want = append(want, "--tag", "localhost/test:unit", "--file", filepath.Join(dir, "Containerfile"), dir)
				if !slices.Equal(p.Args, want) || p.Dir != "/" {
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
				if test.failure == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				if test.failure == "error" {
					return nil, expected
				}
				return nil, nil
			})
			err := buildSandbox(ctx, runner, "localhost/test:unit", test.noCache, stdin, &stdout, &stderr)
			if test.failure == "" && err != nil || test.failure == "error" && !errors.Is(err, expected) || test.failure == "cancel" && !errors.Is(err, context.Canceled) {
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

func TestSandboxContainerfileDefaultCommand(t *testing.T) {
	if !strings.HasSuffix(string(sandboxContainerfile), "CMD [\"bash\"]\n") {
		t.Fatal("sandbox image must default to Bash")
	}
}

func TestSandboxBuildConfiguration(t *testing.T) {
	c, w, engine := sandboxTestFixture(t)
	app := sandboxCommandApp(c, w)
	sandboxTestWrite(t, filepath.Join(app.ConfigDir, "config.yaml"), "sandbox:\n  image: localhost/test:global\n")
	sandboxTestWrite(t, filepath.Join(app.ConfigDir, "main.yaml"), "sandbox:\n  image: localhost/test:repo\n")
	sandboxTestWrite(t, filepath.Join(w.Path, "Containerfile"), "DO NOT READ THIS")
	var tag string
	var noCache bool
	app.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" {
			if podman.Args[0] != "build" {
				t.Fatalf("unexpected Podman call: %v", podman.Args)
			}
			tag = podman.Args[slices.Index(podman.Args, "--tag")+1]
			noCache = slices.Contains(podman.Args, "--no-cache")
			return nil, nil
		}
		return engine.Run(ctx, p)
	})
	for _, test := range []struct{ cwd, tag string }{{w.Root, "localhost/test:repo"}, {w.Path, "localhost/test:repo"}, {c.HomeDir, "localhost/test:global"}} {
		app.Getwd = func() (string, error) { return test.cwd, nil }
		for _, uncached := range []bool{false, true} {
			args := []string{"sandbox", "build"}
			if uncached {
				args = append(args, "--no-cache")
			}
			command, err := ParseCommand(args)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if tag != test.tag || noCache != uncached {
				t.Fatalf("build tag = %s, no-cache = %t, want %s, %t", tag, noCache, test.tag, uncached)
			}
		}
	}
	if _, err := os.Stat(app.StateDir); !os.IsNotExist(err) {
		t.Fatal("build created lifecycle state")
	}
}

func TestSandboxRunStandalone(t *testing.T) {
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
						t.Fatalf("run argv = %q", podman.Args)
					}
					if !p.Foreground || p.Stdin != stdin || p.Stdout != &stdout || p.Stderr != &stderr {
						t.Fatal("run I/O not injected")
					}
					sandboxTestSession(t, engine, podman.Args)
					if _, err := io.Copy(p.Stdout, p.Stdin); err != nil {
						return nil, err
					}
					if _, err := io.WriteString(p.Stderr, "diagnostic"); err != nil {
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
			err := c.RunStandalone(ctx, w, []string{"printf", "'%s'", "'two words'"}, stdin, &stdout, &stderr, nil)
			if failure == "" && err != nil || failure == "exit" && !errors.Is(err, expected) || failure == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("run = %v", err)
			}
			if stdout.String() != "input" || len(engine.sessions) != 0 {
				t.Fatal("run did not stream or left a live container")
			}
			if !strings.HasPrefix(stderr.String(), "warning: sandbox model seed:") || !strings.HasSuffix(stderr.String(), "\ndiagnostic") {
				t.Fatalf("stderr = %q, want missing-seed warning followed by child diagnostic", &stderr)
			}
			if err := c.checkStandalone(t.Context(), w); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(filepath.Join(w.Path, "persistent")); err != nil || string(data) != "keep" {
				t.Fatal("cleanup removed bind files")
			}
		})
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

func TestSandboxRunPlainWorktree(t *testing.T) {
	sandboxTestNonroot(t)
	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"sandbox", "run"}, "exec bash -i"},
		{[]string{"sandbox", "run", "--"}, "exec bash -i"},
		{[]string{"sandbox", "run", "--", "bash"}, "bash"},
		{[]string{"sandbox", "run", "--", "printf", "'%s'", "'two words'"}, "printf '%s' 'two words'"},
		{[]string{"sandbox", "run", "--", "printf '%s' 'two words'"}, "printf '%s' 'two words'"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			var run bool
			c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
				podman := sandboxTestPodmanProcess(p)
				if podman.Name == "podman" && podman.Args[0] == "run" {
					run = true
					if !slices.Equal(podman.Args[:3], []string{"run", "--rm", "-it"}) || !slices.Equal(podman.Args[len(podman.Args)-3:], []string{"bash", "-c", test.want}) {
						t.Fatalf("standalone argv = %q", podman.Args)
					}
					if !p.Foreground {
						t.Fatal("standalone run is not interactive")
					}
				}
				return engine.Run(ctx, p)
			})
			app := sandboxCommandApp(c, w)
			command, err := ParseCommand(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if err := app.Run(t.Context(), command); err != nil {
				t.Fatal(err)
			}
			if !run {
				t.Fatal("plain worktree did not run")
			}
			if _, err := os.Stat(workspacePath(w.Root, w.Handle)); !os.IsNotExist(err) {
				t.Fatal("standalone run created a managed worktree directory")
			}
			if _, err := os.Stat(filepath.Join(app.StateDir, w.RepoID, w.ID+".json")); !os.IsNotExist(err) {
				t.Fatalf("standalone run created managed workspace state: %v", err)
			}
		})
	}
}

func TestSandboxRunStandaloneDefaultCommand(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	called := false
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" && podman.Args[0] == "run" {
			called = true
			if !slices.Equal(podman.Args[len(podman.Args)-3:], []string{"bash", "-c", "exec bash -i"}) {
				t.Fatalf("default command = %q", podman.Args)
			}
			sandboxTestSession(t, engine, podman.Args)
		}
		return engine.Run(ctx, p)
	})
	err := c.RunStandalone(t.Context(), w, nil, nil, nil, nil, nil)
	if err != nil || !called || len(engine.sessions) != 0 {
		t.Fatalf("default command: called=%t, live sessions=%d, %v", called, len(engine.sessions), err)
	}
}

func TestSandboxPanePublicRunLifecycle(t *testing.T) {
	sandboxTestNonroot(t)
	const payload = `opencode --model 'two words'; printf '%s' "$HOME"`
	f := newHostFixture(t, "panes:\n  - command: |\n      "+payload+"\nsandbox:\n  enabled: true\n")
	engine := &sandboxTestEngine{image: "sha256:" + strings.Repeat("b", 64), info: sandboxTestInfo, home: f.home, fail: make(map[string]error)}
	c := &Containers{Runner: engine, HomeDir: f.home, StateDir: f.state, Getenv: func(string) string { return "" }}
	f.app.Sandbox = c
	if err := f.run(t, "add", "topic"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic").Workspace
	argv := f.mux.commands[0]
	if len(argv) != 6 || argv[0] != "cli" || argv[1] != "workmux" {
		t.Fatalf("pane executable = %q", argv)
	}
	command, err := ParseCommand(argv[2:])
	if err != nil || !slices.Equal(command.ShellCommand, []string{payload + "\n"}) {
		t.Fatalf("public pane payload = %+v, %v", command, err)
	}
	for _, call := range sandboxEngineCalls(engine) {
		if call.Args[0] == "run" || call.Args[0] == "create" {
			t.Fatalf("pane handoff waited for container launch: %+v", call)
		}
	}
	seed := []byte(`{"model":"pane-model"}`)
	sandboxTestWrite(t, filepath.Join(f.home, ".local", "state", "opencode", "model.json"), string(seed))
	called := false
	var manual *sandboxInspection
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" && podman.Args[0] == "run" {
			called = true
			if !p.Foreground || podman.Args[len(podman.Args)-1] != modelSeedCommand(payload+"\n", seed, modelSeedDestination) {
				t.Fatalf("pane skipped foreground model-seeding path: %+v", podman)
			}
			found := sandboxTestSession(t, engine, podman.Args)
			if found.Config.Labels[sandboxLabel+"standalone"] != "true" {
				t.Fatal("pane did not register as a public sandbox session")
			}
			copy := *found
			copy.ID, copy.Name = "manual-id", "manual-session"
			manual = &copy
			engine.sessions = append(engine.sessions, manual)
			if err := c.checkStandalone(ctx, w); err == nil {
				t.Fatal("active pane did not protect its checkout")
			}
			for _, kind := range []string{"merge", "remove"} {
				if err := f.run(t, kind, "topic"); err == nil || !strings.Contains(err.Error(), "active standalone sandbox") {
					t.Fatalf("%s while pane is active = %v", kind, err)
				}
			}
			if err := f.run(t, "close", "topic"); err != nil {
				t.Fatal(err)
			}
			if f.mux.window != "" || len(engine.sessions) != 2 || !manual.State.Running {
				t.Fatal("close did not close its window or stopped an unrelated manual session")
			}
			return nil, nil
		}
		return engine.Run(ctx, p)
	})
	app := f.app
	app.Getwd = func() (string, error) { return w.Path, nil }
	if err := app.Run(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	if !called || len(engine.sessions) != 1 || engine.sessions[0] != manual || !manual.State.Running {
		t.Fatal("pane exit cleanup affected another session or retained its own container")
	}
	if err := c.checkStandalone(t.Context(), w); err != nil {
		t.Fatalf("pane exit retained its registration: %v", err)
	}
}

func TestSandboxRunValidatesManagedWorkspace(t *testing.T) {
	for _, test := range []struct{ name, want string }{
		{"incomplete", "workspace setup is incomplete"},
		{"branch changed", "restore the recorded branch"},
		{"locked", "managed worktree is main, locked, or has changed branch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostFixture(t, "")
			if err := f.run(t, "add", "topic"); err != nil {
				t.Fatal(err)
			}
			state := f.load(t, "topic")
			switch test.name {
			case "incomplete":
				state.Stage = "files"
				repo, err := (gitHost{runner: f.app.Runner}).discover(t.Context(), f.root)
				if err != nil {
					t.Fatal(err)
				}
				store, err := lockState(f.state, repo)
				if err != nil {
					t.Fatal(err)
				}
				if err := errors.Join(store.save(state), store.unlock()); err != nil {
					t.Fatal(err)
				}
			case "branch changed":
				hostGit(t, state.Path, "switch", "-c", "other")
			case "locked":
				hostGit(t, f.root, "worktree", "lock", state.Path)
			}
			app := f.app
			app.Getwd = func() (string, error) { return state.Path, nil }
			if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"opencode"}}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("sandbox run = %v, want %q", err, test.want)
			}
		})
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
	} {
		p := sandboxCurrentEngine().process(args...)
		p.Foreground = true
		out, err := runner.Run(t.Context(), p)
		if err != nil || string(out) != strings.Join(args, "\n")+"\n" {
			t.Fatalf("integration runner rewrote terminal arguments: %q %v", out, err)
		}
	}
}
