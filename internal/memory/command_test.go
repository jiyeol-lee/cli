package memory

import (
	"bytes"
	"context"
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
		{name: "no arguments", want: Command{Help: true}},
		{name: "short help", args: []string{"-h"}, want: Command{Help: true}},
		{name: "directory", args: []string{"directory"}, want: Command{Kind: CommandDirectory}},
		{name: "read defaults all", args: []string{"read"}, want: Command{Kind: CommandRead, Scope: ScopeAll}},
		{name: "read project", args: []string{"read", "--scope", "project"}, want: Command{Kind: CommandRead, Scope: ScopeProject}},
		{
			name: "write category then scope", args: []string{"write", "Keep", "Case", "--category", "preference", "--scope", "global"},
			want: Command{Kind: CommandWrite, Memory: "Keep Case", Category: CategoryPreference, Scope: ScopeGlobal},
		},
		{
			name: "write scope then category", args: []string{"write", "use", "tabs", "--scope", "project", "--category", "convention"},
			want: Command{Kind: CommandWrite, Memory: "use tabs", Category: CategoryConvention, Scope: ScopeProject},
		},
		{name: "archive", args: []string{"archive", "42", "--scope", "project"}, want: Command{Kind: CommandArchive, ID: 42, Scope: ScopeProject}},
		{name: "unknown command", args: []string{"remove"}, wantErr: "unknown memory command"},
		{name: "unknown option", args: []string{"write", "text", "--other", "x"}, wantErr: "unknown memory option"},
		{name: "duplicate category", args: []string{"write", "text", "--category", "note", "--category", "note", "--scope", "global"}, wantErr: "duplicate --category"},
		{name: "duplicate scope", args: []string{"write", "text", "--scope", "global", "--scope", "global", "--category", "note"}, wantErr: "duplicate --scope"},
		{name: "missing option value", args: []string{"write", "text", "--scope"}, wantErr: "missing value"},
		{name: "invalid category", args: []string{"write", "text", "--category", "other", "--scope", "global"}, wantErr: "invalid memory category"},
		{name: "invalid write scope", args: []string{"write", "text", "--category", "note", "--scope", "all"}, wantErr: "invalid memory scope"},
		{name: "word after option", args: []string{"write", "text", "--scope", "global", "more", "--category", "note"}, wantErr: "memory words must precede options"},
		{name: "missing memory", args: []string{"write", "--scope", "global", "--category", "note"}, wantErr: "usage: cli memory write"},
		{name: "nonpositive id", args: []string{"archive", "0", "--scope", "global"}, wantErr: "positive integer"},
		{name: "archive all", args: []string{"archive", "1", "--scope", "all"}, wantErr: "invalid memory scope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCommand(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("command = %#v, want %#v", got, tt.want)
			}
		})
	}
}

type fakeResolver struct {
	directory string
	calls     int
}

func (r *fakeResolver) Resolve(context.Context) (string, error) {
	r.calls++
	return r.directory, nil
}

func TestAppDirectoryAndGlobalCommands(t *testing.T) {
	repo := testRepository(t)
	resolver := &fakeResolver{directory: "/main/worktree"}
	var output bytes.Buffer
	app := App{Repo: repo, Resolver: resolver, Stdout: &output}

	if err := app.Run(context.Background(), Command{Kind: CommandDirectory}); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "/main/worktree\n" {
		t.Fatalf("directory output = %q", got)
	}
	output.Reset()
	if err := app.Run(context.Background(), Command{Kind: CommandWrite, Memory: "global note", Category: CategoryNote, Scope: ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("global write resolved directory; calls = %d", resolver.calls)
	}
	if err := app.Run(context.Background(), Command{Kind: CommandRead, Scope: ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("global read resolved directory; calls = %d", resolver.calls)
	}
	got := output.String()
	if !strings.Contains(got, "id  scope") || !strings.Contains(got, "global") || !strings.Contains(got, "global note") {
		t.Fatalf("table output = %q", got)
	}
	entries, err := repo.Read(context.Background(), ScopeGlobal, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Run(context.Background(), Command{Kind: CommandArchive, ID: entries[0].ID, Scope: ScopeGlobal}); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("global archive resolved directory; calls = %d", resolver.calls)
	}
}

func TestRenderTableWritesHeaderForNoRows(t *testing.T) {
	var output bytes.Buffer
	if err := renderTable(&output, nil); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); !strings.HasPrefix(got, "id  scope  project_directory") {
		t.Fatalf("output = %q", got)
	}
}

func TestRenderTableNormalizesEmbeddedWhitespace(t *testing.T) {
	entries := []Entry{{
		ID:        1,
		Scope:     ScopeGlobal,
		Category:  CategoryNote,
		Memory:    "line\tbreak\nphrase",
		UpdatedAt: "now",
	}}
	var output bytes.Buffer
	if err := renderTable(&output, entries); err != nil {
		t.Fatal(err)
	}
	const want = "id  scope   project_directory  category  memory             updated_at\n" +
		"1   global                     note      line break phrase  now\n"
	if output.String() != want {
		t.Fatalf("renderTable() = %q, want %q", output.String(), want)
	}
}
