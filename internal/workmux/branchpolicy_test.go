package workmux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func savePolicyState(t *testing.T, f *hostFixture, state workspaceState) {
	t.Helper()
	store := hostStateStore(t, f)
	if err := store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
}

func policyCommit(t *testing.T, f *hostFixture) workspaceState {
	t.Helper()
	if err := f.run(t, "add", "topic"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "committed source\n")
	hostGit(t, w.Path, "add", "tracked")
	hostGit(t, w.Path, "commit", "-m", "source")
	return w
}

type policyReader func([]byte) (int, error)

func (read policyReader) Read(data []byte) (int, error) { return read(data) }

func dirtySource(t *testing.T, w workspaceState) {
	t.Helper()
	hostWrite(t, filepath.Join(w.Path, "tracked"), "staged\n")
	hostGit(t, w.Path, "add", "tracked")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "unstaged\n")
	hostWrite(t, filepath.Join(w.Path, "untracked"), "local\n")
	hostWrite(t, filepath.Join(w.Path, ".env"), "ignored\n")
}

func checkFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("%s = %q, %v; want %q", path, data, err, want)
	}
}

func TestManagedWorkspaceBranchIdentity(t *testing.T) {
	for _, change := range []string{"switch", "rename", "detach"} {
		for _, stage := range []string{"ready", "closed"} {
			t.Run(change+"/"+stage, func(t *testing.T) {
				f := newHostFixture(t, "post_create: [create-hook]\npre_merge: [merge-hook]\npre_remove: [remove-hook]\nfiles: {copy: [.env]}\nsandbox: {enabled: true}\npanes: [{command: opencode}]\n")
				hostWrite(t, filepath.Join(f.root, ".env"), "original source\n")
				if err := f.run(t, "add", "feature/topic"); err != nil {
					t.Fatal(err)
				}
				w := f.load(t, "feature/topic")
				if w.Branch != "feature/topic" || w.Handle != "feature-topic" {
					t.Fatalf("unexpected workspace identity: %+v", w)
				}
				f.sandbox.present = true
				hostWrite(t, filepath.Join(w.Path, "tracked"), "committed source\n")
				hostGit(t, w.Path, "commit", "-am", "source")
				if stage == "closed" {
					if err := f.run(t, "close", w.Handle); err != nil {
						t.Fatal(err)
					}
				}
				actual := "other/topic"
				switch change {
				case "switch":
					hostGit(t, w.Path, "switch", "-c", actual)
				case "rename":
					hostGit(t, w.Path, "branch", "-m", actual)
				case "detach":
					hostGit(t, w.Path, "switch", "--detach")
					actual = "detached HEAD"
				}
				dirtySource(t, w)
				hostWrite(t, filepath.Join(f.root, ".env"), "updated source\n")
				journalPath := filepath.Join(f.state, w.RepoID, w.ID+".json")
				journal, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatal(err)
				}
				refs := hostGit(t, f.root, "for-each-ref", "--format=%(refname) %(objectname)")
				history := hostGit(t, f.root, "log", "--all", "--format=%H %P %s")
				targetHead := hostGit(t, f.root, "rev-parse", "HEAD")
				sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
				status := hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all")
				index := hostGit(t, w.Path, "diff", "--cached", "--binary")
				trees := hostGit(t, f.root, "worktree", "list", "--porcelain")
				window, container := f.mux.window, f.sandbox.present
				f.app.Stdin = policyReader(func([]byte) (int, error) {
					t.Fatal("branch mismatch prompted for discard")
					return 0, io.EOF
				})
				for _, kind := range []string{"merge", "remove", "add", "open", "close"} {
					names := []string{w.Branch, w.Handle}
					if change != "detach" {
						names = append(names, actual)
					}
					if kind == "merge" || kind == "remove" || kind == "close" {
						names = append(names, "", ".")
					}
					for _, name := range names {
						f.events, f.runner.processes = nil, nil
						f.stdout.Reset()
						cwd := f.root
						if name == "" || name == "." {
							cwd = w.Path
						}
						f.app.Getwd = func() (string, error) { return cwd, nil }
						args := []string{kind}
						if name != "" {
							args = append(args, name)
						}
						err := f.run(t, args...)
						if err == nil || !strings.Contains(err.Error(), `records branch "feature/topic"`) || !strings.Contains(err.Error(), `on "`+actual+`"`) || !strings.Contains(err.Error(), `restore the recorded branch "feature/topic" before retrying`) {
							t.Fatalf("%q did not refuse branch mismatch with restoration guidance: %v", args, err)
						}
						if len(f.events) != 0 || f.stdout.Len() != 0 || f.mux.window != window || f.sandbox.present != container {
							t.Fatalf("%q ran lifecycle or confirmation: events=%q output=%q", args, f.events, f.stdout.String())
						}
						after, err := os.ReadFile(journalPath)
						if err != nil || !bytes.Equal(journal, after) {
							t.Fatalf("%q changed recovery journal: %v", args, err)
						}
						if hostGit(t, f.root, "for-each-ref", "--format=%(refname) %(objectname)") != refs || hostGit(t, f.root, "log", "--all", "--format=%H %P %s") != history || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "worktree", "list", "--porcelain") != trees {
							t.Fatalf("%q changed branches, history, target, or worktree registration", args)
						}
						if hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all") != status || hostGit(t, w.Path, "diff", "--cached", "--binary") != index {
							t.Fatalf("%q changed source work or index", args)
						}
						checkFile(t, filepath.Join(w.Path, "tracked"), "unstaged\n")
						checkFile(t, filepath.Join(w.Path, "untracked"), "local\n")
						checkFile(t, filepath.Join(w.Path, ".env"), "ignored\n")
						checkFile(t, filepath.Join(f.root, ".env"), "updated source\n")
						checkFile(t, filepath.Join(f.root, "tracked"), "initial\n")
					}
				}
				if change == "rename" {
					hostGit(t, w.Path, "branch", "-m", w.Branch)
				} else {
					hostGit(t, w.Path, "switch", w.Branch)
				}
				f.app.Getwd = func() (string, error) { return f.root, nil }
				if err := f.run(t, "open", w.Handle); err != nil {
					t.Fatalf("restored branch could not open: %v", err)
				}
				f.app.Stdin = strings.NewReader("y\n")
				kind := "merge"
				if stage == "closed" {
					kind = "remove"
				}
				if err := f.run(t, kind, w.Handle); err != nil {
					t.Fatalf("restored branch could not %s: %v", kind, err)
				}
				if state := f.load(t, w.Branch); state.Stage != "removed" || state.Branch != w.Branch || state.Removal == nil || !state.Removal.BranchDeleted {
					t.Fatalf("restored branch did not complete normal cleanup: %+v", state)
				}
			})
		}
	}
}

func TestPlainWorktreeAdoptsCurrentBranchOnce(t *testing.T) {
	f := newHostFixture(t, "post_create: [must-not-run]\n")
	path := filepath.Join(t.TempDir(), "plain")
	hostGit(t, f.root, "worktree", "add", "-b", "original/topic", path)
	hostGit(t, path, "switch", "-c", "feature/topic")
	f.runner.hook = func(Process) error { t.Fatal("plain worktree ran setup hook"); return nil }
	if err := f.run(t, "open", "feature/topic"); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(f.root, ".git")
	journalPath := filepath.Join(f.state, identity(common), identity(common, path)+".json")
	w, err := readState(journalPath)
	if err != nil || w.Branch != "feature/topic" || w.Path != path || w.Handle != "plain" {
		t.Fatalf("plain adoption did not record current branch and path: %+v, %v", w, err)
	}
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	hostGit(t, path, "switch", "-c", "other/topic")
	f.events = nil
	for _, name := range []string{w.Branch, w.Handle, "other/topic"} {
		if err := f.run(t, "open", name); err == nil || !strings.Contains(err.Error(), `records branch "feature/topic"`) || !strings.Contains(err.Error(), `on "other/topic"`) || !strings.Contains(err.Error(), "restore the recorded branch") {
			t.Fatalf("adopted worktree silently changed branch through %q: %v", name, err)
		}
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(journal, after) || len(f.events) != 0 {
		t.Fatalf("branch drift changed adopted state or runtime: %v, %q", err, f.events)
	}
	hostGit(t, path, "switch", w.Branch)
	if err := f.run(t, "open", w.Handle); err != nil {
		t.Fatalf("restored adopted branch could not open: %v", err)
	}
}

func TestRemovedWorkspaceDoesNotBindReplacementBranch(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "feature/topic"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "feature/topic")
	if err := f.run(t, "remove", w.Branch); err != nil {
		t.Fatal(err)
	}
	hostGit(t, f.root, "worktree", "add", "-b", "replacement/topic", w.Path)
	journalPath := filepath.Join(f.state, w.RepoID, w.ID+".json")
	journal, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	f.events = nil
	if err := f.run(t, "remove", w.Branch); err != nil {
		t.Fatalf("completed removal was blocked by a replacement branch: %v", err)
	}
	after, err := os.ReadFile(journalPath)
	if err != nil || !bytes.Equal(journal, after) || len(f.events) != 0 {
		t.Fatalf("completed removal changed replacement state or runtime: %v, %q", err, f.events)
	}
	checkFile(t, filepath.Join(w.Path, "tracked"), "initial\n")
	if hostGit(t, w.Path, "branch", "--show-current") != "replacement/topic" {
		t.Fatal("completed removal changed replacement branch")
	}
	if err := f.run(t, "open", "replacement/topic"); err != nil {
		t.Fatalf("removed record blocked plain replacement adoption: %v", err)
	}
	if state := f.load(t, w.Branch); state.Branch != "replacement/topic" || state.Stage != "ready" || state.Removal != nil {
		t.Fatalf("replacement did not receive its own branch identity: %+v", state)
	}
}

func TestDirtyCancellationNeverMutatesGitOrRunsHooks(t *testing.T) {
	for _, kind := range []string{"merge", "remove"} {
		for _, answer := range []string{"N\n", "\n", "", "y", "yes\n"} {
			t.Run(kind+"/"+answer, func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [merge-hook]\npre_remove: [remove-hook]\n")
				w := policyCommit(t, f)
				dirtySource(t, w)
				sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
				status := hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all")
				index := hostGit(t, w.Path, "diff", "--cached", "--binary")
				journal, err := os.ReadFile(filepath.Join(f.state, w.RepoID, w.ID+".json"))
				if err != nil {
					t.Fatal(err)
				}
				f.events, f.runner.processes = nil, nil
				f.app.Stdin = strings.NewReader(answer)
				args := []string{"merge", "topic"}
				if kind == "remove" {
					args[0] = "remove"
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal(err)
				}
				if hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead || hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all") != status || hostGit(t, w.Path, "diff", "--cached", "--binary") != index {
					t.Fatal("cancellation mutated Git")
				}
				after, _ := os.ReadFile(filepath.Join(f.state, w.RepoID, w.ID+".json"))
				if !bytes.Equal(journal, after) {
					t.Fatal("cancellation rewrote journal")
				}
				for _, event := range f.events {
					if strings.HasPrefix(event, "hook:") || event == "mux:close" || event == "mux:capture" {
						t.Fatalf("cancelled operation ran lifecycle: %q", f.events)
					}
				}
				checkFile(t, filepath.Join(w.Path, "tracked"), "unstaged\n")
				checkFile(t, filepath.Join(w.Path, "untracked"), "local\n")
			})
		}
	}
}

type policyAdoptMux struct {
	*hostTestMux
	adopted bool
}

func (mux *policyAdoptMux) Adopt(context.Context, Workspace) (string, error) {
	mux.adopted = true
	return "@7", nil
}

func TestDirtyPlainCancellationDoesNotAdoptOrSave(t *testing.T) {
	for _, kind := range []string{"remove", "merge"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [must-not-run]\npre_remove: [must-not-run]\n")
			path := filepath.Join(t.TempDir(), "plain")
			hostGit(t, f.root, "worktree", "add", "-b", "topic", path)
			hostWrite(t, filepath.Join(path, "tracked"), "dirty")
			mux := &policyAdoptMux{hostTestMux: f.mux}
			f.app.Mux, f.app.Stdin = mux, strings.NewReader("N\n")
			if err := f.run(t, kind, "topic"); err != nil {
				t.Fatal(err)
			}
			if mux.adopted {
				t.Fatal("cancelled command adopted tmux window")
			}
			entries, err := os.ReadDir(filepath.Join(f.state, identity(filepath.Join(f.root, ".git"))))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") {
					t.Fatal("cancelled plain command saved journal")
				}
			}
		})
	}
}

func TestDirtyApprovalDiscardsBeforeHooksWithoutCommitting(t *testing.T) {
	for _, kind := range []string{"merge", "remove"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [merge-hook]\npre_remove: [remove-hook]\n")
			w := policyCommit(t, f)
			head := hostGit(t, w.Path, "rev-parse", "HEAD")
			dirtySource(t, w)
			f.app.Stdin = strings.NewReader("  Y  \n")
			f.runner.hook = func(Process) error {
				if got := hostGit(t, w.Path, "status", "--porcelain=v1"); got != "" {
					t.Fatalf("hook saw dirty source %q", got)
				}
				if hostGit(t, w.Path, "rev-parse", "HEAD") != head {
					t.Fatal("source was automatically committed")
				}
				checkFile(t, filepath.Join(w.Path, ".env"), "ignored\n")
				return nil
			}
			args := []string{"merge", "topic"}
			if kind == "remove" {
				args[0] = "remove"
			}
			if err := f.run(t, args...); err != nil {
				t.Fatal(err)
			}
			if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal == nil || !state.Removal.BranchDeleted {
				t.Fatalf("cleanup incomplete: %+v", state)
			}
			if got := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
				t.Fatal("branch survived")
			}
			if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("worktree survived: %v", err)
			}
			if !strings.Contains(f.stdout.String(), "permanently discarded") {
				t.Fatal("missing permanent-discard warning")
			}
		})
	}
}

func TestRemoveUnmergedBranchNeedsNoCommitPrompt(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [check]\n")
	w := policyCommit(t, f)
	f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("clean unmerged branch prompted"); return 0, io.EOF })
	if err := f.run(t, "remove", "topic"); err != nil {
		t.Fatal(err)
	}
	if got := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
		t.Fatal("unmerged branch retained")
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatal("unmerged worktree retained")
	}
}

func TestDiscardRemovesFilesExposedByRestoredIgnoreRules(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [fail]\n")
	w := policyCommit(t, f)
	hostWrite(t, filepath.Join(w.Path, ".gitignore"), ".env\nignored/\nsecret\n")
	hostWrite(t, filepath.Join(w.Path, "secret"), "discarded with confirmation")
	hostWrite(t, filepath.Join(w.Path, "untracked"), "approved")
	hostWrite(t, filepath.Join(w.Path, ".env"), "still ignored")
	f.app.Stdin = strings.NewReader("y\n")
	hookRan := false
	f.runner.hook = func(Process) error { hookRan = true; return fmtPolicyError() }
	if err := f.run(t, "merge", "topic"); err == nil {
		t.Fatal("operation unexpectedly succeeded")
	}
	for _, name := range []string{"secret", "untracked"} {
		if _, err := os.Stat(filepath.Join(w.Path, name)); !os.IsNotExist(err) {
			t.Fatalf("confirmed discard retained %s: %v", name, err)
		}
	}
	checkFile(t, filepath.Join(w.Path, ".env"), "still ignored")
	if !hookRan {
		t.Fatal("pre_merge did not run after discard")
	}
}

func TestDirtyApprovalCleansReclassifiedFilesBeforeEveryOperation(t *testing.T) {
	for _, kind := range []string{"merge", "remove"} {
		for _, staged := range []bool{false, true} {
			name := kind + "/untracked"
			if staged {
				name = kind + "/staged"
			}
			t.Run(name, func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [check]\npre_remove: [check]\n")
				w := policyCommit(t, f)
				hostWrite(t, filepath.Join(w.Path, ".gitignore"), ".env\nignored/\nnotes.txt\n")
				hostWrite(t, filepath.Join(w.Path, "notes.txt"), "uncommitted notes")
				hostWrite(t, filepath.Join(w.Path, "new-file"), "untracked work")
				hostWrite(t, filepath.Join(w.Path, ".env"), "still ignored")
				if staged {
					hostGit(t, w.Path, "add", ".gitignore", "new-file")
					hostGit(t, w.Path, "add", "-f", "notes.txt")
				}
				f.app.Stdin = strings.NewReader("y\n")
				f.runner.hook = func(Process) error {
					if got := hostGit(t, w.Path, "status", "--porcelain=v1"); got != "" {
						t.Fatalf("hook saw uncommitted work: %q", got)
					}
					for _, file := range []string{"notes.txt", "new-file"} {
						if _, err := os.Stat(filepath.Join(w.Path, file)); !os.IsNotExist(err) {
							t.Fatalf("hook saw discarded file %s: %v", file, err)
						}
					}
					checkFile(t, filepath.Join(w.Path, ".env"), "still ignored")
					return nil
				}
				args := []string{"merge", "topic"}
				if kind == "remove" {
					args[0] = "remove"
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal(err)
				}
				if strings.Count(f.stdout.String(), "[y/N]") != 1 {
					t.Fatalf("expected one confirmation: %q", f.stdout.String())
				}
				if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
					t.Fatalf("worktree survived successful cleanup: %v", err)
				}
				if got := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
					t.Fatal("branch survived successful cleanup")
				}
			})
		}
	}
}

func TestDiscardRejectsFreshChangesToReclassifiedFiles(t *testing.T) {
	for _, phase := range []string{"confirmation", "restore"} {
		t.Run(phase, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [must-not-run]\n")
			w := policyCommit(t, f)
			hostWrite(t, filepath.Join(w.Path, ".gitignore"), ".env\nignored/\nnotes.txt\n")
			notes := filepath.Join(w.Path, "notes.txt")
			hostWrite(t, notes, "original notes")
			answer := strings.NewReader("y\n")
			if phase == "confirmation" {
				changed := false
				f.app.Stdin = policyReader(func(data []byte) (int, error) {
					if !changed {
						changed = true
						hostWrite(t, notes, "fresh work")
					}
					return answer.Read(data)
				})
			} else {
				f.app.Stdin = answer
				f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
					out, err := f.runner.Run(ctx, p)
					args := hostGitArgs(p)
					if p.Name == "git" && len(args) > 0 && args[0] == "restore" && err == nil {
						hostWrite(t, notes, "fresh work")
					}
					return out, err
				})
			}
			if err := f.run(t, "remove", "topic"); err == nil || !strings.Contains(err.Error(), "contents changed") {
				t.Fatalf("fresh changes were not refused: %v", err)
			}
			checkFile(t, notes, "fresh work")
			for _, event := range f.events {
				if event == "hook:must-not-run" {
					t.Fatal("fresh changes ran removal hook")
				}
			}
		})
	}
}

func fmtPolicyError() error { return errors.New("test hook failed") }

func TestDiscardRejectsChangedInputsDuringConfirmation(t *testing.T) {
	for _, change := range []string{"same tracked path", "new untracked", "same untracked", "index only"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [must-not-run]\n")
			w := policyCommit(t, f)
			dirtySource(t, w)
			answer := strings.NewReader("y\n")
			changed := false
			f.app.Stdin = policyReader(func(data []byte) (int, error) {
				if !changed {
					changed = true
					switch change {
					case "same tracked path":
						hostWrite(t, filepath.Join(w.Path, "tracked"), "new tracked work")
					case "new untracked":
						hostWrite(t, filepath.Join(w.Path, "new-file"), "new work")
					case "same untracked":
						hostWrite(t, filepath.Join(w.Path, "untracked"), "new untracked work")
					case "index only":
						hostGit(t, w.Path, "add", "tracked")
					}
				}
				return answer.Read(data)
			})
			if err := f.run(t, "remove", "topic"); err == nil || !strings.Contains(err.Error(), "contents changed") {
				t.Fatalf("changed inputs not refused: %v", err)
			}
			if w := f.load(t, "topic"); w.Removal != nil {
				t.Fatal("changed confirmation began cleanup")
			}
			for _, event := range f.events {
				if event == "hook:must-not-run" {
					t.Fatal("changed confirmation ran hook")
				}
			}
			if change == "same tracked path" {
				checkFile(t, filepath.Join(w.Path, "tracked"), "new tracked work")
			}
			if change == "new untracked" {
				checkFile(t, filepath.Join(w.Path, "new-file"), "new work")
			}
			if change == "same untracked" {
				checkFile(t, filepath.Join(w.Path, "untracked"), "new untracked work")
			}
		})
	}
}

func TestDiscardDetectsWritesAtRestoreCleanBoundary(t *testing.T) {
	for _, change := range []string{"new untracked", "same untracked", "tracked"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [must-not-run]\n")
			w := policyCommit(t, f)
			dirtySource(t, w)
			f.app.Stdin = strings.NewReader("y\n")
			f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
				out, err := f.runner.Run(ctx, p)
				args := hostGitArgs(p)
				if p.Name == "git" && len(args) > 0 && args[0] == "restore" && err == nil {
					name := "new-file"
					if change == "same untracked" {
						name = "untracked"
					}
					if change == "tracked" {
						name = "tracked"
					}
					hostWrite(t, filepath.Join(w.Path, name), "fresh boundary work")
				}
				return out, err
			})
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("fresh boundary edits ignored")
			}
			name := "new-file"
			if change == "same untracked" {
				name = "untracked"
			}
			if change == "tracked" {
				name = "tracked"
			}
			checkFile(t, filepath.Join(w.Path, name), "fresh boundary work")
			checkFile(t, filepath.Join(w.Path, "untracked"), map[bool]string{true: "fresh boundary work", false: "local\n"}[change == "same untracked"])
		})
	}
}

func TestDiscardCleanIsRestrictedToApprovedPaths(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	hostWrite(t, filepath.Join(w.Path, "untracked"), "approved")
	f.app.Stdin = strings.NewReader("y\n")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 0 && args[0] == "clean" {
			hostWrite(t, filepath.Join(w.Path, "new-file"), "late work")
		}
		return nil
	}
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("late file not detected")
	}
	checkFile(t, filepath.Join(w.Path, "new-file"), "late work")
}

func TestRemoveMultipleDirtyNamesPreservesInput(t *testing.T) {
	f := newHostFixture(t, "pre_remove: [hook]\n")
	for _, name := range []string{"one", "two"} {
		if err := f.run(t, "add", name); err != nil {
			t.Fatal(err)
		}
		hostWrite(t, filepath.Join(f.load(t, name).Path, "tracked"), "dirty")
	}
	f.app.Stdin = strings.NewReader("y\ny\nhook input\n")
	if err := f.run(t, "remove", "one", "two"); err != nil {
		t.Fatal(err)
	}
	if f.load(t, "one").Stage != "removed" || f.load(t, "two").Stage != "removed" {
		t.Fatal("second confirmation was consumed by first")
	}
	remaining, err := io.ReadAll(f.app.Stdin)
	if err != nil || string(remaining) != "hook input\n" {
		t.Fatalf("confirmation consumed other input: %q %v", remaining, err)
	}
}

func TestUnfinishedGitOperationsRefusedBeforeConfirmation(t *testing.T) {
	for _, target := range []bool{false, true} {
		for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_LOG", "index.lock"} {
			t.Run(name+map[bool]string{true: "/target", false: "/source"}[target], func(t *testing.T) {
				f := newHostFixture(t, "")
				w := policyCommit(t, f)
				hostWrite(t, filepath.Join(w.Path, "tracked"), "dirty")
				admin := filepath.Join(f.root, ".git")
				if !target {
					pointer, _ := os.ReadFile(filepath.Join(w.Path, ".git"))
					admin = strings.TrimSpace(strings.TrimPrefix(string(pointer), "gitdir: "))
				}
				hostWrite(t, filepath.Join(admin, name), "unfinished")
				f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("unfinished operation prompted"); return 0, io.EOF })
				if err := f.run(t, "merge", "topic"); err == nil {
					t.Fatal("unfinished Git operation accepted")
				}
				checkFile(t, filepath.Join(w.Path, "tracked"), "dirty")
			})
		}
	}
}

func TestLifecycleHooksCannotAuthorizeFreshDirtOrHead(t *testing.T) {
	for _, kind := range []string{"merge", "remove"} {
		for _, change := range []string{"tracked", "untracked", "head"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [change]\npre_remove: [change]\n")
				w := policyCommit(t, f)
				f.runner.hook = func(Process) error {
					hostWrite(t, filepath.Join(w.Path, change), "hook work")
					if change == "head" {
						hostGit(t, w.Path, "add", "head")
						hostGit(t, w.Path, "commit", "-m", "hook")
					}
					return nil
				}
				if err := f.run(t, kind, "topic"); err == nil {
					t.Fatal("fresh hook work deleted")
				}
				checkFile(t, filepath.Join(w.Path, change), "hook work")
				if got := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got == "" {
					t.Fatal("branch deleted after hook drift")
				}
			})
		}
	}
}

func TestMergeFailurePreservesFreshTargetWrites(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "failure", true: "cancelled"}[interrupted], func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == "merge" {
					hostWrite(t, filepath.Join(f.root, "tracked"), "fresh target work")
					hostWrite(t, filepath.Join(f.root, "new-file"), "fresh untracked work")
					if interrupted {
						cancel()
						return context.Canceled
					}
					return errors.New("injected Git failure")
				}
				return nil
			}
			command := Command{Kind: "merge", Name: "topic"}
			if err := f.app.Run(ctx, command); err == nil {
				t.Fatal("injected failure unexpectedly succeeded")
			}
			checkFile(t, filepath.Join(f.root, "tracked"), "fresh target work")
			checkFile(t, filepath.Join(f.root, "new-file"), "fresh untracked work")
			if state := f.load(t, "topic"); state.MergedCommit != "" || state.Removal != nil {
				t.Fatalf("failed integration began cleanup: %+v", state)
			}
			if hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead {
				t.Fatal("failed integration changed branch heads")
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal("source removed after failure")
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && (args[0] == "reset" || args[0] == "merge" && slices.Contains(args, "--abort")) {
					t.Fatalf("automatic abort/reset ran: %q", args)
				}
			}
			f.runner.git = nil
			f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("dirty target prompted source discard"); return 0, io.EOF })
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("fresh invocation ignored target edits")
			}
			checkFile(t, filepath.Join(f.root, "tracked"), "fresh target work")
			checkFile(t, filepath.Join(f.root, "new-file"), "fresh untracked work")
		})
	}
}

func TestTargetDirtyAndIgnoredCollisionsRemainProtected(t *testing.T) {
	for _, change := range []string{"tracked", "ignored"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [merge-check]\n")
			w := policyCommit(t, f)
			if change == "tracked" {
				hostWrite(t, filepath.Join(f.root, "tracked"), "target local work")
			} else {
				hostWrite(t, filepath.Join(w.Path, ".env"), "source tracked")
				hostGit(t, w.Path, "add", "-f", ".env")
				hostGit(t, w.Path, "commit", "-m", "source env")
				hostWrite(t, filepath.Join(f.root, ".env"), "target secret")
			}
			if change == "tracked" {
				dirtySource(t, w)
			}
			sourceStatus := hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all")
			f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("unsafe target prompted source discard"); return 0, io.EOF })
			f.events = nil
			before := hostGit(t, f.root, "rev-parse", "HEAD")
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("unsafe target accepted")
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != before {
				t.Fatal("target advanced")
			}
			if hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all") != sourceStatus {
				t.Fatal("unsafe target discarded source changes")
			}
			if change == "tracked" && slices.Contains(f.events, "hook:merge-check") {
				t.Fatal("dirty target ran pre_merge")
			}
			if change == "tracked" {
				checkFile(t, filepath.Join(f.root, "tracked"), "target local work")
			} else {
				checkFile(t, filepath.Join(f.root, ".env"), "target secret")
			}
		})
	}
}

func TestBranchDeleteIsAtomicAgainstAdvancedHead(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	advanced := hostGit(t, f.root, "rev-parse", "main")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 0 && args[0] == "update-ref" {
			hostGit(t, f.root, "update-ref", "refs/heads/topic", advanced)
		}
		return nil
	}
	if err := f.run(t, "remove", "topic"); err == nil {
		t.Fatal("changed branch was deleted")
	}
	if hostGit(t, f.root, "rev-parse", "topic") != advanced {
		t.Fatal("new branch HEAD lost")
	}
	if state := f.load(t, "topic"); state.Removal == nil || !state.Removal.WorktreeRemoved || state.Removal.BranchDeleted {
		t.Fatalf("bad recovery checkpoint: %+v", state)
	}
	if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
		t.Fatal("recorded worktree removal missing")
	}
}

func TestMultipleRemoveDeclineStopsLaterNames(t *testing.T) {
	for _, earlier := range []bool{false, true} {
		for _, answer := range []string{"N\n", "\n", ""} {
			t.Run(map[bool]string{false: "first", true: "after completed"}[earlier]+"/"+answer, func(t *testing.T) {
				f := newHostFixture(t, "pre_remove: [remove-hook]\n")
				names := []string{"dirty", "later"}
				if earlier {
					names = append([]string{"done"}, names...)
				}
				for _, name := range names {
					if err := f.run(t, "add", name); err != nil {
						t.Fatal(err)
					}
				}
				dirty := f.load(t, "dirty")
				hostWrite(t, filepath.Join(dirty.Path, "tracked"), "dirty work")
				journals := make(map[string][]byte)
				heads := make(map[string]string)
				for _, name := range []string{"dirty", "later"} {
					w := f.load(t, name)
					data, err := os.ReadFile(filepath.Join(f.state, w.RepoID, w.ID+".json"))
					if err != nil {
						t.Fatal(err)
					}
					journals[name], heads[name] = data, hostGit(t, f.root, "rev-parse", name)
				}
				var hooked, closed []string
				f.runner.hook = func(p Process) error {
					for _, entry := range p.Env {
						if name, ok := strings.CutPrefix(entry, "WM_BRANCH_NAME="); ok {
							hooked = append(hooked, name)
						}
					}
					return nil
				}
				f.mux.onClose = func(w Workspace) { closed = append(closed, w.Branch) }
				f.app.Stdin = strings.NewReader(answer)
				if err := f.run(t, append([]string{"remove"}, names...)...); err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"dirty", "later"} {
					w := f.load(t, name)
					after, err := os.ReadFile(filepath.Join(f.state, w.RepoID, w.ID+".json"))
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(journals[name], after) || hostGit(t, f.root, "rev-parse", name) != heads[name] {
						t.Fatalf("decline changed %s journal or branch", name)
					}
					if _, err := os.Stat(w.Path); err != nil {
						t.Fatalf("decline removed %s: %v", name, err)
					}
				}
				checkFile(t, filepath.Join(dirty.Path, "tracked"), "dirty work")
				if earlier {
					if f.load(t, "done").Stage != "removed" || !slices.Equal(hooked, []string{"done"}) || !slices.Equal(closed, []string{"done"}) {
						t.Fatalf("wrong completed lifecycle: hooks=%q closes=%q", hooked, closed)
					}
				} else if len(hooked) != 0 || len(closed) != 0 {
					t.Fatalf("decline ran later lifecycle: hooks=%q closes=%q", hooked, closed)
				}
			})
		}
	}
}

func TestIndexFlagsRefusedBeforeConfirmation(t *testing.T) {
	for _, flag := range []string{"assume-unchanged", "skip-worktree"} {
		for _, location := range []string{"merge source", "remove source", "merge target"} {
			for _, dirty := range []bool{false, true} {
				t.Run(flag+"/"+location+map[bool]string{false: "/clean", true: "/hidden dirt"}[dirty], func(t *testing.T) {
					f := newHostFixture(t, "pre_merge: [must-not-run]\npre_remove: [must-not-run]\n")
					w := policyCommit(t, f)
					path := w.Path
					if location == "merge target" {
						path = f.root
					}
					hostGit(t, path, "update-index", "--"+flag, "tracked")
					want := "committed source\n"
					if path == f.root {
						want = "initial\n"
					}
					if dirty {
						want = "hidden work"
						hostWrite(t, filepath.Join(path, "tracked"), want)
					}
					flags := hostGit(t, path, "ls-files", "-v")
					sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
					journalPath := filepath.Join(f.state, w.RepoID, w.ID+".json")
					before, err := os.ReadFile(journalPath)
					if err != nil {
						t.Fatal(err)
					}
					f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("flagged index prompted"); return 0, io.EOF })
					f.events = nil
					kind := "merge"
					if location == "remove source" {
						kind = "remove"
					}
					if err := f.run(t, kind, "topic"); err == nil || !strings.Contains(err.Error(), "clear the flag manually") {
						t.Fatalf("flagged index accepted: %v", err)
					}
					checkFile(t, filepath.Join(path, "tracked"), want)
					if hostGit(t, path, "ls-files", "-v") != flags || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead {
						t.Fatal("flag preflight changed index flags or branch heads")
					}
					after, err := os.ReadFile(journalPath)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) {
						t.Fatal("flag preflight changed journal")
					}
					for _, event := range f.events {
						if strings.HasPrefix(event, "hook:") || event == "mux:close" {
							t.Fatalf("flag preflight ran lifecycle: %q", f.events)
						}
					}
				})
			}
		}
	}
}

func TestLifecycleCannotHideFreshDirtWithIndexFlags(t *testing.T) {
	for _, flag := range []string{"assume-unchanged", "skip-worktree"} {
		for _, timing := range []string{"merge hook", "remove hook", "remove shutdown"} {
			t.Run(flag+"/"+timing, func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [change]\npre_remove: [change]\n")
				w := policyCommit(t, f)
				sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
				change := func() {
					hostGit(t, w.Path, "update-index", "--"+flag, "tracked")
					hostWrite(t, filepath.Join(w.Path, "tracked"), "hidden fresh work")
				}
				if timing == "remove shutdown" {
					f.mux.onClose = func(Workspace) { change() }
				} else {
					f.runner.hook = func(Process) error { change(); return nil }
				}
				kind := "remove"
				if timing == "merge hook" {
					kind = "merge"
				}
				if err := f.run(t, kind, "topic"); err == nil || !strings.Contains(err.Error(), "clear the flag manually") {
					t.Fatalf("hidden lifecycle dirt lost: %v", err)
				}
				checkFile(t, filepath.Join(w.Path, "tracked"), "hidden fresh work")
				if hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead {
					t.Fatal("hidden lifecycle dirt changed branch heads")
				}
				entries := hostGit(t, w.Path, "ls-files", "-v")
				if flag == "assume-unchanged" && !strings.Contains(entries, "h tracked") || flag == "skip-worktree" && !strings.Contains(entries, "S tracked") {
					t.Fatal("index flag was automatically cleared")
				}
				if state := f.load(t, "topic"); state.Stage == "removed" || state.Removal != nil && state.Removal.WorktreeRemoved {
					t.Fatal("hidden worktree marked removed")
				}
			})
		}
	}
}
