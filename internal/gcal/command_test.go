package gcal

import "testing"

func TestParseCommand(t *testing.T) {
	tests := []struct {
		args    []string
		want    Command
		wantErr bool
	}{
		{args: []string{"list"}, want: Command{Name: "list"}},
		{args: []string{"soon", "--text"}, want: Command{Name: "soon", Text: true}},
		{args: []string{"help"}, want: Command{Help: true}},
		{args: []string{"-h"}, want: Command{Help: true}},
		{args: []string{"--help"}, want: Command{Help: true}},
		{args: []string{"list", "-h"}, want: Command{Help: true}},
		{args: []string{"list", "--help"}, want: Command{Help: true}},
		{args: []string{"soon", "-h"}, want: Command{Help: true}},
		{args: []string{"soon", "--help"}, want: Command{Help: true}},
		{args: []string{"in-progress", "-h"}, want: Command{Help: true}},
		{args: []string{"in-progress", "--help"}, want: Command{Help: true}},
		{args: nil, wantErr: true},
		{args: []string{"bad"}, wantErr: true},
		{args: []string{"list", "help"}, wantErr: true},
		{args: []string{"list", "--text", "extra"}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseCommand(tt.args)
		if (err != nil) != tt.wantErr {
			t.Fatalf("ParseCommand(%q) error = %v", tt.args, err)
		}
		if got != tt.want {
			t.Fatalf("ParseCommand(%q) = %#v, want %#v", tt.args, got, tt.want)
		}
	}
}
