package workmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSandboxRunUsesCheckoutRoot(t *testing.T) {
	sandboxTestNonroot(t)
	for _, checkout := range []string{"main", "linked"} {
		for _, location := range []string{"root", "nested", "symlink"} {
			t.Run(checkout+"/"+location, func(t *testing.T) {
				c, w, engine := sandboxTestFixture(t)
				root := w.Path
				if checkout == "main" {
					root = w.Root
				}
				cwd := root
				if location != "root" {
					cwd = filepath.Join(root, "nested", "directory")
					if err := os.MkdirAll(cwd, 0700); err != nil {
						t.Fatal(err)
					}
				}
				if location == "symlink" {
					target := cwd
					cwd = filepath.Join(c.HomeDir, "caller-alias")
					if err := os.Symlink(target, cwd); err != nil {
						t.Fatal(err)
					}
				}
				var args []string
				c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
					podman := sandboxTestPodmanProcess(p)
					if podman.Name == "podman" && podman.Args[0] == "run" {
						args = slices.Clone(podman.Args)
					}
					return engine.Run(ctx, p)
				})
				app := sandboxCommandApp(c, w)
				app.Getwd = func() (string, error) { return cwd, nil }
				if err := app.Run(t.Context(), Command{Kind: "sandbox", Name: "run", ShellCommand: []string{"pwd"}}); err != nil {
					t.Fatal(err)
				}
				index := slices.Index(args, "--workdir")
				if index < 0 || args[index+1] != root {
					t.Fatalf("workdir = %q, want %q", args, root)
				}
				for _, label := range []string{
					sandboxLabel + "path=" + root,
					sandboxLabel + "workspace=" + identity(w.CommonDir, root),
					sandboxLabel + "repository=" + w.RepoID,
				} {
					if !slices.Contains(args, label) {
						t.Fatalf("missing checkout identity %q in %q", label, args)
					}
				}
				mount := "type=bind,source=" + root + ",target=" + root + ",relabel=shared"
				if !slices.Contains(args, mount) {
					t.Fatalf("missing checkout mount %q in %q", mount, args)
				}
			})
		}
	}
}

func TestSandboxPaneKeepsCheckoutDirectory(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, _ := sandboxTestFixture(t)
	args, err := c.testRunCommand(t.Context(), w, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(args, "--workdir")
	if index < 0 || args[index+1] != w.Path {
		t.Fatalf("managed pane changed workdir: %q", args)
	}
}
