package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jiyeol-lee/cli/internal/gcal"
	"github.com/jiyeol-lee/cli/internal/memory"
	"github.com/jiyeol-lee/cli/internal/xdg"
)

func TestInvalidGcalInvocationsDoNotConstructOAuthDependencies(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "missing", args: []string{"gcal"}, wantErr: true},
		{name: "unknown", args: []string{"gcal", "remove"}, wantErr: true},
		{name: "invalid option", args: []string{"gcal", "list", "--json"}, wantErr: true},
		{name: "extra argument", args: []string{"gcal", "soon", "--text", "extra"}, wantErr: true},
		{name: "help", args: []string{"gcal", "--help"}},
		{name: "command help", args: []string{"gcal", "list", "--help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			var out bytes.Buffer
			deps := dependencies{
				stdout: &out,
				newCalendar: func(context.Context, xdg.Dirs) (gcal.Calendar, error) {
					called = true
					return gcal.Calendar{}, nil
				},
			}
			err := runWithDependencies(context.Background(), tt.args, deps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr = %v", err, tt.wantErr)
			}
			if called {
				t.Fatal("calendar dependency was constructed")
			}
		})
	}
}

func TestInvalidCommandsDoNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown app", args: []string{"typo"}, want: `unknown app "typo"`},
		{name: "missing voca command", args: []string{"voca"}, want: "usage: cli voca"},
		{name: "invalid voca command", args: []string{"voca", "typo"}, want: `unknown voca command "typo"`},
		{name: "missing add phrase", args: []string{"voca", "add"}, want: "usage: cli voca add <phrase>"},
		{name: "missing delete phrase", args: []string{"voca", "delete"}, want: "usage: cli voca delete <phrase>"},
		{name: "extra list argument", args: []string{"voca", "list", "extra"}, want: "usage: cli voca list"},
		{name: "extra story argument", args: []string{"voca", "story", "extra"}, want: "usage: cli voca story"},
		{name: "extra news argument", args: []string{"voca", "news", "extra"}, want: "usage: cli voca news"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithDependencies(context.Background(), tt.args, dependencies{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestGcalHelpDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	called := false
	var out bytes.Buffer
	deps := dependencies{
		stdout: &out,
		newCalendar: func(context.Context, xdg.Dirs) (gcal.Calendar, error) {
			called = true
			return gcal.Calendar{}, nil
		},
	}
	if err := runWithDependencies(context.Background(), []string{"gcal", "--help"}, deps); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("calendar dependency was constructed")
	}
	if got, want := out.String(), gcal.Usage+"\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestInvalidMemoryCommandsDoNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unknown", args: []string{"memory", "remove"}, want: `unknown memory command "remove"`},
		{name: "bad read scope", args: []string{"memory", "read", "--scope", "local"}, want: "invalid memory scope"},
		{name: "missing write options", args: []string{"memory", "write", "note"}, want: "usage: cli memory write"},
		{name: "bad archive id", args: []string{"memory", "archive", "zero", "--scope", "global"}, want: "positive integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWithDependencies(context.Background(), tt.args, dependencies{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestMemoryHelpDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	for _, args := range [][]string{{"memory"}, {"memory", "--help"}, {"memory", "-h"}} {
		var out bytes.Buffer
		if err := runWithDependencies(context.Background(), args, dependencies{stdout: &out}); err != nil {
			t.Fatal(err)
		}
		if got, want := out.String(), memory.Usage+"\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
	}
}

type fixedDirectoryResolver struct {
	directory string
}

func (r fixedDirectoryResolver) Resolve(context.Context) (string, error) {
	return r.directory, nil
}

func TestMemoryDirectoryDoesNotRequireXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")
	var out bytes.Buffer
	deps := dependencies{
		directoryResolver: fixedDirectoryResolver{directory: "/main/worktree"},
		stdout:            &out,
	}
	if err := runWithDependencies(context.Background(), []string{"memory", "directory"}, deps); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "/main/worktree\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}
