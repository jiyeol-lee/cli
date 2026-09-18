package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The child executes the real CLI entry point. Only Podman is replaced; Git,
// repository locks, saved state, signal forwarding, and cleanup are real.
func TestSandboxSignalProcess(t *testing.T) {
	mode := os.Getenv("CLI_SIGNAL_HELPER")
	if mode == "" {
		return
	}
	if mode != "podman" {
		os.Args = []string{"cli", "workmux", "sandbox", "shell"}
		if mode == "exec" {
			os.Args = append(os.Args, "-e")
		}
		main()
		os.Exit(0)
	}
	base := os.Getenv("CLI_SIGNAL_BASE")
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	if len(args) == 0 {
		os.Exit(2)
	}
	write := func(name string, data []byte) {
		if err := os.WriteFile(filepath.Join(base, name), data, 0600); err != nil {
			panic(err)
		}
	}
	image := "sha256:" + strings.Repeat("b", 64)
	switch args[0] {
	case "info":
		fmt.Printf(`{"Host":{"Hostname":"fixture","DatabaseBackend":"sqlite"},"Store":{"GraphRoot":%q,"RunRoot":%q}}`, filepath.Join(base, "store"), filepath.Join(base, "run"))
	case "image":
		fmt.Printf(`[{"Id":%q}]`, image)
	case "container":
		data, err := os.ReadFile(filepath.Join(base, "container.json"))
		if args[1] == "inspect" {
			if err != nil {
				os.Exit(1)
			}
			if _, err := os.Stdout.Write(data); err != nil {
				panic(err)
			}
		} else if err == nil {
			var items []struct{ Name string }
			if err := json.Unmarshal(data, &items); err != nil {
				panic(err)
			}
			fmt.Println(items[0].Name)
		}
	case "run", "exec":
		kind := args[0]
		if kind == "run" {
			name := ""
			labels := make(map[string]string)
			for i, arg := range args {
				if arg == "--name" {
					name = args[i+1]
				}
				if arg == "--label" {
					key, value, _ := strings.Cut(args[i+1], "=")
					labels[key] = value
				}
			}
			data, err := json.Marshal([]any{map[string]any{"Id": strings.Repeat("a", 64), "Name": name, "Image": image, "State": map[string]bool{"Running": true}, "Config": map[string]any{"Labels": labels}}})
			if err != nil {
				panic(err)
			}
			write("container.json", data)
			if os.Getenv("CLI_SIGNAL_CLEANUP") == "1" {
				write("run.success", nil)
				os.Exit(0)
			}
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		write(kind+".ready", nil)
		var received os.Signal
		select {
		case received = <-signals:
		case <-time.After(30 * time.Second):
			os.Exit(124)
		}
		write(kind+".signal", []byte(received.String()))
		if received == syscall.SIGTERM {
			os.Exit(143)
		}
		os.Exit(130)
	case "rm":
		if os.Getenv("CLI_SIGNAL_CLEANUP") == "1" {
			write("cleanup.ready", nil)
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(base, "cleanup.release")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					os.Exit(124)
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
		if err := os.Remove(filepath.Join(base, "container.json")); err != nil {
			os.Exit(1)
		}
		write("removed", nil)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestSandboxCLISignals(t *testing.T) {
	if os.Getuid() == 0 || os.Getgid() == 0 {
		t.Skip("protected sandbox mounts require a nonroot UID:GID")
	}
	for _, received := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		for _, mode := range []string{"fresh", "exec", "cleanup"} {
			t.Run(fmt.Sprintf("%s/%s", mode, received), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				defer cancel()
				base := t.TempDir()
				home, root, linked, bin := filepath.Join(base, "home"), filepath.Join(base, "main"), filepath.Join(base, "linked"), filepath.Join(base, "bin")
				for _, path := range []string{home, root, bin} {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				script := "#!/bin/bash\nCLI_SIGNAL_HELPER=podman exec '" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "' -test.run=^TestSandboxSignalProcess$ -- \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				env := []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "CLI_SIGNAL_BASE=" + base, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GORACE=atexit_sleep_ms=0"}
				if mode == "cleanup" {
					env = append(env, "CLI_SIGNAL_CLEANUP=1")
				}
				git := func(args ...string) {
					cmd := exec.CommandContext(ctx, "git", args...)
					cmd.Dir, cmd.Env = root, env
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git %v: %v %s", args, err, out)
					}
				}
				git("init", "-q", "--template=", "--initial-branch=main")
				git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "initial")
				git("worktree", "add", "-qb", "topic", linked)
				waitFile := func(name string) {
					for {
						if _, err := os.Stat(filepath.Join(base, name)); err == nil {
							return
						}
						select {
						case <-ctx.Done():
							t.Fatalf("waiting for %s", name)
						case <-time.After(5 * time.Millisecond):
						}
					}
				}
				start := func(kind string) (*exec.Cmd, <-chan error, *bytes.Buffer) {
					cmd := exec.CommandContext(ctx, binary, "-test.run=^TestSandboxSignalProcess$")
					cmd.Dir, cmd.Env = linked, append(append([]string{}, env...), "CLI_SIGNAL_HELPER="+kind)
					var output bytes.Buffer
					cmd.Stdout, cmd.Stderr = &output, &output
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() { done <- cmd.Wait() }()
					return cmd, done, &output
				}
				original, originalDone, originalOutput := start("fresh")
				if mode == "cleanup" {
					waitFile("run.success")
					waitFile("cleanup.ready")
					if err := original.Process.Signal(received); err != nil {
						t.Fatal(err)
					}
					// Leave cleanup blocked while the CLI dispatches the signal.
					// Its WithoutCancel context must still allow rm to succeed.
					select {
					case err := <-originalDone:
						t.Fatalf("CLI exited before cleanup completed: %v", err)
					case <-time.After(100 * time.Millisecond):
					}
					if err := os.WriteFile(filepath.Join(base, "cleanup.release"), nil, 0600); err != nil {
						t.Fatal(err)
					}
					err := <-originalDone
					if _, err := os.Stat(filepath.Join(base, "removed")); err != nil {
						t.Fatal("signal interrupted cleanup", err)
					}
					if _, err := os.Stat(filepath.Join(base, "container.json")); !os.IsNotExist(err) {
						t.Fatal("cleanup left the container registered")
					}
					if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 128+int(received) {
						t.Fatalf("CLI exit after successful foreground and cleanup = %v, output=%s", err, originalOutput)
					}
					return
				}
				waitFile("run.ready")
				// The fake engine's ready file precedes the CLI's ownership inspection.
				stateBase := filepath.Join(home, ".local", "state", "cli", "workmux")
				for {
					entries, err := os.ReadDir(stateBase)
					if err != nil {
						t.Fatal(err)
					}
					unlocked := false
					for _, entry := range entries {
						if len(entry.Name()) != 32 {
							continue
						}
						file, err := os.OpenFile(filepath.Join(stateBase, entry.Name(), "lock"), os.O_RDWR, 0)
						if err != nil {
							t.Fatal(err)
						}
						if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
							if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
								t.Error(err)
							}
							unlocked = true
						}
						if err := file.Close(); err != nil {
							t.Fatal(err)
						}
					}
					if unlocked {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("shell did not release repository lock")
					case <-time.After(5 * time.Millisecond):
					}
				}
				cmd, done, output, kind := original, originalDone, originalOutput, "run"
				if mode == "exec" {
					cmd, done, output = start("exec")
					kind = "exec"
					waitFile("exec.ready")
				}
				if err := cmd.Process.Signal(received); err != nil {
					t.Fatal(err)
				}
				var exit *exec.ExitError
				err = <-done
				if !errors.As(err, &exit) || exit.ExitCode() != 128+int(received) {
					t.Fatalf("CLI exit = %v, output=%s", err, output)
				}
				forwarded, err := os.ReadFile(filepath.Join(base, kind+".signal"))
				if err != nil || string(forwarded) != received.String() {
					t.Fatalf("forwarded signal = %q, %v", forwarded, err)
				}
				if mode == "exec" {
					if _, err := os.Stat(filepath.Join(base, "container.json")); err != nil {
						t.Fatal("exec removed original container", err)
					}
					if _, err := os.Stat(filepath.Join(base, "removed")); !os.IsNotExist(err) {
						t.Fatal("exec invoked fresh cleanup")
					}
					if err := original.Process.Signal(syscall.SIGTERM); err != nil {
						t.Fatal(err)
					}
					<-originalDone
				}
				if _, err := os.Stat(filepath.Join(base, "container.json")); !os.IsNotExist(err) {
					t.Fatal("fresh cancellation left container registered")
				}
				if _, err := os.Stat(filepath.Join(base, "removed")); err != nil {
					t.Fatal("fresh cancellation skipped owned cleanup", err)
				}
			})
		}
	}
}
