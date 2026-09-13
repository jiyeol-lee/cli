package workmux

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Process struct {
	Name         string
	Args         []string
	Dir          string
	Env          []string
	CleanGitEnv  bool
	ProcessGroup bool
	Stdin        io.Reader
	Stdout       io.Writer
	Stderr       io.Writer
}

type Runner interface {
	Run(context.Context, Process) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, p Process) ([]byte, error) {
	cmd := exec.CommandContext(ctx, p.Name, p.Args...)
	cmd.WaitDelay = time.Second
	group := p.ProcessGroup
	if stdin, ok := p.Stdin.(*os.File); ok {
		info, err := stdin.Stat()
		if err == nil && info.Mode()&os.ModeCharDevice != 0 {
			// A new background group cannot read the caller's terminal (SIGTTIN).
			group = false
		}
	}
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			if err == syscall.ESRCH {
				return os.ErrProcessDone
			}
			return err
		}
	}
	cmd.Dir = p.Dir
	cmd.Env = os.Environ()
	if p.CleanGitEnv {
		env := make([]string, 0, len(cmd.Env))
		for _, entry := range cmd.Env {
			if !strings.HasPrefix(entry, "GIT_") {
				env = append(env, entry)
			}
		}
		cmd.Env = env
	}
	cmd.Env = append(cmd.Env, p.Env...)
	cmd.Stdin = p.Stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = p.Stdout
	if cmd.Stdout == nil {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr
	if p.Stderr != nil {
		cmd.Stderr = io.MultiWriter(p.Stderr, &stderr)
	}
	err := cmd.Run()
	if ctx.Err() != nil {
		return stdout.Bytes(), ctx.Err()
	}
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message != "" {
			return stdout.Bytes(), fmt.Errorf("run %s: %w: %s", p.Name, err, message)
		}
		return stdout.Bytes(), fmt.Errorf("run %s: %w", p.Name, err)
	}
	return stdout.Bytes(), nil
}
