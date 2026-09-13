package main

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
)

func TestSandboxShellExitCode(t *testing.T) {
	err := exec.Command("/bin/bash", "-c", "exit 23").Run()
	for _, test := range []struct {
		args []string
		err  error
		want int
	}{
		{[]string{"workmux", "sandbox", "shell"}, fmt.Errorf("run sandbox: %w", err), 23},
		{[]string{"workmux", "sandbox", "shell", "-e"}, err, 23},
		{[]string{"workmux", "sandbox", "shell"}, context.Canceled, 130},
		{[]string{"workmux", "sandbox", "build"}, err, 1},
		{[]string{"workmux", "open", "topic"}, err, 1},
		{[]string{"workmux", "sandbox", "shell"}, fmt.Errorf("ownership mismatch"), 1},
	} {
		if got := commandExitCode(test.args, test.err); got != test.want {
			t.Fatalf("exit %v: %d, want %d", test.args, got, test.want)
		}
	}
}
