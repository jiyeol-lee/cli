package workmux

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigForRepoValidatesAbsoluteFilePatterns(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		for _, location := range []string{"inside", "outside", "sibling-prefix"} {
			t.Run(field+"/"+location, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "project")
				configDir := filepath.Join(base, "config")
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(configDir, 0700); err != nil {
					t.Fatal(err)
				}
				pattern := filepath.Join(root, "nested", "*.env")
				switch location {
				case "outside":
					pattern = filepath.Join(base, "outside", "*.env")
				case "sibling-prefix":
					pattern = filepath.Join(root+"-other", "*.env")
				}
				path := writeConfigFixture(t, configDir, "config.yaml", "files: {"+field+": ['"+pattern+"']}\n")
				if location != "inside" {
					writeConfigFixture(t, configDir, "project.yaml", "files: {"+field+": []}\n")
				}
				config, err := LoadConfigForRepo(configDir, root)
				if location == "inside" {
					if err != nil {
						t.Fatal(err)
					}
					patterns := config.Files.Copy
					if field == "symlink" {
						patterns = config.Files.Symlink
					}
					if len(patterns) != 1 || patterns[0] != pattern {
						t.Fatalf("absolute pattern changed: %q", patterns)
					}
				} else if err == nil || !strings.Contains(err.Error(), "outside the repository") || !strings.Contains(err.Error(), path) {
					t.Fatalf("masked outside pattern error = %v", err)
				}
			})
		}
	}
}

func TestOutsideFilePatternPrecedesProvisioning(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		t.Run(field, func(t *testing.T) {
			f := newHostFixture(t, "")
			pattern := filepath.Join(f.home, "*.env")
			hostWrite(t, filepath.Join(f.config, "config.yaml"), "files: {"+field+": ['"+pattern+"']}\npost_create: [must-not-run]\n")
			if err := f.run(t, "add", "topic"); err == nil || !strings.Contains(err.Error(), "outside the repository") {
				t.Fatalf("outside pattern was not rejected: %v", err)
			}
			if _, err := os.Stat(workspacePath(f.root, "topic")); !os.IsNotExist(err) {
				t.Fatalf("invalid config created a worktree: %v", err)
			}
			if len(f.events) != 0 {
				t.Fatalf("invalid config caused side effects: %q", f.events)
			}
		})
	}
}

func TestLoadConfigForRepoProtectsGitMetadata(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		for _, test := range []struct {
			pattern string
			allowed bool
		}{
			{".git", false},
			{"././.git//config", false},
			{".git/**", false},
			{"absolute root", false},
			{"absolute descendant", false},
			{"absolute metadata glob", false},
			{".", true},
			{"*", true},
			{"**", true},
			{"**/.git/config", true},
			{"a/.git/config", true},
			{"absolute nested metadata", true},
			{"absolute broad glob", true},
		} {
			t.Run(field+"/"+test.pattern, func(t *testing.T) {
				root, configDir := t.TempDir(), t.TempDir()
				pattern := test.pattern
				switch pattern {
				case "absolute root":
					pattern = filepath.Join(root, ".git")
				case "absolute descendant":
					pattern = root + "/./.git//config"
				case "absolute metadata glob":
					pattern = filepath.Join(root, ".git", "**")
				case "absolute nested metadata":
					pattern = filepath.Join(root, "a", ".git", "config")
				case "absolute broad glob":
					pattern = filepath.Join(root, "**")
				}
				path := writeConfigFixture(t, configDir, "config.yaml", "files: {"+field+": ['"+pattern+"']}\n")
				writeConfigFixture(t, configDir, filepath.Base(root)+".yaml", "files: {"+field+": []}\n")
				_, err := LoadConfigForRepo(configDir, root)
				if test.allowed {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), "root .git metadata") || !strings.Contains(err.Error(), path) {
					t.Fatalf("masked metadata pattern error = %v", err)
				}
			})
		}
	}
}

func TestGitMetadataPatternPrecedesProvisioning(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		for _, pattern := range []string{".git", "././.git//config", ".git/**", "absolute metadata"} {
			t.Run(field+"/"+pattern, func(t *testing.T) {
				f := newHostFixture(t, "")
				if pattern == "absolute metadata" {
					pattern = filepath.Join(f.root, ".git", "config")
				}
				hostWrite(t, filepath.Join(f.config, "config.yaml"), "files: {"+field+": ['"+pattern+"']}\npost_create: [must-not-run]\n")
				if err := f.run(t, "add", "topic"); err == nil || !strings.Contains(err.Error(), "root .git metadata") {
					t.Fatalf("metadata pattern was not rejected: %v", err)
				}
				if _, err := os.Stat(workspacePath(f.root, "topic")); !os.IsNotExist(err) {
					t.Fatalf("invalid config created a worktree: %v", err)
				}
				if len(f.events) != 0 {
					t.Fatalf("invalid config caused side effects: %q", f.events)
				}
			})
		}
	}
}
