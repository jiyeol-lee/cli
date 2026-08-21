package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/jiyeol-lee/cli/internal/gcal"
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
