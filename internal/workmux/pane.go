package workmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func isOpenCodeCommand(command string) bool {
	fields := commandTokens(command)
	if len(fields) > 0 && fields[0].value == "exec" {
		fields = fields[1:]
	}
	if len(fields) == 0 || fields[0].assignment {
		return false
	}
	return filepath.Base(fields[0].value) == "opencode"
}

func commandWords(command string) []string {
	tokens := commandTokens(command)
	if tokens == nil {
		return nil
	}
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = token.value
	}
	return words
}

type commandWord struct {
	value      string
	assignment bool
}

func commandTokens(command string) []commandWord {
	var words []commandWord
	var word strings.Builder
	var quote rune
	escaped, started, comment := false, false, false
	plain, assignment := true, false
	flushWord := func() {
		if started {
			words = append(words, commandWord{value: word.String(), assignment: assignment})
		}
		word.Reset()
		started, plain, assignment = false, true, false
	}
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
			plain = false
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
			plain = false
			quote = r
			started = true
			continue
		}
		if r == '#' && !started {
			comment = true
			continue
		}
		if strings.ContainsRune(";|&<>", r) {
			flushWord()
			words = append(words, commandWord{value: string(r)})
			continue
		}
		if strings.ContainsRune(" \t\r\n", r) {
			flushWord()
			continue
		}
		if r == '=' && plain && sandboxEnvKey(word.String()) {
			assignment = true
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escaped {
		return nil
	}
	flushWord()
	return words
}

func sandboxPane(config SandboxConfig, pane Pane) bool {
	return config.Enabled && isOpenCodeCommand(pane.Command)
}

func (app *App) paneCommands(ctx context.Context, w Workspace, panes []Pane) ([][]string, error) {
	commands := make([][]string, len(panes))
	prepared := false
	for i, pane := range panes {
		if sandboxPane(w.Config.Sandbox, pane) {
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
			commands[i] = []string{"cli", "workmux", "sandbox", "run", "--", pane.Command}
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
	// Keep the public pane launch readable without interpreting its shell payload.
	if len(command) == 6 && command[0] == "cli" && command[1] == "workmux" && command[2] == "sandbox" && command[3] == "run" && command[4] == "--" {
		copy(quoted[:5], command[:5])
		if command[5] == "opencode" {
			quoted[5] = command[5]
		}
	}
	prefix := ""
	if len(command) > 0 && filepath.Base(shell) == "nu" {
		prefix = "^"
	}
	return prefix + strings.Join(quoted, " ")
}

type paneLaunch struct{ path string }

func validateGeneratedPaneCommand(command []string) error {
	if len(command) < 2 || command[0] == "" {
		return fmt.Errorf("invalid generated pane command")
	}
	for _, arg := range command {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("generated pane argument contains NUL")
		}
	}
	return nil
}

func preparePaneLaunch(base string, command []string) (*paneLaunch, error) {
	if err := validateGeneratedPaneCommand(command); err != nil {
		return nil, err
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
