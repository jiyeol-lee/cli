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
	got, err := (Tmux{Runner: runner}).Create(context.Background(), "$2", w, panes, commands)
	if err != nil || got != "@7" {
		t.Fatalf("create = %q, %v", got, err)
	}
	want := [][]string{
		{"list-windows", "-a", "-F", "#{window_id}\t#{@cli_workmux_id}"},
		{"show-options", "-A", "-v", "-t", "$2", "default-shell"},
		{"new-window", "-d", "-P", "-F", "#{window_id}\t#{pane_id}", "-t", "$2:", "-n", "topic", "-c", w.Path, "--", "sleep", "2147483647"},
		{"set-option", "-w", "-t", "@7", "@cli_workmux_id", w.ID},
		{"set-option", "-w", "-t", "@7", "remain-on-exit", "on"},
		{"respawn-pane", "-k", "-t", "%10", "-c", w.Path, "--", "sh", "-lc", commands[0][2]},
		{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%10", "-v", "-c", w.Path, "-l", "35%", "--", ""},
		{"respawn-pane", "-k", "-t", "%11", "-c", w.Path, "--", "/bin/sh", "-l"},
		{"split-window", "-d", "-P", "-F", "#{pane_id}", "-t", "%11", "-h", "-c", w.Path, "-l", "20", "--", ""},
		{"respawn-pane", "-k", "-t", "%12", "-c", w.Path, "--", "env", "editor"},
		{"select-pane", "-t", "%12"},
		{"resize-pane", "-Z", "-t", "%12"},
	}
	var actual [][]string
	for _, call := range runner.calls {
		actual = append(actual, call.Args)
		if call.Name != "tmux" || call.Dir != "/" {
			t.Fatalf("tmux must not depend on caller cwd: %+v", call)
		}
		if slices.Contains(call.Args, "send-keys") {
			t.Fatal("pane command sent as keystrokes")
		}
	}
	if !reflect.DeepEqual(actual, want) {
		t.Fatalf("argv = %q\nwant = %q", actual, want)
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
		{"ambiguous owner", "@7\t" + w.ID + "\n@8\t" + w.ID + "\n", "", true, false},
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

func TestTmuxCapturedOwnershipAndSocket(t *testing.T) {
	w := tmuxTestWorkspace()
	token := strings.Repeat("b", 32)
	socket := filepath.Join(t.TempDir(), "captured.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
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
		{"replacement window", "@8\t" + w.ID + "\t" + token + "\t" + pid + "\n", false, true},
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
		runner.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
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
