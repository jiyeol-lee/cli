package workmux

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type tmuxTestRunner struct {
	calls []Process
	run   func(Process) ([]byte, error)
}

func (runner *tmuxTestRunner) Run(_ context.Context, p Process) ([]byte, error) {
	runner.calls = append(runner.calls, p)
	if runner.run != nil {
		return runner.run(p)
	}
	return nil, nil
}

func tmuxTestGetenv(key string) string {
	switch key {
	case "TMUX":
		return "/isolated/socket,100,0"
	case "TMUX_PANE":
		return "%1"
	}
	return ""
}

func tmuxTestWorkspace() Workspace {
	return Workspace{ID: strings.Repeat("a", 32), Root: "/repo space", Path: "/repo space__worktrees/topic", Handle: "topic"}
}

func TestTmuxSessionRequiresLivePaneAndVersion(t *testing.T) {
	for _, test := range []struct {
		name, version, paneOutput string
		getenv                    func(string) string
		wantErr                   bool
	}{
		{"live", "tmux 3.2a\n", "$2\t%1\n", tmuxTestGetenv, false},
		{"newer", "tmux 4.0\n", "$2\t%1\n", tmuxTestGetenv, false},
		{"old", "tmux 3.1c\n", "$2\t%1\n", tmuxTestGetenv, true},
		{"malformed", "tmux unknown\n", "$2\t%1\n", tmuxTestGetenv, true},
		{"stale", "tmux 3.3\n", "$2\t%2\n", tmuxTestGetenv, true},
		{"outside", "tmux 3.3\n", "$2\t%1\n", func(string) string { return "" }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				if reflect.DeepEqual(p.Args, []string{"-V"}) {
					return []byte(test.version), nil
				}
				if !reflect.DeepEqual(p.Args, []string{"display-message", "-p", "-t", "%1", "#{session_id}\t#{pane_id}"}) {
					t.Fatalf("unexpected session argv %q", p.Args)
				}
				return []byte(test.paneOutput), nil
			}}
			got, err := (Tmux{Runner: runner, Getenv: test.getenv}).Session(context.Background())
			if (err != nil) != test.wantErr || !test.wantErr && got != "$2" {
				t.Fatalf("session = %q, %v", got, err)
			}
		})
	}
}

func TestTmuxCreateDirectArgvAndLayout(t *testing.T) {
	w := tmuxTestWorkspace()
	panes := []Pane{{Command: "first"}, {Split: "horizontal", Percentage: 35}, {Split: "vertical", Size: 20, Focus: true, Zoom: true}}
	commands := [][]string{{"sh", "-lc", "printf '%s' 'a b'; exit 0"}, nil, {"editor"}}
	runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
		switch p.Args[0] {
		case "show-options":
			return []byte("/bin/sh\n"), nil
		case "new-window":
			return []byte("@7\t%10\n"), nil
		case "split-window":
			if slices.Contains(p.Args, "%10") {
				return []byte("%11\n"), nil
			}
			return []byte("%12\n"), nil
		}
		return nil, nil
	}}
	got, err := (Tmux{Runner: runner, TempDir: t.TempDir()}).Create(context.Background(), "$2", w, panes, commands)
	if err != nil || got != "@7" {
		t.Fatalf("create = %q, %v", got, err)
	}
	splits, commandsSent, shells := 0, 0, 0
	for _, call := range runner.calls {
		if call.Name != "tmux" || call.Dir != "/" {
			t.Fatalf("tmux must not depend on caller cwd: %+v", call)
		}
		if slices.Contains(call.Args, "remain-on-exit") {
			t.Fatal("must inherit remain-on-exit")
		}
		switch call.Args[0] {
		case "new-window":
			if !slices.Contains(call.Args, "-a") || !slices.Contains(call.Args, "wm-topic") {
				t.Fatalf("window placement/name = %q", call.Args)
			}
		case "split-window":
			direction := "-h"
			if splits == 1 {
				direction = "-v"
			}
			if !slices.Contains(call.Args, direction) {
				t.Fatalf("split = %q", call.Args)
			}
			splits++
		case "respawn-pane":
			if splits != 2 {
				t.Fatal("started shell before layout completed")
			}
			startup := commandWords(call.Args[len(call.Args)-1])
			if len(startup) != 3 || !strings.Contains(startup[2], "exec '/bin/sh' -l") {
				t.Fatal("missing login shell")
			}
			shells++
		case "send-keys":
			if slices.Contains(call.Args, "-l") {
				commandsSent++
			}
		}
	}
	if shells != 1 || commandsSent != 2 {
		t.Fatalf("shells=%d commands=%d", shells, commandsSent)
	}
}

func TestTmuxFindAndCloseRevalidateOwnershipNotNames(t *testing.T) {
	w := tmuxTestWorkspace()
	w.Window = "@999"
	for _, test := range []struct {
		name, listed, confirmed string
		wantErr                 bool
		wantKill                bool
	}{
		{"renamed linked window", "@7\t" + w.ID + "\n@7\t" + w.ID + "\n@999\tother\n", "@7\t" + w.ID + "\n", false, true},
		{"legacy stale saved window", "@999\tother\n", "", true, false},
		{"ownership changed", "@7\t" + w.ID + "\n", "@7\tother\n", true, false},
		{"multiple windows primary", "@8\t" + w.ID + "\n@7\t" + w.ID + "\n", "@7\t" + w.ID + "\n", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				switch p.Args[0] {
				case "list-windows":
					return []byte(test.listed), nil
				case "display-message":
					return []byte(test.confirmed), nil
				case "kill-window":
					if !reflect.DeepEqual(p.Args, []string{"kill-window", "-t", "@7"}) {
						t.Fatalf("killed stale or unowned window: %q", p.Args)
					}
				}
				return nil, nil
			}}
			err := (Tmux{Runner: runner}).Close(context.Background(), w)
			if (err != nil) != test.wantErr {
				t.Fatalf("close error = %v", err)
			}
			killed := false
			for _, call := range runner.calls {
				killed = killed || call.Args[0] == "kill-window"
			}
			if killed != test.wantKill {
				t.Fatalf("killed = %v, want %v", killed, test.wantKill)
			}
		})
	}
}

func TestTmuxCreatePaneShellBoundary(t *testing.T) {
	for _, sandbox := range []bool{false, true} {
		t.Run(fmt.Sprintf("sandbox=%t", sandbox), func(t *testing.T) {
			w := tmuxTestWorkspace()
			w.Config.Sandbox.Enabled = sandbox
			commands := [][]string{{"translated", "literal argument"}, {"/usr/bin/false"}}
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				switch p.Args[0] {
				case "show-options":
					return []byte("/custom/login-shell\n"), nil
				case "new-window":
					return []byte("@7\t%10\n"), nil
				case "split-window":
					return []byte("%11\n"), nil
				}
				return nil, nil
			}}
			if _, err := (Tmux{Runner: runner, TempDir: t.TempDir()}).Create(context.Background(), "$2", w, []Pane{{Command: "not the translated argv"}, {Split: "horizontal"}}, commands); err != nil {
				t.Fatal(err)
			}
			queries, launches := 0, 0
			for _, call := range runner.calls {
				switch call.Args[0] {
				case "show-options":
					queries++
				case "respawn-pane":
					startup := commandWords(call.Args[len(call.Args)-1])
					if len(startup) != 3 || !strings.Contains(startup[2], "exec '/custom/login-shell' -l") {
						t.Fatalf("not a host shell: %q", call.Args)
					}
					launches++
				}
			}
			if launches != 1 || queries != 1 {
				t.Fatalf("launches=%d default-shell queries=%d", launches, queries)
			}
		})
	}
}

func TestTmuxCapturedOwnershipAndSocket(t *testing.T) {
	w := tmuxTestWorkspace()
	token := strings.Repeat("b", 32)
	socket := filepath.Join(t.TempDir(), "captured.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Error(err)
		}
	})
	id, err := tmuxSocketIdentity(socket)
	if err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	for _, test := range []struct {
		name, listing string
		present, fail bool
	}{
		{"owned", "@7\t" + w.ID + "\t" + token + "\t" + pid + "\n", true, false},
		{"reused id", "@7\tother\t" + token + "\t" + pid + "\n", false, true},
		{"new token", "@7\t" + w.ID + "\tother\t" + pid + "\n", false, true},
		{"another owned window", "@8\t" + w.ID + "\t" + token + "\t" + pid + "\n", false, false},
		{"new server", "@7\t" + w.ID + "\t" + token + "\t1\n", false, true},
		{"gone", "@8\tother\t\t" + pid + "\n", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				if !reflect.DeepEqual(p.Args[:2], []string{"-S", socket}) {
					t.Fatalf("lost captured socket route: %q", p.Args)
				}
				return []byte(test.listing), nil
			}}
			present, err := (Tmux{Runner: runner}).CapturedExists(context.Background(), w, CleanupWindow{
				ID: "@7", Token: token, Socket: socket, ServerPID: os.Getpid(), SocketDevice: id.Device, SocketInode: id.Inode,
			})
			if present != test.present || (err != nil) != test.fail {
				t.Fatalf("present = %v, err = %v", present, err)
			}
		})
	}
}

func TestTmuxFindDoesNotTreatEngineErrorsAsAbsence(t *testing.T) {
	for _, message := range []string{"permission denied", "failed to connect: Connection refused"} {
		runner := &tmuxTestRunner{run: func(Process) ([]byte, error) { return nil, fmt.Errorf("%s", message) }}
		if _, err := (Tmux{Runner: runner}).Find(context.Background(), tmuxTestWorkspace()); err == nil {
			t.Fatalf("treated error as absence: %s", message)
		}
	}
}

func TestTmuxGuardedCloseRejectsTokenRace(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	if _, err := mux.Create(ctx, session, w, []Pane{{}}, [][]string{{"sleep", "60"}}); err != nil {
		t.Fatal(err)
	}
	captured, err := mux.Capture(ctx, w, strings.Repeat("b", 32))
	if err != nil {
		t.Fatal(err)
	}
	guarded := false
	mux.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
		if slices.Contains(p.Args, "if-shell") {
			guarded = true
			for _, expected := range []string{"#{pid}," + strconv.Itoa(captured.ServerPID), "#{window_id}," + captured.ID, "#{@cli_workmux_id}," + w.ID, "#{@cli_workmux_cleanup}," + captured.Token} {
				if !strings.Contains(strings.Join(p.Args, " "), expected) {
					t.Fatalf("missing atomic guard %q: %q", expected, p.Args)
				}
			}
			if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-w", "-t", captured.ID, "@cli_workmux_cleanup", strings.Repeat("c", 32)}}); err != nil {
				t.Fatal(err)
			}
		}
		return runner.Run(ctx, p)
	})
	if err := mux.CloseCaptured(ctx, w, captured); err == nil || !strings.Contains(err.Error(), "refused") || !guarded {
		t.Fatalf("separate precheck authorized stale kill: %v, guard=%v", err, guarded)
	}
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", captured.ID, "#{window_id}"}})
	if err != nil || strings.TrimSpace(string(out)) != captured.ID {
		t.Fatalf("guard killed a rebound window: %s, %v", out, err)
	}
}

func TestTmuxArgvEscapesCommandSeparators(t *testing.T) {
	runner := &tmuxTestRunner{}
	mux := Tmux{Runner: runner}
	args := []string{"respawn-pane", "-t", "%1", "--", "sh", "-c", "printf ok;"}
	if _, err := mux.run(context.Background(), args...); err != nil {
		t.Fatal(err)
	}
	if got := runner.calls[0].Args; !reflect.DeepEqual(got, []string{"respawn-pane", "-t", "%1", "--", "sh", "-c", "printf ok\\;"}) {
		t.Fatalf("unescaped tmux command separator: %q", got)
	}
	if args[len(args)-1] != "printf ok;" {
		t.Fatal("tmux mutated caller's argv")
	}
	if got := tmuxLiteral("/repo#{window_id}"); got != "/repo##{window_id}" {
		t.Fatalf("unescaped tmux format: %s", got)
	}
}

type isolatedTmuxRunner struct{ socket, home string }

func (runner isolatedTmuxRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	p.Args = append([]string{"-S", runner.socket, "-f", "/dev/null"}, p.Args...)
	p.Env = append(p.Env, "TMUX=", "TMUX_PANE=", "HOME="+runner.home, "SHELL=/bin/sh")
	p.Dir = "/"
	out, err := (ExecRunner{}).Run(ctx, p)
	if err != nil {
		return out, fmt.Errorf("isolated tmux %q: %w", p.Args, err)
	}
	return out, nil
}

func isolatedTmux(t *testing.T) (context.Context, isolatedTmuxRunner, Tmux, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("isolated tmux integration test")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	runner := isolatedTmuxRunner{socket: filepath.Join(t.TempDir(), "tmux.sock"), home: t.TempDir()}
	version, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"-V"}})
	if err != nil || !supportedTmux(strings.TrimSpace(string(version))) {
		t.Skip("tmux 3.2+ is unavailable")
	}
	t.Logf("isolated server version: %s", strings.TrimSpace(string(version)))
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Some tests close the last window or replace the socket before teardown.
		_, _ = runner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
	})
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"new-session", "-d", "-x", "120", "-y", "40", "-s", "workmux-test", "-P", "-F", "#{session_id}\t#{pane_id}", "--", "sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	session, caller, ok := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if !ok {
		t.Fatalf("invalid initial pane output %q", out)
	}
	mux := Tmux{Runner: runner, Getenv: func(key string) string {
		if key == "TMUX_PANE" {
			return caller
		}
		if key == "TMUX" {
			return runner.socket + ",0,0"
		}
		return ""
	}}
	if got, err := mux.Session(ctx); err != nil || got != session {
		t.Fatalf("live session = %s, %v", got, err)
	}
	return ctx, runner, mux, session
}

func TestTmuxIsolatedServerQuickCommandsAndRename(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	caller := mux.Getenv("TMUX_PANE")
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	window, err := mux.Create(ctx, session, w, []Pane{{}, {Split: "horizontal", Percentage: 40}}, [][]string{{"sh", "-c", "exit 0"}, {"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"rename-window", "-t", window, "user-renamed"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := mux.Find(ctx, w); err != nil || got != window {
		t.Fatalf("find renamed quick-command window = %s, %v", got, err)
	}
	captured, err := mux.Capture(ctx, w, strings.Repeat("b", 32))
	if err != nil || captured.Caller || captured.Socket != runner.socket || captured.ID != window {
		t.Fatalf("capture = %+v, %v", captured, err)
	}
	if err := mux.CloseCaptured(ctx, w, captured); err != nil {
		t.Fatal(err)
	}
	if err := mux.Close(ctx, w); err != nil {
		t.Fatal("idempotent tmux close", err)
	}
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", caller, "#{pane_id}"}}); err != nil {
		t.Fatal("closing workmux window removed unrelated caller pane", err)
	}
	w.Handle = "literal;"
	w.Path = filepath.Join(t.TempDir(), "path#{window_id};")
	if err := os.Mkdir(w.Path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := mux.Create(ctx, session, w, []Pane{{}}, [][]string{{"sh", "-c", "printf '%s' 'literal #{window_id}' > result;"}}); err != nil {
		t.Fatal("literal tmux arguments", err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(w.Path, "result"))
		if err == nil && string(data) == "literal #{window_id}" {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("literal command/path was interpreted by tmux: %s, %v", data, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := mux.Close(ctx, w); err != nil {
		t.Fatal(err)
	}
	window, err = mux.Create(ctx, session, w, []Pane{{}}, [][]string{nil})
	if err != nil {
		t.Fatal("default shell creation", err)
	}
	for {
		out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_dead}\t#{pane_current_command}"}})
		if err == nil && strings.TrimSpace(string(out)) == "0\tsh" {
			break
		}
		if err != nil || ctx.Err() != nil || strings.HasPrefix(string(out), "1\t") {
			t.Fatalf("default shell pane is dead or still running the bootstrap command: %s, %v", out, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTmuxConfiguredCommandReturnsToInteractiveShell(t *testing.T) {
	const interrupted = "sh -c 'umask 077; printf \"%s\" \"$$\" > child-pid; exec sleep 60'"
	for _, command := range []string{
		"true", "false", "/missing-workmux-executable",
		"printf '%s' 'quotes \" $dollar $(literal)' > literal\nprintf '%s' ' newline' >> literal;",
		interrupted,
	} {
		t.Run(command, func(t *testing.T) {
			ctx, runner, mux, session := isolatedTmux(t)
			if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-t", session, "default-shell", "/bin/sh"}}); err != nil {
				t.Fatal(err)
			}
			w := tmuxTestWorkspace()
			w.Path = t.TempDir()
			panes := []Pane{{Command: command}}
			commands, err := (&App{}).paneCommands(ctx, w, panes)
			if err != nil {
				t.Fatal(err)
			}
			window, err := mux.Create(ctx, session, w, panes, commands)
			if err != nil {
				t.Fatal(err)
			}
			if command == interrupted {
				childPID := waitTestProcessPID(t, ctx, filepath.Join(w.Path, "child-pid"))
				if err := syscall.Kill(childPID, 0); err != nil {
					t.Fatal("configured child exited before Ctrl-C", err)
				}
				if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "C-c"}}); err != nil {
					t.Fatal(err)
				}
				waitCleanupProcessExit(t, ctx, childPID)
			}
			waitTmuxPaneCommand(t, ctx, runner, window, "sh")
			input := `printf '%s\n' "$-" "$PWD" > interactive`
			if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "-l", input}}); err != nil {
				t.Fatal(err)
			}
			if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "Enter"}}); err != nil {
				t.Fatal(err)
			}
			waitHostFile(t, ctx, filepath.Join(w.Path, "interactive"))
			data, err := os.ReadFile(filepath.Join(w.Path, "interactive"))
			flags, cwd, ok := strings.Cut(strings.TrimSuffix(string(data), "\n"), "\n")
			if err != nil || !ok || !strings.Contains(flags, "i") || cwd != w.Path {
				t.Fatalf("shell did not accept interactive input in workspace: %q, %v", data, err)
			}
			if strings.HasPrefix(command, "printf") {
				data, err := os.ReadFile(filepath.Join(w.Path, "literal"))
				if err != nil || string(data) != "quotes \" $dollar $(literal) newline" {
					t.Fatalf("command was not literal: %q, %v", data, err)
				}
			}
			if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "exit", "Enter"}}); err != nil {
				t.Fatal(err)
			}
			for {
				out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_dead}"}})
				if err != nil && strings.Contains(err.Error(), "can't find window") {
					break
				}
				if err != nil || ctx.Err() != nil {
					t.Fatalf("final shell exit did not close window: %s, %v", out, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func waitTmuxPaneCommand(t *testing.T, ctx context.Context, runner isolatedTmuxRunner, target, command string) {
	t.Helper()
	for {
		out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", target, "-F", "#{pane_dead}\t#{pane_current_command}"}})
		if err == nil && strings.TrimSpace(string(out)) == "0\t"+command {
			return
		}
		if err != nil || ctx.Err() != nil || strings.HasPrefix(string(out), "1\t") {
			t.Fatalf("no live %s in pane: %s, %v", command, out, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitHostFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTmuxConfiguredFirstPaneRespawnsDuringProvisioningOnly(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	shell := filepath.Join(t.TempDir(), "login-shell")
	count := filepath.Join(w.Path, "init-count")
	script := "#!/bin/sh\nif [ \"${1-}\" = -c ]; then exec /bin/sh \"$@\"; fi\nprintf x >> " + shellQuote(count, "sh") + "\nsleep 0.3\nexec /bin/sh -i\n"
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-t", session, "default-shell", shell}}); err != nil {
		t.Fatal(err)
	}
	// Observe the initial shell before permitting its respawn, without relying on scheduling.
	mux.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
		out, err := runner.Run(ctx, p)
		if err == nil && len(p.Args) > 0 && p.Args[0] == "new-window" {
			waitHostContent(t, ctx, count, "x")
		}
		return out, err
	})
	command := "export WORKMUX_TEST_VALUE=retained; cd /; sh -c 'exit 7'; printf '%s:%s' \"$WORKMUX_TEST_VALUE\" \"$PWD\" > " + shellQuote(filepath.Join(w.Path, "native"), "sh")
	window, err := mux.Create(ctx, session, w, []Pane{{Command: command}}, [][]string{{command}})
	if err != nil {
		t.Fatal(err)
	}
	waitHostContent(t, ctx, filepath.Join(w.Path, "native"), "retained:/")
	data, err := os.ReadFile(filepath.Join(w.Path, "native"))
	if err != nil || string(data) != "retained:/" {
		t.Fatalf("native shell state = %q, %v", data, err)
	}
	data, err = os.ReadFile(count)
	if err != nil || string(data) != "xx" {
		t.Fatalf("login initialization count = %q, %v", data, err)
	}
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_pid}"}})
	if err != nil {
		t.Fatal(err)
	}
	pid := strings.TrimSpace(string(out))
	command = "printf '%s:%s' \"$$\" \"$WORKMUX_TEST_VALUE\" > " + shellQuote(filepath.Join(w.Path, "prompt"), "sh")
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "-l", command}}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"send-keys", "-t", window, "Enter"}}); err != nil {
		t.Fatal(err)
	}
	waitHostContent(t, ctx, filepath.Join(w.Path, "prompt"), pid+":retained")
	data, err = os.ReadFile(filepath.Join(w.Path, "prompt"))
	if err != nil || string(data) != pid+":retained" {
		t.Fatalf("shell was supervised or replaced: %q, pane PID %s, %v", data, pid, err)
	}
	data, err = os.ReadFile(count)
	if err != nil || string(data) != "xx" {
		t.Fatalf("application exit restarted the login shell: %q, %v", data, err)
	}
}

func TestTmuxEmptyFirstPaneKeepsInitialGlobalShell(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	shell := filepath.Join(t.TempDir(), "login-shell")
	count := filepath.Join(w.Path, "initial-count")
	script := "#!/bin/sh\nprintf x >> " + shellQuote(count, "sh") + "\nsleep 0.2\nexec /bin/sh -i\n"
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"set-option", "-g", "default-shell", shell}}); err != nil {
		t.Fatal(err)
	}
	window, err := mux.Create(ctx, session, w, []Pane{{}}, [][]string{nil})
	if err != nil {
		t.Fatal(err)
	}
	waitHostContent(t, ctx, count, "x")
	waitTmuxPaneCommand(t, ctx, runner, window, "sh")
	data, err := os.ReadFile(count)
	if err != nil || string(data) != "x" {
		t.Fatalf("empty first pane was respawned: %q, %v", data, err)
	}
}

func TestTmuxEmptyConfiguredLayoutKeepsNativeWindow(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	window, err := mux.Create(ctx, session, w, []Pane{}, [][]string{})
	if err != nil {
		t.Fatal(err)
	}
	waitTmuxPaneCommand(t, ctx, runner, window, "sh")
	if found, err := mux.Find(ctx, w); err != nil || found != window {
		t.Fatalf("empty configured layout lost its native shell: %s, %v", found, err)
	}
}

func TestTmuxFailedInjectionCleansPrivateLaunchers(t *testing.T) {
	for _, stage := range []string{"literal", "enter"} {
		t.Run(stage, func(t *testing.T) {
			base := t.TempDir()
			var launchPath string
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				literal := slices.Contains(p.Args, "-l")
				if literal {
					line := p.Args[len(p.Args)-1]
					words := commandWords(line)
					if len(line) > 1024 || len(words) != 2 || words[0] != "/bin/sh" {
						t.Fatalf("unsafe terminal handoff: %q", line)
					}
					launchPath = words[1]
					if info, err := os.Stat(launchPath); err != nil || info.Mode().Perm() != 0600 {
						t.Fatalf("launcher was missing or public during injection: %v, %v", info, err)
					}
				}
				if literal == (stage == "literal") {
					return nil, fmt.Errorf("injected send failure")
				}
				return nil, nil
			}}
			mux := Tmux{Runner: runner, TempDir: base}
			if err := mux.sendPaneCommand(t.Context(), tmuxTestWorkspace(), "%1", "/bin/sh", []string{"engine", strings.Repeat("secret", 2000)}); err == nil {
				t.Fatal("injection failure was ignored")
			}
			if launchPath == "" {
				t.Fatal("no private launcher was prepared")
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("failed injection leaked a launcher: %v, %v", entries, err)
			}
		})
	}
}

type preparedPaneSandbox struct {
	*hostTestSandbox
	command []string
}

func (sandbox preparedPaneSandbox) PaneCommand(context.Context, Workspace, string) ([]string, error) {
	return slices.Clone(sandbox.command), nil
}

func TestTmuxLongSandboxArgvAfterSlowLoginReturnsToHost(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	f := newHostFixture(t, "sandbox: {enabled: true}\npanes:\n  - command: opencode\n  - command: export HOST_VALUE=retained; printf '%s' \"$HOST_VALUE\" > host-ready\n    split: horizontal\n  - split: vertical\n")
	outdir, bindir := t.TempDir(), t.TempDir()
	shell := filepath.Join(bindir, "login-shell")
	gate := filepath.Join(outdir, "finish-login")
	script := "#!/bin/sh\nif [ \"${1-}\" = -c ]; then exec /bin/sh \"$@\"; fi\nexport LOGIN_VALUE=initialized\nsleep 0.3\nwhile [ ! -f " + shellQuote(gate, "sh") + " ]; do sleep 0.01; done\nexec /bin/sh -i\n"
	if err := os.WriteFile(shell, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	fallback := filepath.Join(outdir, "host-agent-fallback")
	if err := os.WriteFile(filepath.Join(bindir, "opencode"), []byte("#!/bin/sh\nprintf bad > "+shellQuote(fallback, "sh")+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"set-option", "-t", session, "default-shell", shell}, {"set-environment", "-g", "PATH", bindir + ":" + os.Getenv("PATH")}} {
		if _, err := runner.Run(ctx, Process{Name: "tmux", Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	engine := filepath.Join(bindir, "fake engine's executable")
	arguments := filepath.Join(outdir, "argv")
	environment := filepath.Join(outdir, "environment")
	script = "#!/bin/sh\nprintf '%s\\0' \"$@\" > " + shellQuote(arguments, "sh") + "\nprintf '%s' \"$LONG_VALUE:$LOGIN_VALUE\" > " + shellQuote(environment, "sh") + "\nexit 7\n"
	if err := os.WriteFile(engine, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("captured 'quotes' \"double\" $value\n", 400)
	args := []string{"run", "--rm", "--env", "literal=quotes ' and\nnewlines", strings.Repeat("--mount=data with spaces;", 500), "last argument"}
	prepared := append([]string{"env", "LONG_VALUE=" + value, engine}, args...)
	if len(strings.Join(prepared, " ")) <= 8192 {
		t.Fatal("test command does not exceed the terminal buffer")
	}
	mux.TempDir = t.TempDir()
	f.app.Mux = tmuxNoClient{Tmux: mux}
	f.app.Sandbox = preparedPaneSandbox{hostTestSandbox: f.sandbox, command: prepared}
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(mux.TempDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("launcher did not survive CLI return while login was blocked: %v, %v", entries, err)
	}
	hostWrite(t, gate, "ready")
	state := f.load(t, "topic")
	waitHostContent(t, ctx, arguments, strings.Join(args, "\x00")+"\x00")
	waitHostContent(t, ctx, environment, value+":initialized")
	waitHostContent(t, ctx, filepath.Join(state.Path, "host-ready"), "retained")
	panes, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", state.Window, "-F", "#{pane_id}"}})
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(panes))
	if len(ids) != 3 {
		t.Fatalf("mixed layout changed: %q", panes)
	}
	pid, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", ids[0], "#{pane_pid}"}})
	if err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(outdir, "host-prompt")
	line := "printf '%s:%s:%s' \"$$\" \"$LOGIN_VALUE\" \"$?\" > " + shellQuote(prompt, "sh")
	for _, args := range [][]string{{"send-keys", "-t", ids[0], "-l", line}, {"send-keys", "-t", ids[0], "Enter"}} {
		if _, err := runner.Run(ctx, Process{Name: "tmux", Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	waitHostContent(t, ctx, prompt, strings.TrimSpace(string(pid))+":initialized:7")
	entries, err = os.ReadDir(mux.TempDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("executed launcher was not removed: %v, %v", entries, err)
	}
	if _, err := os.Stat(fallback); !os.IsNotExist(err) {
		t.Fatalf("failed engine fell back to a host agent: %v", err)
	}
}

func waitHostContent(t *testing.T, ctx context.Context, path, expected string) {
	t.Helper()
	for {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == expected {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s to contain %q; got %q, %v", path, expected, data, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTmuxNativeExitClosesPanesAndDuplicatesCloseTogether(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	for _, command := range []string{"exit 0", "exec true"} {
		window, err := mux.Create(ctx, session, w, []Pane{{}, {Split: "horizontal"}}, [][]string{{command}, {command}})
		if err != nil {
			t.Fatal(err)
		}
		for {
			out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-windows", "-a", "-F", "#{window_id}"}})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(strings.Fields(string(out)), window) {
				break
			}
			if ctx.Err() != nil {
				t.Fatal("native exit did not close window")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first, err := mux.Create(ctx, session, w, []Pane{{}}, [][]string{nil})
	if err != nil {
		t.Fatal(err)
	}
	w.Window, w.NewWindow = first, true
	second, err := mux.Create(ctx, session, w, []Pane{{}}, [][]string{nil})
	if err != nil || first == second {
		t.Fatalf("duplicate = %s, %v", second, err)
	}
	capture, err := mux.CaptureAll(ctx, w, strings.Repeat("c", 32))
	if err != nil || len(capture.Others) != 1 {
		t.Fatalf("capture = %+v, %v", capture, err)
	}
	if err := mux.CloseCaptured(ctx, w, capture); err != nil {
		t.Fatal(err)
	}
	if present, err := mux.CapturedExists(ctx, w, capture); err != nil || present {
		t.Fatalf("duplicates survive: %v, %v", present, err)
	}
}

func TestTmuxReopenAfterRecordedServerExited(t *testing.T) {
	ctx, runner, mux, _ := isolatedTmux(t)
	f := newHostFixture(t, "panes: [{}]\n")
	f.app.Mux = mux
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	old := f.load(t, "topic")
	if _, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"kill-server"}}); err != nil {
		t.Fatal(err)
	}
	for {
		exited, err := tmuxServerExited(old.ServerPID)
		if err != nil {
			t.Fatal(err)
		}
		if exited {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, _, next, _ := isolatedTmux(t)
	f.app.Mux = tmuxNoClient{Tmux: next}
	if err := f.run(t, "open", "topic"); err != nil {
		t.Fatal(err)
	}
	current := f.load(t, "topic")
	if current.ServerPID == old.ServerPID || current.Socket == old.Socket {
		t.Fatalf("did not adopt new server: %+v", current)
	}
}

type tmuxNoClient struct{ Tmux }

func (tmux tmuxNoClient) Focus(ctx context.Context, w Workspace, window string) error {
	return tmux.owned(ctx, window, w)
}

func TestTmuxSplitTargetGeometryAndInsertion(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"new-window", "-d", "-t", session + ":", "-n", "after", "--", "sleep", "60"}})
	if err != nil {
		t.Fatalf("seed trailing window: %s, %v", out, err)
	}
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	zero := 0
	window, err := mux.Create(ctx, session, w, []Pane{{}, {Split: "horizontal", Percentage: 50}, {Split: "vertical", Target: &zero, Percentage: 50}}, [][]string{nil, nil, nil})
	if err != nil {
		t.Fatal(err)
	}
	out, err = runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_id} #{pane_left} #{pane_top} #{pane_width} #{pane_height}"}})
	if err != nil {
		t.Fatal(err)
	}
	var positions [][4]int
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		var id string
		var position [4]int
		if _, err := fmt.Sscan(line, &id, &position[0], &position[1], &position[2], &position[3]); err != nil {
			t.Fatal(err)
		}
		positions = append(positions, position)
	}
	if len(positions) != 3 || positions[0][0] != positions[1][0] || positions[0][1] >= positions[1][1] || positions[2][0] <= positions[0][0] || positions[2][3] <= positions[0][3] {
		t.Fatalf("wrong target/split geometry: %s", out)
	}
	out, err = runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-windows", "-t", session, "-F", "#{window_name}"}})
	if err != nil {
		t.Fatal(err)
	}
	if names := strings.Fields(string(out)); len(names) != 3 || names[1] != "wm-topic" || names[2] != "after" {
		t.Fatalf("window was not inserted after current: %s", out)
	}
}

func TestTmuxAdoptionRequiresMatchingCwdNotOnlyName(t *testing.T) {
	ctx, runner, mux, session := isolatedTmux(t)
	w := tmuxTestWorkspace()
	w.Path = t.TempDir()
	newWindow := func(cwd string) string {
		t.Helper()
		out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"new-window", "-d", "-P", "-F", "#{window_id}", "-t", session + ":", "-n", "wm-topic", "-c", cwd, "--", "sleep", "60"}})
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	unrelated := newWindow(t.TempDir())
	if id, err := mux.Adopt(ctx, w); err != nil || id != "" {
		t.Fatalf("adopted name alone: %s, %v", id, err)
	}
	owned := newWindow(w.Path)
	if id, err := mux.Adopt(ctx, w); err != nil || id != owned {
		t.Fatalf("did not adopt matching cwd: %s, %v", id, err)
	}
	if err := mux.Close(ctx, w); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", unrelated, "#{window_id}\t#{@cli_workmux_id}"}})
	if err != nil || string(out) != unrelated+"\t\n" {
		t.Fatalf("unowned window was changed: %q, %v", out, err)
	}
}

func TestTmuxExplicitZeroPaneSize(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			config := "panes:\n  - {}\n  - split: horizontal\n"
			if explicit {
				config += "    size: 0\n"
			}
			f := newHostFixture(t, config)
			loaded, err := LoadConfigForRepo(f.config, f.root)
			if err != nil {
				t.Fatal(err)
			}
			panes, err := loaded.SelectPanes("")
			if err != nil {
				t.Fatal(err)
			}
			runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) {
				switch p.Args[0] {
				case "show-options":
					return []byte("/bin/sh\n"), nil
				case "new-window":
					return []byte("@7\t%10\n"), nil
				case "split-window":
					return []byte("%11\n"), nil
				}
				return nil, nil
			}}
			if _, err := (Tmux{Runner: runner}).Create(t.Context(), "$1", tmuxTestWorkspace(), panes, [][]string{nil, nil}); err != nil {
				t.Fatal(err)
			}
			for _, call := range runner.calls {
				if call.Args[0] != "split-window" {
					continue
				}
				index := slices.Index(call.Args, "-l")
				if (index >= 0) != explicit || index >= 0 && call.Args[index+1] != "0" {
					t.Fatalf("lost size presence: %q", call.Args)
				}
			}
		})
	}
}

func TestTmuxMultipleOpenRemoveNamesIncludeDuplicateWindows(t *testing.T) {
	ctx, runner, mux, _ := isolatedTmux(t)
	f := newHostFixture(t, "panes: [{}]\n")
	f.app.Mux = tmuxNoClient{Tmux: mux}
	for _, name := range []string{"one", "two"} {
		if err := f.run(t, "add", name, "-b"); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(t, "open", "one", "two", "--new"); err != nil {
		t.Fatal(err)
	}
	out, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-windows", "-a", "-F", "#{@cli_workmux_id}"}})
	if err != nil || len(strings.Fields(string(out))) != 4 {
		t.Fatalf("multiple opens did not create duplicates: %q, %v", out, err)
	}
	if err := f.run(t, "rm", "one", "two", "-k"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if state := f.load(t, name); state.Stage != "removed" {
			t.Fatalf("%s was not removed", name)
		}
	}
	out, err = runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-windows", "-a", "-F", "#{@cli_workmux_id}"}})
	if err != nil || len(strings.Fields(string(out))) != 0 {
		t.Fatalf("duplicate windows survived removal: %q, %v", out, err)
	}
}
