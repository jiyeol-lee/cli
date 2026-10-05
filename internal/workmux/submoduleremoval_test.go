package workmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxEmptyModulesDirectoryBlocksNativeRemoval(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, _ := sandboxTestFixture(t)
	if _, err := c.testRunCommand(t.Context(), w, "opencode"); err != nil {
		t.Fatal(err)
	}
	g := gitHost{runner: c.Runner}
	admin, err := g.run(t.Context(), w.Path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(strings.TrimSpace(string(admin)), "modules")
	entries, err := os.ReadDir(modules)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected an empty sandbox-created modules directory: %v, %v", entries, err)
	}
	if _, err := g.run(t.Context(), w.Root, "worktree", "remove", "--", w.Path); err == nil || !strings.Contains(err.Error(), "containing submodules") {
		t.Fatalf("native removal did not reject empty modules directory: %v", err)
	}
	if err := os.Remove(modules); err != nil {
		t.Fatal(err)
	}
	if _, err := g.run(t.Context(), w.Root, "worktree", "remove", "--", w.Path); err != nil {
		t.Fatalf("native removal failed after removing only empty metadata: %v", err)
	}
}
