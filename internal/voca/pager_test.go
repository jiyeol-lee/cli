package voca

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalPagerPipeline(t *testing.T) {
	tests := []struct {
		name       string
		awk        string
		less       string
		wantOutput bool
		wantError  string
	}{
		{name: "normal", awk: "exec /bin/cat", less: "exec /bin/cat", wantOutput: true},
		{name: "successful pager closes pipe", awk: "exec /usr/bin/yes", less: "IFS= read -r line\nexit 0"},
		{name: "formatter failure", awk: "exit 7", less: "exec /bin/cat", wantError: "format article:"},
		{name: "pager failure with closed formatter pipe", awk: "exec /usr/bin/yes", less: "IFS= read -r line\nexit 9", wantError: "page article:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPagerCommands(t, tt.awk, tt.less)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var output bytes.Buffer
			err := (TerminalPager{}).Show(ctx, ArticlePage{Title: "Title", Body: "Body\n", URL: "https://example.test/article"}, strings.NewReader(""), &output, &bytes.Buffer{})
			if tt.wantError == "" && err != nil {
				t.Fatal(err)
			}
			if tt.wantError != "" && (err == nil || !strings.Contains(err.Error(), tt.wantError)) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
			if tt.wantOutput && !strings.Contains(output.String(), "Title") {
				t.Fatalf("output = %q", output.String())
			}
		})
	}
}

func TestTerminalPagerStartupFailures(t *testing.T) {
	tests := []struct {
		name      string
		awk       string
		less      string
		wantError string
	}{
		{name: "pager", awk: "exec /bin/cat", less: "", wantError: "page article:"},
		{name: "formatter", awk: "", less: "exec /bin/cat", wantError: "format article:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPagerCommands(t, tt.awk, tt.less)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := (TerminalPager{}).Show(ctx, ArticlePage{Title: "Title"}, strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("error = %v, want %q", err, tt.wantError)
			}
		})
	}
}

func installPagerCommands(t *testing.T, awk, less string) {
	t.Helper()
	dir := t.TempDir()
	writeCommand := func(name, body string) {
		t.Helper()
		content := body
		if body != "" {
			content = "#!/bin/sh\n" + body + "\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeCommand("awk", awk)
	writeCommand("less", less)
	t.Setenv("PATH", dir)
	t.Setenv("TMUX", "")
}
