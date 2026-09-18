package workmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func isAgentCommand(command string) bool {
	return isAgentCommandOnPath(command, "")
}

func isAgentCommandOnPath(command, path string) bool {
	fields := commandWords(command)
	for len(fields) > 0 {
		key, _, assignment := strings.Cut(fields[0], "=")
		if !assignment || !sandboxEnvKey(key) {
			break
		}
		fields = fields[1:]
	}
	if len(fields) > 0 && filepath.Base(fields[0]) == "env" {
		fields = fields[1:]
		for len(fields) > 0 {
			word := fields[0]
			if word == "-u" || word == "--unset" || word == "-S" || word == "-P" {
				if len(fields) < 2 {
					return false
				}
				fields = fields[2:]
				continue
			}
			key, _, assignment := strings.Cut(word, "=")
			if strings.HasPrefix(word, "-") || assignment && sandboxEnvKey(key) {
				fields = fields[1:]
				continue
			}
			break
		}
	}
	if len(fields) == 0 {
		return false
	}
	program := fields[0]
	resolved := ""
	if path != "" && !strings.ContainsRune(program, '/') {
		for _, directory := range filepath.SplitList(path) {
			candidate := filepath.Join(directory, program)
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
				resolved = candidate
				break
			}
		}
	}
	if resolved == "" {
		resolved, _ = exec.LookPath(program)
	}
	if resolved != "" {
		if canonical, err := filepath.EvalSymlinks(resolved); err == nil {
			program = canonical
		}
	}
	stem := filepath.Base(program)
	if ext := filepath.Ext(stem); ext != stem {
		stem = strings.TrimSuffix(stem, ext)
	}
	switch stem {
	case "claude", "gemini", "agy", "opencode", "codex", "pi", "omp", "kiro-cli", "vibe", "grok":
		return true
	}
	return false
}

func commandWords(command string) []string {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started, comment := false, false, false
	for _, r := range command {
		if comment {
			if r == '\n' {
				comment = false
			}
			continue
		}
		if escaped {
			if r != '\n' {
				if quote == '"' && !strings.ContainsRune("$`\"\\", r) {
					word.WriteRune('\\')
				}
				word.WriteRune(r)
				started = true
			}
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if r == '#' && !started {
			comment = true
			continue
		}
		if strings.ContainsRune(" \t\r\n", r) {
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escaped {
		return nil
	}
	if started {
		words = append(words, word.String())
	}
	return words
}

func sandboxPane(config SandboxConfig, pane Pane) bool {
	return config.Enabled && pane.Command != "" && (config.Target == "all" || isAgentCommand(pane.Command))
}

func (app *App) paneCommands(ctx context.Context, w Workspace, panes []Pane) ([][]string, error) {
	commands := make([][]string, len(panes))
	path := ""
	if w.Config.Sandbox.Enabled && w.Config.Sandbox.Target != "all" {
		if mux, ok := app.Mux.(interface {
			AgentPath(context.Context) (string, error)
		}); ok {
			path, _ = mux.AgentPath(ctx)
		}
	}
	prepared := false
	for i, pane := range panes {
		if w.Config.Sandbox.Enabled && pane.Command != "" && (w.Config.Sandbox.Target == "all" || isAgentCommandOnPath(pane.Command, path)) {
			if app.Sandbox == nil {
				return nil, fmt.Errorf("sandbox implementation is unavailable")
			}
			if !prepared {
				if err := app.Sandbox.Check(ctx, w.Config.Sandbox); err != nil {
					return nil, err
				}
				if err := app.Sandbox.Ensure(ctx, w); err != nil {
					return nil, err
				}
				prepared = true
			}
			var err error
			commands[i], err = app.Sandbox.PaneCommand(ctx, w, pane.Command)
			if err != nil {
				return nil, err
			}
			if len(commands[i]) < 2 {
				return nil, fmt.Errorf("sandbox returned an invalid pane command; refusing host fallback")
			}
		} else if pane.Command != "" {
			commands[i] = []string{pane.Command}
		}
	}
	return commands, nil
}

func shellQuote(value, shell string) string {
	if filepath.Base(shell) == "fish" {
		return "'" + strings.ReplaceAll(strings.ReplaceAll(value, "\\", "\\\\"), "'", "\\'") + "'"
	}
	if filepath.Base(shell) == "nu" {
		if !strings.Contains(value, "'") {
			return "'" + value + "'"
		}
		hashes := "#"
		for strings.Contains(value, "'"+hashes) {
			hashes += "#"
		}
		return "r" + hashes + "'" + value + "'" + hashes
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func nativeShellCommand(command []string, shell string) string {
	if len(command) == 1 {
		return command[0]
	}
	quoted := make([]string, len(command))
	for i, arg := range command {
		quoted[i] = shellQuote(arg, shell)
	}
	prefix := ""
	if len(command) > 0 && filepath.Base(shell) == "nu" {
		prefix = "^"
	}
	return prefix + strings.Join(quoted, " ")
}

type paneLaunch struct{ path string }

func preparePaneLaunch(base string, command []string) (*paneLaunch, error) {
	if len(command) < 2 || command[0] == "" {
		return nil, fmt.Errorf("invalid generated pane command")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return nil, fmt.Errorf("generated pane argument contains NUL")
		}
	}
	if base == "" {
		base = os.TempDir()
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp(base, "cli-workmux-pane-")
	if err != nil {
		return nil, fmt.Errorf("create private pane launcher: %w", err)
	}
	launch := &paneLaunch{path: filepath.Join(directory, "launch")}
	fail := func(err error) (*paneLaunch, error) { return nil, errors.Join(err, launch.remove()) }
	if err := os.Chmod(directory, 0700); err != nil {
		return fail(err)
	}
	// Parse the whole compound command before unlinking the file that contains it.
	script := "#!/bin/bash\n{\n/bin/rm -f -- " + shellQuote(launch.path, "sh") + " || exit\n/bin/rmdir -- " + shellQuote(directory, "sh") + " || exit\nexec " + nativeShellCommand(command, "sh") + "\n}\n"
	file, err := os.OpenFile(launch.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fail(err)
	}
	chmodErr := file.Chmod(0600)
	_, writeErr := file.WriteString(script)
	closeErr := file.Close()
	if err := errors.Join(chmodErr, writeErr, closeErr); err != nil {
		return fail(err)
	}
	return launch, nil
}

func (launch *paneLaunch) remove() error {
	fileErr := os.Remove(launch.path)
	if os.IsNotExist(fileErr) {
		fileErr = nil
	}
	dirErr := os.Remove(filepath.Dir(launch.path))
	if os.IsNotExist(dirErr) {
		dirErr = nil
	}
	return errors.Join(fileErr, dirErr)
}
