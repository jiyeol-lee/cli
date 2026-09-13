package workmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestOpenCodeCommandSelection(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{"opencode", true}, {"/usr/bin/opencode --model test", true}, {"claude --resume", false},
		{"codex", false}, {"gemini", false}, {"echo opencode", false}, {"nvim", false},
		{"my-opencode", false}, {"", false}, {"env opencode", false}, {"<agent>", false},
		{"FOO=bar opencode", false}, {"'/path with space/opencode' --model test", true},
		{"agy", false}, {"alias-opencode", false},
		{"pi", false}, {"omp", false}, {"kiro-cli", false}, {"vibe", false}, {"grok", false}, {"copilot", false},
		{"/not-installed/opencode.exe --help", false}, {"opencode.js", false},
		{"env -S opencode", false}, {"env -- FOO=bar opencode", false},
		{"exec opencode --model test", true}, {"exec /usr/bin/opencode", true}, {"exec", false},
		{"exec -a opencode nvim", false}, {"bash -c opencode", false}, {"sh -c 'opencode'", false},
		{"opencode && echo done", true}, {"opencode;echo done", true}, {"opencode|cat", true},
		{"opencode&echo done", true}, {"opencode</tmp/input", true}, {"opencode>/tmp/output", true},
		{"opencode&&echo done", true}, {"opencode||echo done", true}, {"opencode>>/tmp/output", true},
		{"exec opencode>/tmp/output", true}, {"/usr/bin/opencode|cat", true}, {"exec;opencode", false},
		{`'/path;with|operators&<>/opencode' --model test`, true},
		{`'/tmp/opencode;other'`, false}, {`"/tmp/opencode|other"`, false}, {`/tmp/opencode\>other`, false},
		{`opencode 'argument;with|operators&<>'`, true},
		{"FOO=/tmp/opencode nvim", false}, {"exec FOO=/tmp/opencode nvim", false},
		{`FOO="/tmp/opencode" nvim`, false}, {`exec FOO="/tmp/opencode" nvim`, false},
		{"exec 1FOO=/tmp/opencode nvim", true}, {"123=/tmp/opencode --help", true},
		{`'/path=directory/opencode' --model test`, true},
		{`'tools=local/opencode' --help`, true}, {`exec 'tools=local/opencode' --help`, true},
		{`tools\=local/opencode --help`, true}, {`exec tools\=local/opencode --help`, true},
		{`'FOO'=/tmp/opencode --help`, true}, {`exec F"O"O=/tmp/opencode --help`, true},
		{`F\OO=/tmp/opencode --help`, true}, {`F''OO=/tmp/opencode --help`, true},
		{`dir/tools=local/opencode --help`, true}, {`tools=local/opencode --help`, false},
		{"opencode 'unterminated", false}, {`opencode \`, false}, {"$(which opencode)", false},
	} {
		if got := isOpenCodeCommand(test.command); got != test.want {
			t.Errorf("isOpenCodeCommand(%q) = %v", test.command, got)
		}
	}
}

func TestOpenCodeRecognitionDoesNotResolveExecutableAliases(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "opencode")
	marker := filepath.Join(directory, "must-not-execute")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch "+shellQuote(marker, "sh")+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "custom-agent")
	if err := os.Symlink(program, alias); err != nil {
		t.Fatal(err)
	}
	if isOpenCodeCommand(alias+" --model example") || isOpenCodeCommand("custom-agent") {
		t.Fatal("resolved an executable alias")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("classification executed the program")
	}
	if !isOpenCodeCommand(program + " --model example") {
		t.Fatal("explicit opencode executable was not recognized")
	}
}

func TestCommandWordsRespectsShellWordBoundaries(t *testing.T) {
	for _, test := range []struct {
		text  string
		words []string
	}{
		{`opencode "a b" 'c d'`, []string{"opencode", "a b", "c d"}},
		{`opencode "a\zb" "a\$b"`, []string{"opencode", `a\zb`, "a$b"}},
		{"opencode \\\n--help", []string{"opencode", "--help"}},
		{"# comment\nopencode # trailing\n--help", []string{"opencode", "--help"}},
		{`opencode a#b`, []string{"opencode", "a#b"}},
		{`opencode && echo done`, []string{"opencode", "&", "&", "echo", "done"}},
		{`opencode;echo done`, []string{"opencode", ";", "echo", "done"}},
		{`opencode|cat&echo done`, []string{"opencode", "|", "cat", "&", "echo", "done"}},
		{`opencode</tmp/input>/tmp/output`, []string{"opencode", "<", "/tmp/input", ">", "/tmp/output"}},
		{`'/path;with|operators&<>/opencode' "literal;|&<>"`, []string{"/path;with|operators&<>/opencode", "literal;|&<>"}},
		{`opencode\;other argument\|literal`, []string{"opencode;other", "argument|literal"}},
		{`opencode '' ""`, []string{"opencode", "", ""}},
		{`opencode 'unterminated`, nil}, {`opencode \`, nil},
	} {
		if got := commandWords(test.text); !reflect.DeepEqual(got, test.words) {
			t.Errorf("commandWords(%q) = %q, want %q", test.text, got, test.words)
		}
	}
}

func TestCommandTokensPreserveAssignmentSyntax(t *testing.T) {
	for _, test := range []struct {
		text       string
		assignment bool
	}{
		{`FOO=/tmp/opencode`, true}, {`FOO="/tmp/opencode"`, true}, {`FOO='/tmp/opencode'`, true},
		{`FOO=\path/opencode`, true}, {`FOO=a=/tmp/opencode`, true}, {`_FOO2=/tmp/opencode`, true},
		{`'tools=local/opencode'`, false}, {`"tools=local/opencode"`, false}, {`tools\=local/opencode`, false},
		{`'FOO'=/tmp/opencode`, false}, {`F"O"O=/tmp/opencode`, false}, {`F''OO=/tmp/opencode`, false},
		{`F\OO=/tmp/opencode`, false}, {`123=/tmp/opencode`, false}, {`dir/tools=local/opencode`, false},
		{`tools-local=/tmp/opencode`, false}, {`=/tmp/opencode`, false},
	} {
		t.Run(test.text, func(t *testing.T) {
			tokens := commandTokens(test.text + " opencode FOO=/tmp/opencode 'FOO=/tmp/opencode'")
			if len(tokens) != 4 || tokens[0].assignment != test.assignment || tokens[1].assignment || !tokens[2].assignment || tokens[3].assignment {
				t.Fatalf("tokens = %#v, want first assignment=%v", tokens, test.assignment)
			}
		})
	}
}

func TestPaneCommandsMixedSandbox(t *testing.T) {
	w := tmuxTestWorkspace()
	w.Config.Sandbox.Enabled = true
	var events []string
	app := App{Sandbox: &hostTestSandbox{events: &events}}
	first := []string{"cli", "workmux", "sandbox", "run", "--", "opencode"}
	last := []string{"cli", "workmux", "sandbox", "run", "--", "exec /usr/bin/opencode --model test"}
	got, err := app.paneCommands(context.Background(), w, []Pane{{Command: "opencode"}, {Command: "nvim"}, {}, {Command: "claude"}, {Command: "exec /usr/bin/opencode --model test"}})
	if err != nil || len(got) != 5 || !reflect.DeepEqual(got[0], first) || !reflect.DeepEqual(got[1], []string{"nvim"}) || got[2] != nil || !reflect.DeepEqual(got[3], []string{"claude"}) || !reflect.DeepEqual(got[4], last) {
		t.Fatalf("commands = %q, %v", got, err)
	}
	if !reflect.DeepEqual(events, []string{"sandbox:check", "sandbox:ensure"}) {
		t.Fatalf("events = %q", events)
	}
	app.Sandbox.(*hostTestSandbox).ensureErr = fmt.Errorf("preflight failed")
	if commands, err := app.paneCommands(context.Background(), w, []Pane{{Command: "opencode"}}); err == nil || commands != nil {
		t.Fatalf("wrapper failure fell back to host: %q, %v", commands, err)
	}
}

func TestSandboxOnlySelectsOpenCode(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		config := SandboxConfig{Enabled: enabled}
		for _, command := range []string{"opencode", "", "nvim", "claude", "codex", "env opencode"} {
			if got := sandboxPane(config, Pane{Command: command}); got != (enabled && command == "opencode") {
				t.Fatalf("enabled=%v command=%q sandboxed=%v", enabled, command, got)
			}
		}
	}
}

func TestPaneCommandsPreflightFailuresDoNotFallBack(t *testing.T) {
	for _, test := range []struct {
		name    string
		sandbox *hostTestSandbox
		want    []string
	}{
		{"missing sandbox", nil, nil},
		{"check", &hostTestSandbox{checkErr: fmt.Errorf("check failed")}, []string{"sandbox:check"}},
		{"ensure", &hostTestSandbox{ensureErr: fmt.Errorf("ensure failed")}, []string{"sandbox:check", "sandbox:ensure"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := tmuxTestWorkspace()
			w.Config.Sandbox.Enabled = true
			var events []string
			app := App{}
			if test.sandbox != nil {
				test.sandbox.events = &events
				app.Sandbox = test.sandbox
			}
			commands, err := app.paneCommands(t.Context(), w, []Pane{{Command: "opencode"}})
			if err == nil || commands != nil || !slices.Equal(events, test.want) {
				t.Fatalf("failed preflight prepared panes: %q, %v, %q", commands, err, events)
			}
		})
	}
}

func TestPaneCommandsDisabledSandboxKeepsHostCommand(t *testing.T) {
	w := tmuxTestWorkspace()
	w.Config.Sandbox.Enabled = false
	command := `opencode --model 'two words'; printf '%s' "$HOME"`
	app := App{}
	got, err := app.paneCommands(t.Context(), w, []Pane{{Command: command}, {}})
	if err != nil || !reflect.DeepEqual(got, [][]string{{command}, nil}) {
		t.Fatalf("host commands = %q, %v", got, err)
	}
}

func TestPaneCommandsRouteOperatorAndAssignmentBoundaries(t *testing.T) {
	for _, test := range []struct {
		command string
		sandbox bool
	}{
		{"opencode;echo done", true}, {"opencode|cat", true}, {"opencode&echo done", true},
		{"opencode</tmp/input", true}, {"opencode>/tmp/output", true}, {"opencode>>/tmp/output", true},
		{"opencode&&echo done", true}, {"opencode||echo done", true}, {"exec /usr/bin/opencode>/tmp/output", true},
		{`'/path;with|operators&<>/opencode' 'argument;|&<>'`, true},
		{`'/tmp/opencode;other'`, false}, {`"/tmp/opencode|other"`, false}, {`/tmp/opencode\>other`, false},
		{"FOO=/tmp/opencode nvim", false}, {"exec FOO=/tmp/opencode nvim", false},
		{`FOO="/tmp/opencode" nvim`, false}, {`exec FOO="/tmp/opencode" nvim`, false},
		{`'tools=local/opencode' --help`, true}, {`exec 'tools=local/opencode' --help`, true},
		{`tools\=local/opencode --help`, true}, {`exec tools\=local/opencode --help`, true},
		{`'FOO'=/tmp/opencode --help`, true}, {`exec F"O"O=/tmp/opencode --help`, true},
		{`F\OO=/tmp/opencode --help`, true}, {`F''OO=/tmp/opencode --help`, true},
		{"exec 1FOO=/tmp/opencode nvim", true}, {"123=/tmp/opencode --help", true},
		{`dir/tools=local/opencode --help`, true}, {`tools=local/opencode --help`, false},
		{"exec;opencode", false}, {"env opencode", false}, {"claude|cat", false},
	} {
		t.Run(test.command, func(t *testing.T) {
			w := tmuxTestWorkspace()
			w.Config.Sandbox.Enabled = true
			var events []string
			sandbox := &hostTestSandbox{events: &events}
			app := App{Sandbox: sandbox}
			commands, err := app.paneCommands(t.Context(), w, []Pane{{Command: test.command}})
			if err != nil {
				t.Fatal(err)
			}
			if test.sandbox {
				if !reflect.DeepEqual(events, []string{"sandbox:check", "sandbox:ensure"}) {
					t.Fatalf("sandbox events = %q", events)
				}
				if len(commands) != 1 || !slices.Equal(commands[0], []string{"cli", "workmux", "sandbox", "run", "--", test.command}) {
					t.Fatalf("sandbox lost the full command: %q", commands)
				}
			} else if len(events) != 0 || !reflect.DeepEqual(commands, [][]string{{test.command}}) {
				t.Fatalf("host command invoked sandbox: events=%q commands=%q", events, commands)
			}
		})
	}
}

func TestNativeShellCommand(t *testing.T) {
	raw := "cd /tmp; export VALUE='a b'; exec nvim"
	for _, shell := range []string{"/bin/bash", "/bin/zsh", "/bin/fish", "/bin/nu"} {
		if got := nativeShellCommand([]string{raw}, shell); got != raw {
			t.Fatalf("literal changed: %s", got)
		}
		got := nativeShellCommand([]string{"podman", "run", "a b", "$literal"}, shell)
		if !strings.Contains(got, "'a b'") || !strings.Contains(got, "'$literal'") {
			t.Fatalf("unquoted argv: %s", got)
		}
	}
}

func TestNativeShellPublicPaneCommand(t *testing.T) {
	for _, shell := range []string{"/bin/sh", "/bin/bash", "/bin/zsh", "/bin/fish", "/bin/nu"} {
		prefix := "cli workmux sandbox run -- "
		if filepath.Base(shell) == "nu" {
			prefix = "^" + prefix
		}
		for _, payload := range []string{"opencode", `opencode --model 'two words'; printf '%s' "$HOME"`, "opencode\nexit 7"} {
			want := prefix + shellQuote(payload, shell)
			if payload == "opencode" {
				want = prefix + payload
			}
			got := nativeShellCommand([]string{"cli", "workmux", "sandbox", "run", "--", payload}, shell)
			if got != want {
				t.Fatalf("%s public command = %q, want %q", shell, got, want)
			}
		}
	}
}

func TestNativeShellPublicPaneArgvRoundTrip(t *testing.T) {
	for _, shell := range []string{"sh", "bash", "zsh", "fish", "nu"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(shell + " is not installed")
			}
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "cli"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			for _, payload := range []string{"opencode", "opencode 'quotes' \"double\" $dollar `tick` \\backslash\nnewline '# r##'"} {
				args := []string{"workmux", "sandbox", "run", "--", payload}
				command := nativeShellCommand(append([]string{"cli"}, args...), path)
				out, err := (ExecRunner{}).Run(t.Context(), Process{Name: path, Args: []string{"-c", command}, Env: []string{"PATH=" + bin + ":/usr/bin:/bin"}, Dir: t.TempDir()})
				if err != nil || string(out) != strings.Join(args, "\x00")+"\x00" {
					t.Fatalf("public argv round trip = %q, %v; command %q", out, err, command)
				}
			}
		})
	}
}

func TestNativeShellArgvRoundTrip(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish", "nu"} {
		t.Run(shell, func(t *testing.T) {
			path, err := exec.LookPath(shell)
			if err != nil {
				t.Skip(shell + " is not installed")
			}
			value := "space 'quote' \"double\" $dollar `tick` \\backslash\nnewline '# r##'"
			command := nativeShellCommand([]string{"printf", "%s", value}, path)
			out, err := (ExecRunner{}).Run(t.Context(), Process{Name: path, Args: []string{"-c", command}, Dir: t.TempDir()})
			if err != nil || string(out) != value {
				t.Fatalf("argv round trip = %q, %v; command %q", out, err, command)
			}
		})
	}
}

func TestPrivatePaneLaunchPreservesArgumentsAndUnlinksBeforeExec(t *testing.T) {
	base := t.TempDir()
	values := []string{"", "quotes ' \" $literal `ticks` \\ and\nnewlines", strings.Repeat("long value ", 2000)}
	command := append([]string{"/bin/bash", "-c", `values=("$@"); [[ ${#values[@]} == 3 ]] || exit 1; printf '%s\0' "${values[@]}"`, "engine"}, values...)
	launch, err := preparePaneLaunch(base, command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = launch.remove() })
	for _, entry := range []struct {
		path string
		mode os.FileMode
	}{{filepath.Dir(launch.path), 0700}, {launch.path, 0600}} {
		info, err := os.Stat(entry.path)
		if err != nil || info.Mode().Perm() != entry.mode {
			t.Fatalf("unsafe launcher permissions at %s: %v, %v", entry.path, info, err)
		}
	}
	script, err := os.ReadFile(launch.path)
	if err != nil || !strings.HasPrefix(string(script), "#!/bin/bash\n") {
		t.Fatalf("launcher interpreter = %q, %v", script, err)
	}
	out, err := (ExecRunner{}).Run(t.Context(), Process{Name: "/bin/bash", Args: []string{launch.path}, Dir: t.TempDir()})
	if err != nil || string(out) != strings.Join(values, "\x00")+"\x00" {
		t.Fatalf("launcher changed argv: %q, %v", out, err)
	}
	if _, err := os.Stat(filepath.Dir(launch.path)); !os.IsNotExist(err) {
		t.Fatalf("exec left the private launcher behind: %v", err)
	}
	if launch, err := preparePaneLaunch(base, []string{"engine", "bad\x00value"}); err == nil || launch != nil {
		t.Fatal("accepted an invalid generated argument")
	}
}
