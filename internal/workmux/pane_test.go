package workmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAgentCommandSelection(t *testing.T) {
	for _, test := range []struct {
		command string
		want    bool
	}{
		{"opencode", true}, {"/usr/bin/opencode --model test", true}, {"claude --resume", true},
		{"codex", true}, {"gemini", true}, {"echo opencode", false}, {"nvim", false},
		{"my-opencode", false}, {"", false}, {"env opencode", true}, {"<agent>", false},
		{"env -u FOO claude", true}, {"FOO=bar claude", true}, {"'/path with space/opencode' --model test", true},
		{"env -u FOO echo opencode", false}, {"agy", true}, {"alias-opencode", false},
		{"pi", true}, {"omp", true}, {"kiro-cli", true}, {"vibe", true}, {"grok", true}, {"copilot", false},
		{"/not-installed/opencode.exe --help", true}, {"env -S ignored kiro-cli", true}, {"env -P /bin vibe", true},
		{"env --unset FOO --ignore-environment grok", true}, {"env -S opencode", false},
		{"env -- FOO=bar opencode", true},
		{"opencode && echo done", true}, {"opencode;echo done", false}, {"opencode|cat", false},
	} {
		if got := isAgentCommand(test.command); got != test.want {
			t.Errorf("isAgentCommand(%q) = %v", test.command, got)
		}
	}
}

func TestAgentExecutableAliasesAreResolvedWithoutRunningThem(t *testing.T) {
	directory := t.TempDir()
	program := filepath.Join(directory, "opencode.exe")
	marker := filepath.Join(directory, "must-not-execute")
	if err := os.WriteFile(program, []byte("#!/bin/sh\ntouch "+shellQuote(marker, "sh")+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(directory, "custom-agent")
	if err := os.Symlink(program, alias); err != nil {
		t.Fatal(err)
	}
	if !isAgentCommand(alias+" --model example") || !isAgentCommandOnPath("custom-agent", directory) {
		t.Fatal("symlink executable alias was not recognized")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("classification executed the program")
	}
	if isAgentCommandOnPath("shell-function-alias", directory) {
		t.Fatal("guessed an arbitrary shell alias")
	}
}

func TestCommandWordsMatchesShlexRules(t *testing.T) {
	for _, test := range []struct {
		text  string
		words []string
	}{
		{`opencode "a b" 'c d'`, []string{"opencode", "a b", "c d"}},
		{`opencode "a\zb" "a\$b"`, []string{"opencode", `a\zb`, "a$b"}},
		{"opencode \\\n--help", []string{"opencode", "--help"}},
		{"# comment\nopencode # trailing\n--help", []string{"opencode", "--help"}},
		{`opencode a#b`, []string{"opencode", "a#b"}},
		{`opencode && echo done`, []string{"opencode", "&&", "echo", "done"}},
		{`opencode;echo done`, []string{"opencode;echo", "done"}},
		{`opencode '' ""`, []string{"opencode", "", ""}},
		{`opencode 'unterminated`, nil}, {`opencode \`, nil},
	} {
		if got := commandWords(test.text); !reflect.DeepEqual(got, test.words) {
			t.Errorf("commandWords(%q) = %q, want %q", test.text, got, test.words)
		}
	}
}

func TestPaneCommandsMixedSandbox(t *testing.T) {
	w := tmuxTestWorkspace()
	w.Config.Sandbox.Enabled = true
	var events []string
	app := App{Sandbox: &hostTestSandbox{events: &events}}
	got, err := app.paneCommands(context.Background(), w, []Pane{{Command: "opencode"}, {Command: "nvim"}, {}})
	if err != nil || len(got) != 3 || got[0][0] != "podman" || !reflect.DeepEqual(got[1], []string{"nvim"}) || got[2] != nil {
		t.Fatalf("commands = %q, %v", got, err)
	}
	if !reflect.DeepEqual(events, []string{"sandbox:check", "sandbox:ensure"}) {
		t.Fatalf("events = %q", events)
	}
	app.Sandbox.(*hostTestSandbox).paneErr = fmt.Errorf("wrapper failed")
	if commands, err := app.paneCommands(context.Background(), w, []Pane{{Command: "opencode"}}); err == nil || commands != nil {
		t.Fatalf("wrapper failure fell back to host: %q, %v", commands, err)
	}
}

func TestSandboxTargetSelection(t *testing.T) {
	for _, target := range []string{"", "agent", "all"} {
		config := SandboxConfig{Enabled: true, Target: target}
		if !sandboxPane(config, Pane{Command: "opencode"}) {
			t.Fatal("agent was not selected")
		}
		if sandboxPane(config, Pane{}) {
			t.Fatal("omitted command was sandboxed")
		}
		if got := sandboxPane(config, Pane{Command: "nvim"}); got != (target == "all") {
			t.Fatalf("target=%q sandboxed nvim=%v", target, got)
		}
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
	command := append([]string{"sh", "-c", `printf '%s\0' "$@"`, "engine"}, values...)
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
	out, err := (ExecRunner{}).Run(t.Context(), Process{Name: "sh", Args: []string{launch.path}, Dir: t.TempDir()})
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
