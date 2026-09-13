package workmux

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

const modelSeedLimit = 64 << 10

// Leave room below Linux's single-argument limit for the complete bash -c argument.
const modelSeedCommandLimit = 120 << 10
const modelSeedDestination = "/tmp/.local/state/opencode/model.json"

func (c *Containers) seedModelCommand(command string, stderr io.Writer) string {
	path := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
	data, err := readModelSeed(path)
	if err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "warning: sandbox model seed: %v\n", err)
		}
		return command
	}
	seeded := modelSeedCommand(command, data, modelSeedDestination)
	if len(seeded)+1 > modelSeedCommandLimit {
		if stderr != nil {
			fmt.Fprintln(stderr, "warning: sandbox model seed: startup command exceeds 120 KiB; skipping seed")
		}
		return command
	}
	return seeded
}

func readModelSeed(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (symlinks are not allowed)", path)
	}
	// Do not follow a replacement symlink or block on a replacement FIFO.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > modelSeedLimit {
		return nil, fmt.Errorf("%s exceeds 64 KiB", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, modelSeedLimit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > modelSeedLimit {
		return nil, fmt.Errorf("%s exceeds 64 KiB", path)
	}
	return data, nil
}

func modelSeedCommand(command string, data []byte, destination string) string {
	// Check ancestors from root to leaf before mkdir or chmod can follow a symlink.
	checks := ""
	for ancestor := filepath.Dir(destination); ; ancestor = filepath.Dir(ancestor) {
		checks = "[ ! -L " + shellQuote(ancestor, "bash") + " ] &&\n" + checks
		if filepath.Dir(ancestor) == ancestor {
			break
		}
	}
	directory := shellQuote(filepath.Dir(destination), "bash")
	file := shellQuote(destination, "bash")
	payload := shellQuote(base64.StdEncoding.EncodeToString(data), "bash")
	warning := shellQuote("warning: sandbox model seed: copy to "+destination+" failed", "bash")
	// Isolate umask and pipefail. Mark writing only after the output opens successfully.
	return "(\n" +
		"umask 077\n" +
		"set -o pipefail\n" +
		"writing=false\n" +
		"if { " + checks +
		"mkdir -p -- " + directory + " &&\n" +
		"chmod 700 -- " + directory + " &&\n" +
		"rm -f -- " + file + " &&\n" +
		"{ writing=true; printf '%s' " + payload + " | base64 --decode; } > " + file + " &&\n" +
		"chmod 600 -- " + file + "; } 2>/dev/null; then\n" +
		":\n" +
		"else\n" +
		"if [ \"$writing\" = true ]; then rm -f -- " + file + " 2>/dev/null || :; fi\n" +
		"printf '%s\\n' " + warning + " >&2\n" +
		"fi\n" +
		")\nexec bash -c " + shellQuote(command, "bash")
}
