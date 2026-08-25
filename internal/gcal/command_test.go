package gcal

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Command
		wantErr string
	}{
		{name: "list", args: []string{"list"}, want: Command{Name: "list"}},
		{name: "soon text", args: []string{"soon", "--text"}, want: Command{Name: "soon", Text: true}},
		{name: "in progress text", args: []string{"in-progress", "--text"}, want: Command{Name: "in-progress", Text: true}},
		{name: "text then join", args: []string{"list", "--text", "--join", ","}, want: Command{Name: "list", Text: true, Join: ",", JoinSet: true}},
		{name: "join then text", args: []string{"soon", "--join", " | ", "--text"}, want: Command{Name: "soon", Text: true, Join: " | ", JoinSet: true}},
		{name: "empty join", args: []string{"in-progress", "--join", "", "--text"}, want: Command{Name: "in-progress", Text: true, JoinSet: true}},
		{name: "help", args: []string{"help"}, want: Command{Help: true}},
		{name: "short help", args: []string{"-h"}, want: Command{Help: true}},
		{name: "long help", args: []string{"--help"}, want: Command{Help: true}},
		{name: "list help", args: []string{"list", "-h"}, want: Command{Help: true}},
		{name: "soon help", args: []string{"soon", "--help"}, want: Command{Help: true}},
		{name: "in progress help", args: []string{"in-progress", "--help"}, want: Command{Help: true}},
		{name: "missing command", args: nil, wantErr: Usage},
		{name: "unknown command", args: []string{"bad"}, wantErr: "unknown gcal command"},
		{name: "join without text", args: []string{"list", "--join", ","}, wantErr: "--join requires --text"},
		{name: "missing separator", args: []string{"soon", "--text", "--join"}, wantErr: "missing separator for --join"},
		{name: "duplicate text", args: []string{"list", "--text", "--text"}, wantErr: "duplicate --text"},
		{name: "duplicate join", args: []string{"list", "--text", "--join", ",", "--join", " "}, wantErr: "duplicate --join"},
		{name: "unknown option", args: []string{"list", "--text", "--json"}, wantErr: "usage: cli gcal list"},
		{name: "positional extra", args: []string{"soon", "--text", "extra"}, wantErr: "usage: cli gcal soon"},
		{name: "command help with option", args: []string{"in-progress", "--help", "--text"}, wantErr: "usage: cli gcal in-progress"},
		{name: "raw option separator still requires text", args: []string{"list", "--join", "--text"}, wantErr: "--join requires --text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCommand(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("ParseCommand(%q) error = %v, want containing %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCommand(%q) error = %v", tt.args, err)
			}
			if got != tt.want {
				t.Fatalf("ParseCommand(%q) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}
