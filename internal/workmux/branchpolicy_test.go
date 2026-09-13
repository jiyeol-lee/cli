package workmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func featureBase(t *testing.T, f *hostFixture) (string, string) {
	t.Helper()
	main := hostGit(t, f.root, "rev-parse", "main")
	hostGit(t, f.root, "switch", "-c", "feat/workmux")
	for _, text := range []string{"feature one\n", "feature two\n"} {
		hostWrite(t, filepath.Join(f.root, "tracked"), text)
		hostGit(t, f.root, "commit", "-am", text)
	}
	return main, hostGit(t, f.root, "rev-parse", "HEAD")
}

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
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "source work\n")
	hostGit(t, w.Path, "commit", "-am", "source work")
	f.stdout.Reset()
	f.events = nil
	return w
}

func policyEditor(t *testing.T, f *hostFixture, message string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "editor")
	marker := path + ".used"
	script := "#!/bin/bash\nprintf '%s\\n' " + shellQuote(message, "sh") + " > \"$1\"\nprintf x >> " + shellQuote(marker, "sh") + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	hostGit(t, f.root, "config", "core.editor", path)
	return marker
}

func TestMergeIgnoreUncommittedDoesNotCommitTheIndex(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%v", keep), func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			head := hostGit(t, w.Path, "rev-parse", "HEAD")
			hostGit(t, f.root, "config", "core.editor", "false")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "staged\n")
			hostGit(t, w.Path, "add", "tracked")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "unstaged\n")
			hostWrite(t, filepath.Join(w.Path, "local"), "untracked\n")
			args := []string{"merge", "topic", "--ignore-uncommitted"}
			if keep {
				args = append(args, "--keep")
			}
			if err := f.run(t, args...); err != nil {
				t.Fatal(err)
			}
			if got := hostGit(t, f.root, "show", "HEAD:tracked"); got != "source work" {
				t.Fatalf("merged index instead of HEAD: %q", got)
			}
			if state := f.load(t, "topic"); state.MergedCommit != head {
				t.Fatal("ignore-uncommitted created a source commit")
			}
			if keep {
				if got := hostGit(t, w.Path, "show", ":tracked"); got != "staged" {
					t.Fatalf("index changed: %q", got)
				}
				if data, err := os.ReadFile(filepath.Join(w.Path, "tracked")); err != nil || string(data) != "unstaged\n" {
					t.Fatalf("worktree changed: %q, %v", data, err)
				}
			} else if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("explicit ignore cleanup failed: %v", err)
			}
		})
	}
}

func TestMergeStrategiesKeepAndCleanup(t *testing.T) {
	for _, strategy := range []string{"rebase", "squash"} {
		for _, keep := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/keep=%v", strategy, keep), func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [verify]\npre_remove: [remove]\n")
				w := policyCommit(t, f)
				hostWrite(t, filepath.Join(w.Path, "second"), "source second\n")
				hostGit(t, w.Path, "add", "second")
				hostGit(t, w.Path, "commit", "-m", "second source commit")
				sourceBefore := hostGit(t, w.Path, "rev-parse", "HEAD")
				hostWrite(t, filepath.Join(f.root, "base-only"), "target\n")
				hostGit(t, f.root, "add", "base-only")
				hostGit(t, f.root, "commit", "-m", "target advances")
				targetBefore := hostGit(t, f.root, "rev-parse", "HEAD")
				marker := policyEditor(t, f, "squash through editor")
				hostWrite(t, filepath.Join(f.root, "unrelated-untracked"), "keep me")
				args := []string{"merge", "topic", "--" + strategy}
				if keep {
					args = append(args, "--keep")
				} else {
					args = append(args, "--cleanup")
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal(err)
				}
				state := f.load(t, "topic")
				if state.MergeStrategy != strategy || state.MergeResult != hostGit(t, f.root, "rev-parse", "HEAD") {
					t.Fatalf("missing strategy result: %+v", state)
				}
				if strategy == "squash" {
					if _, err := os.Stat(marker); err != nil {
						t.Fatal("squash bypassed editor", err)
					}
					if got := hostGit(t, f.root, "rev-list", "--count", targetBefore+"..HEAD"); got != "1" {
						t.Fatalf("squash produced %s commits", got)
					}
					if state.MergedCommit != sourceBefore {
						t.Fatal("squash rewrote source")
					}
				} else if state.MergedCommit == sourceBefore {
					t.Fatal("source was not rebased onto advanced target")
				}
				if got := hostGit(t, f.root, "show", "HEAD:tracked"); got != "source work" {
					t.Fatalf("lost source change: %s", got)
				}
				if _, err := os.Stat(filepath.Join(f.root, "unrelated-untracked")); err != nil {
					t.Fatal(err)
				}
				if keep {
					if _, err := os.Stat(w.Path); err != nil {
						t.Fatal(err)
					}
					if err := f.run(t, "remove", "topic"); err != nil {
						t.Fatal("kept strategy result cannot be cleaned up", err)
					}
				} else if state.Stage != "removed" {
					t.Fatalf("cleanup stuck: %+v", state)
				}
			})
		}
	}
}

func TestMergeNoVerifySkipsOnlyPreMerge(t *testing.T) {
	for _, flag := range []string{"", "--no-verify", "--no-hooks"} {
		t.Run(flag, func(t *testing.T) {
			f := newHostFixture(t, "post_create: [created]\npre_merge: [verify]\npre_remove: [remove]\n")
			w := policyCommit(t, f)
			created := false
			for _, p := range f.runner.processes {
				if p.Name == "bash" && p.Args[1] == "created" {
					created = true
					if p.Dir != w.Path {
						t.Fatal("post_create did not run on the host in the worktree")
					}
				}
			}
			if !created {
				t.Fatal("post_create was suppressed")
			}
			policyEditor(t, f, "staged source")
			hostWrite(t, filepath.Join(w.Path, "staged"), "staged\n")
			hostGit(t, w.Path, "add", "staged")
			marker := filepath.Join(t.TempDir(), "git-hook")
			hooks := t.TempDir()
			if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/bash\nprintf x >> "+shellQuote(marker, "sh")+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			hostGit(t, f.root, "config", "core.hooksPath", hooks)
			signer := filepath.Join(t.TempDir(), "signer")
			signed := signer + ".used"
			if err := os.WriteFile(signer, []byte("#!/bin/bash\nprintf x > "+shellQuote(signed, "sh")+"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			hostGit(t, f.root, "config", "commit.gpgSign", "true")
			hostGit(t, f.root, "config", "gpg.program", signer)
			args := []string{"merge", "topic"}
			if flag != "" {
				args = append(args, flag)
			}
			if err := f.run(t, args...); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{marker, signed} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("protected Git executed a hook or signer: %s, %v", path, err)
				}
			}
			if slices.Contains(f.events, "hook:verify") != (flag == "") || slices.Contains(f.events, "hook:remove") != (flag != "--no-hooks") {
				t.Fatalf("wrong lifecycle hook scope: %q", f.events)
			}
		})
	}
}

func TestProtectedInteractiveCommitHonorsAmbientEditor(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	marker := policyEditor(t, f, "ambient editor commit")
	t.Setenv("GIT_EDITOR", strings.TrimSuffix(marker, ".used"))
	hostGit(t, f.root, "config", "core.editor", "false")
	hostWrite(t, filepath.Join(w.Path, "staged"), "staged data\n")
	hostGit(t, w.Path, "add", "staged")
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal(err)
	}
	if got := hostGit(t, w.Path, "log", "-1", "--format=%s"); got != "ambient editor commit" {
		t.Fatalf("ambient editor was ignored: %q", got)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
		t.Fatalf("editor invocation = %q, %v", data, err)
	}
}

func TestIgnoreUncommittedCleanupKeepsForceAcrossCheckpoints(t *testing.T) {
	for _, squash := range []bool{false, true} {
		for _, mode := range []string{"direct", "retry", "worker"} {
			t.Run(fmt.Sprintf("squash=%v/%s", squash, mode), func(t *testing.T) {
				f := newHostFixture(t, "pre_remove: [generate]\n")
				w := policyCommit(t, f)
				head := hostGit(t, w.Path, "rev-parse", "HEAD")
				hostWrite(t, filepath.Join(w.Path, "tracked"), "staged data\n")
				hostGit(t, w.Path, "add", "tracked")
				hostWrite(t, filepath.Join(w.Path, "tracked"), "unstaged data\n")
				hostWrite(t, filepath.Join(w.Path, "untracked"), "untracked data\n")
				f.runner.hook = func(Process) error {
					hostWrite(t, filepath.Join(w.Path, "tracked"), "hook output\n")
					hostWrite(t, filepath.Join(w.Path, "generated"), "generated output\n")
					return nil
				}
				var launch CleanupLaunch
				if mode == "retry" {
					f.mux.closeErr = fmt.Errorf("defer cleanup")
				}
				if mode == "worker" {
					f.mux.caller = true
					f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, captured CleanupLaunch) error { launch = captured; return nil })
				}
				args := []string{"merge", "topic", "--ignore-uncommitted"}
				if squash {
					policyEditor(t, f, "squashed existing HEAD")
					args = append(args, "--squash")
				}
				err := f.run(t, args...)
				if (err != nil) != (mode == "retry") {
					t.Fatalf("merge error = %v", err)
				}
				state := f.load(t, "topic")
				if state.MergedCommit != head || state.Removal == nil || !state.Removal.Force || state.PendingSquashTree != "" || state.PendingMergeCommit != "" {
					t.Fatalf("cleanup lost its approved HEAD or force policy: %+v", state)
				}
				if got := hostGit(t, f.root, "show", "HEAD:tracked"); got != "source work" {
					t.Fatalf("merged pending source data: %q", got)
				}
				if mode == "retry" {
					f.mux.closeErr = nil
					if err := f.run(t, "remove", "topic"); err != nil {
						t.Fatal("saved force did not survive retry", err)
					}
				}
				if mode == "worker" {
					state.Removal.Job.Status = "queued"
					savePolicyState(t, f, state)
					if err := f.app.RunCleanup(t.Context(), launch.Command, cleanupReadyFunc(func(data []byte) (int, error) {
						state.Removal.Job.Status = "armed"
						savePolicyState(t, f, state)
						return len(data), nil
					})); err != nil {
						t.Fatal("worker rejected approved dirty source", err)
					}
				}
				if state := f.load(t, "topic"); state.Stage != "removed" || !state.Removal.BranchDeleted {
					t.Fatalf("cleanup stopped after strategy completion: %+v", state)
				}
				if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
					t.Fatalf("approved dirty worktree remains: %v", err)
				}
			})
		}
	}
}

func TestMergeStrategyConflictsAndRecovery(t *testing.T) {
	for _, strategy := range []string{"rebase", "squash"} {
		t.Run(strategy, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			hostWrite(t, filepath.Join(f.root, "tracked"), "target change\n")
			hostGit(t, f.root, "commit", "-am", "target conflict")
			target := hostGit(t, f.root, "rev-parse", "HEAD")
			hostWrite(t, filepath.Join(f.root, "untracked"), "preserve")
			policyEditor(t, f, "resolved squash")
			if err := f.run(t, "merge", "topic", "--"+strategy); err == nil {
				t.Fatal("expected conflict")
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal(err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != target {
				t.Fatal("target advanced on conflict")
			}
			if strategy == "rebase" {
				admin := hostGit(t, w.Path, "rev-parse", "--path-format=absolute", "--git-path", "rebase-merge")
				if _, err := os.Stat(admin); err != nil {
					t.Fatal("source rebase was automatically aborted", err)
				}
				hostGit(t, w.Path, "rebase", "--abort")
			} else {
				if got := hostGit(t, f.root, "diff", "HEAD", "--", "tracked"); got != "" {
					t.Fatalf("failed squash was not reset: %s", got)
				}
				if !slices.Contains(f.events, "git:merge --no-edit") {
					t.Fatal("squash did not invoke merge")
				}
			}
			if _, err := os.Stat(filepath.Join(f.root, "untracked")); err != nil {
				t.Fatal(err)
			}
			hostWrite(t, filepath.Join(w.Path, "tracked"), "target change\n")
			hostGit(t, w.Path, "commit", "-am", "resolve conflict")
			if err := f.run(t, "merge", "topic", "--keep"); err != nil {
				t.Fatal("clean retry froze the previous strategy", err)
			}
		})
	}
}

func TestSquashResetDoesNotDiscardConcurrentTargetWrites(t *testing.T) {
	for _, path := range []string{"tracked", "unrelated"} {
		t.Run(path, func(t *testing.T) {
			f := newHostFixture(t, "")
			hostWrite(t, filepath.Join(f.root, "unrelated"), "original\n")
			hostGit(t, f.root, "add", "unrelated")
			hostGit(t, f.root, "commit", "-m", "unrelated tracked data")
			w := policyCommit(t, f)
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == "merge" && slices.Contains(args, "--squash") {
					hostWrite(t, filepath.Join(f.root, path), "concurrent work\n")
					return fmt.Errorf("squash command interrupted before mutation")
				}
				return nil
			}
			if err := f.run(t, "merge", "topic", "--squash"); err == nil {
				t.Fatal("expected failed squash")
			}
			if data, err := os.ReadFile(filepath.Join(f.root, path)); err != nil || string(data) != "concurrent work\n" {
				t.Fatalf("reset discarded concurrent work: %q, %v", data, err)
			}
			if _, err := os.Stat(w.Path); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSquashCommitRetryAndRecordedResultRecovery(t *testing.T) {
	for _, failure := range []string{"editor", "lost commit result", "manual reset"} {
		t.Run(failure, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			marker := policyEditor(t, f, "one squash commit")
			if failure == "lost commit result" {
				f.runner.git = func(p Process) error {
					args := hostGitArgs(p)
					if p.Dir == f.root && len(args) > 0 && args[0] == "commit" {
						if _, err := (ExecRunner{}).Run(t.Context(), p); err != nil {
							return err
						}
						return fmt.Errorf("lost commit result")
					}
					return nil
				}
			} else {
				hostGit(t, f.root, "config", "core.editor", "false")
			}
			if err := f.run(t, "merge", "topic", "--squash"); err == nil {
				t.Fatal("expected interrupted squash commit")
			}
			if state := f.load(t, "topic"); state.PendingSquashTree == "" || state.Removal != nil {
				t.Fatalf("lost staged result: %+v", state)
			}
			f.runner.git = nil
			if failure == "manual reset" {
				hostGit(t, f.root, "reset", "--hard", "HEAD")
				if err := f.run(t, "merge", "topic", "--rebase"); err != nil {
					t.Fatal("manual reset did not release squash attempt", err)
				}
			} else {
				if failure == "editor" {
					marker = policyEditor(t, f, "one squash commit")
				}
				if err := f.run(t, "merge", "topic"); err != nil {
					t.Fatal("cannot resume squash", err)
				}
				if data, err := os.ReadFile(marker); err != nil || string(data) != "x" {
					t.Fatalf("editor ran more than once after commit: %q, %v", data, err)
				}
			}
			if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("resumed cleanup failed: %v", err)
			}
		})
	}
}

func TestKeptMergeAllowsFreshStrategyHooksAndDefaultTarget(t *testing.T) {
	f := newHostFixture(t, "pre_merge: [first]\n")
	w := policyCommit(t, f)
	policyEditor(t, f, "squash source")
	if err := f.run(t, "merge", "topic", "--squash", "--keep"); err != nil {
		t.Fatal(err)
	}
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "pre_merge: [fresh]\n")
	f.events = nil
	if err := f.run(t, "merge", "topic", "--rebase", "--keep"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.MergeStrategy != "rebase" || state.MergedCommit != hostGit(t, f.root, "rev-parse", "HEAD") {
		t.Fatalf("kept strategy was frozen: %+v", state)
	}
	if !slices.Contains(f.events, "hook:fresh") {
		t.Fatal("ordinary merge reused old hook completion")
	}
	hostGit(t, f.root, "branch", "release")
	hostWrite(t, filepath.Join(w.Path, "next"), "next change\n")
	hostGit(t, w.Path, "add", "next")
	hostGit(t, w.Path, "commit", "-m", "next change")
	if err := f.run(t, "merge", "topic", "--into", "release", "--keep"); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal(err)
	}
	if state := f.load(t, "topic"); state.MergeTarget != "main" {
		t.Fatalf("old --into overrode the saved base: %+v", state)
	}
}

func TestOwnedConflictManualAbortReleasesAttempt(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	hostWrite(t, filepath.Join(f.root, "tracked"), "target change\n")
	hostGit(t, f.root, "commit", "-am", "target conflict")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 1 && args[0] == "merge" && args[1] == "--abort" {
			return fmt.Errorf("injected abort failure")
		}
		return nil
	}
	if err := f.run(t, "merge", "topic"); err == nil {
		t.Fatal("expected conflict")
	}
	if state := f.load(t, "topic"); !state.PendingConflict {
		t.Fatal("owned conflict was not recorded")
	}
	hostGit(t, f.root, "merge", "--abort")
	hostWrite(t, filepath.Join(w.Path, "tracked"), "target change\n")
	hostGit(t, w.Path, "commit", "-am", "correct source")
	hostWrite(t, filepath.Join(f.config, "config.yaml"), "pre_merge: [fresh-after-abort]\n")
	f.runner.git = nil
	if err := f.run(t, "merge", "topic", "--keep"); err != nil {
		t.Fatal("manual abort left a permanent source lock", err)
	}
	if !slices.Contains(f.events, "hook:fresh-after-abort") {
		t.Fatal("completed recovery reused stale hooks")
	}
}

func TestSquashResetNeverRewindsAnAdvancedTarget(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	hostWrite(t, filepath.Join(f.root, "tracked"), "target change\n")
	hostGit(t, f.root, "commit", "-am", "target conflict")
	next := ""
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 1 && args[0] == "reset" && args[1] == "--hard" {
			old := hostGit(t, f.root, "rev-parse", "HEAD")
			tree := hostGit(t, f.root, "rev-parse", "HEAD^{tree}")
			next = hostGit(t, f.root, "commit-tree", tree, "-p", old, "-m", "concurrent target commit")
			hostGit(t, f.root, "update-ref", "refs/heads/main", next, old)
		}
		return nil
	}
	if err := f.run(t, "merge", "topic", "--squash"); err == nil {
		t.Fatal("expected conflict recovery failure")
	}
	if next == "" || hostGit(t, f.root, "rev-parse", "HEAD") != next {
		t.Fatal("squash reset rewound a concurrent target commit")
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatal("uncertain squash removed source", err)
	}
}

func TestSquashCleanupPinsTargetAndSourceWithoutAncestry(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	policyEditor(t, f, "squash source")
	source := hostGit(t, w.Path, "rev-parse", "HEAD")
	f.runner.git = func(p Process) error {
		args := hostGitArgs(p)
		if len(args) > 0 && args[0] == "update-ref" {
			hostGit(t, f.root, "commit", "--allow-empty", "-m", "target advanced before source deletion")
		}
		return nil
	}
	if err := f.run(t, "merge", "topic", "--squash"); err == nil {
		t.Fatal("source deletion ignored a changed approved target")
	}
	if got := hostGit(t, f.root, "rev-parse", "refs/heads/topic"); got != source {
		t.Fatal("uncertain cleanup deleted or rewrote the source ref")
	}
	state := f.load(t, "topic")
	if state.Removal == nil || state.Removal.MergeResult == "" || state.Removal.BranchDeleted {
		t.Fatalf("lost strategy cleanup proof: %+v", state)
	}
	f.runner.git = nil
	if err := f.run(t, "remove", "topic", "--keep-branch"); err != nil {
		t.Fatal("branch-preserving recovery failed", err)
	}
}

func TestBranchPolicyFeatureLifecycle(t *testing.T) {
	for _, operation := range []string{"remove", "merge unchanged", "merge commit"} {
		t.Run(operation, func(t *testing.T) {
			f := newHostFixture(t, "")
			main, feature := featureBase(t, f)
			config, err := os.ReadFile(filepath.Join(f.root, ".git", "config"))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.run(t, "add", "hello-world", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "hello-world")
			if w.BaseRef != "feat/workmux" || w.InitialCommit != feature {
				t.Fatalf("wrong base: %+v", w)
			}
			want := feature
			if operation == "merge commit" {
				hostWrite(t, filepath.Join(w.Path, "tracked"), "hello world\n")
				hostGit(t, w.Path, "commit", "-am", "hello world")
				want = hostGit(t, w.Path, "rev-parse", "HEAD")
			}
			f.stdout.Reset()
			command := "merge"
			if operation == "remove" {
				command = "remove"
			}
			if err := f.run(t, command, "hello-world"); err != nil {
				t.Fatal(err)
			}
			if hostGit(t, f.root, "rev-parse", "main") != main || hostGit(t, f.root, "rev-parse", "feat/workmux") != want {
				t.Fatal("wrong branch changed")
			}
			if state := f.load(t, "hello-world"); state.Stage != "removed" || state.Removal.DiscardCommits || strings.Contains(f.stdout.String(), "Are you sure") {
				t.Fatalf("unexpected cleanup: %+v, %s", state, f.stdout.String())
			}
			after, err := os.ReadFile(filepath.Join(f.root, ".git", "config"))
			if err != nil || !bytes.Equal(config, after) {
				t.Fatalf("base metadata changed protected Git config: %v", err)
			}
		})
	}
}

func TestBranchPolicyExplicitBases(t *testing.T) {
	for _, kind := range []string{"branch", "full branch", "tag", "full tag", "sha", "revision", "detached", "detached explicit"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			main, feature := featureBase(t, f)
			hostGit(t, f.root, "tag", "baseline", feature)
			base, saved, target := "feat/workmux", "feat/workmux", "feat/workmux"
			switch kind {
			case "full branch":
				base = "refs/heads/feat/workmux"
			case "tag", "full tag":
				base, saved, target = "baseline", "baseline", "main"
				if kind == "full tag" {
					base, saved = "refs/tags/baseline", "refs/tags/baseline"
				}
			case "sha":
				base, saved, target = feature, feature, "main"
			case "revision":
				base, saved, target = "feat/workmux~0", "feat/workmux~0", "main"
			case "detached", "detached explicit":
				hostGit(t, f.root, "switch", "--detach", feature)
				base, saved, target = "", "", "main"
				if kind == "detached explicit" {
					base, saved = feature, feature
				}
			}
			args := []string{"add", "topic", "-b"}
			if base != "" {
				args = append(args, "--base", base)
			}
			err := f.run(t, args...)
			if kind == "detached" {
				if err == nil || !strings.Contains(err.Error(), "detached HEAD requires --base") || slices.Contains(f.events, "git:worktree add") {
					t.Fatalf("detached add = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			if w.BaseRef != saved || w.InitialCommit != feature {
				t.Fatalf("wrong explicit base: %+v", w)
			}
			if err := f.run(t, "merge", "topic", "--keep"); err != nil {
				t.Fatal(err)
			}
			if w := f.load(t, "topic"); w.MergeTarget != target {
				t.Fatalf("merge selected %s, want %s", w.MergeTarget, target)
			}
			if target != "main" && hostGit(t, f.root, "rev-parse", "main") != main {
				t.Fatal("main changed")
			}
		})
	}
}

func TestBranchPolicyExistingAndLegacy(t *testing.T) {
	for _, kind := range []string{"unknown", "kept", "deleted recreated", "legacy", "missing base", "explicit override"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			_, feature := featureBase(t, f)
			if kind == "unknown" {
				hostGit(t, f.root, "branch", "topic", feature)
			}
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			if kind == "kept" || kind == "deleted recreated" {
				args := []string{"remove", "topic"}
				if kind == "kept" {
					args = append(args, "--keep-branch")
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal(err)
				}
				if kind == "deleted recreated" {
					hostGit(t, f.root, "branch", "topic", feature)
				}
				if err := f.run(t, "add", "topic", "-b"); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "legacy" {
				w := f.load(t, "topic")
				w.BaseRef = ""
				savePolicyState(t, f, w)
				f.stdout.Reset()
				if err := f.run(t, "remove", "topic"); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(f.stdout.String(), "Aborted") || f.load(t, "topic").Removal != nil {
					t.Fatal("legacy state inferred origin from matching tips")
				}
			}
			if kind == "missing base" {
				hostGit(t, f.root, "switch", "main")
				hostGit(t, f.root, "branch", "-D", "feat/workmux")
			}
			target := "main"
			args := []string{"merge", "topic", "--keep"}
			if kind == "legacy" {
				args = append(args, "--into", "feat/workmux")
				target = "feat/workmux"
			}
			if kind == "explicit override" {
				args = append(args, "--into", "main")
			}
			if kind == "kept" {
				target = "feat/workmux"
			}
			if (kind == "unknown" || kind == "deleted recreated") && f.load(t, "topic").BaseRef != "" {
				t.Fatal("invented origin of existing branch")
			}
			if err := f.run(t, args...); err != nil {
				t.Fatal(err)
			}
			if w := f.load(t, "topic"); w.MergeTarget != target {
				t.Fatalf("target = %s, want %s", w.MergeTarget, target)
			}
		})
	}
}

func TestBranchPolicyTargetPlacement(t *testing.T) {
	for _, kind := range []string{"switch", "elsewhere", "untracked", "dirty", "unfinished", "locked", "untracked collision", "ignored collision", "hook branch change", "switch source drift", "switch target drift"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [check]\n")
			main, _ := featureBase(t, f)
			if err := f.run(t, "add", "topic", "--base", "main", "-b"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			rootBranch := "feat/workmux"
			targetPath := f.root
			if kind == "elsewhere" || kind == "locked" {
				targetPath = filepath.Join(f.home, "target")
				hostGit(t, f.root, "worktree", "add", targetPath, "main")
				if kind == "locked" {
					hostGit(t, f.root, "worktree", "lock", targetPath)
				}
			}
			path := filepath.Join(f.root, "local")
			switch kind {
			case "untracked":
				hostWrite(t, path, "local data\n")
			case "dirty":
				path = filepath.Join(f.root, "tracked")
				hostWrite(t, path, "local data\n")
			case "unfinished":
				hostWrite(t, filepath.Join(f.root, ".git", "MERGE_HEAD"), main+"\n")
			case "ignored collision", "untracked collision":
				name := "local"
				if kind == "ignored collision" {
					name = ".env"
				}
				// Prepare target content in a temporary linked checkout, not the source.
				other := filepath.Join(f.home, "prepare")
				hostGit(t, f.root, "worktree", "add", other, "main")
				hostWrite(t, filepath.Join(other, name), "target data\n")
				hostGit(t, other, "add", "-f", name)
				hostGit(t, other, "commit", "-m", "target file")
				hostGit(t, f.root, "worktree", "remove", other)
				main = hostGit(t, f.root, "rev-parse", "main")
				path = filepath.Join(f.root, name)
				hostWrite(t, path, "local data\n")
			case "hook branch change":
				f.runner.hook = func(Process) error {
					hostGit(t, f.root, "switch", "feat/workmux")
					return nil
				}
			case "switch source drift", "switch target drift":
				f.runner.git = func(p Process) error {
					args := hostGitArgs(p)
					if len(args) > 0 && args[0] == "switch" {
						if kind == "switch source drift" {
							hostGit(t, w.Path, "commit", "--allow-empty", "-m", "new source")
						} else {
							hostGit(t, f.root, "update-ref", "refs/heads/main", hostGit(t, f.root, "rev-parse", "HEAD"))
						}
					}
					return nil
				}
			}
			err := f.run(t, "merge", "topic", "--keep")
			success := kind == "switch" || kind == "elsewhere" || kind == "untracked"
			if success && err != nil || !success && err == nil {
				t.Fatalf("merge = %v, success wanted %v", err, success)
			}
			if success && targetPath == f.root {
				rootBranch = "main"
			}
			if kind != "switch source drift" && kind != "switch target drift" && hostGit(t, f.root, "branch", "--show-current") != rootBranch {
				t.Fatal("unexpected main checkout branch")
			}
			if kind != "switch target drift" && hostGit(t, f.root, "rev-parse", "main") != main {
				t.Fatal("target ref changed unexpectedly")
			}
			if kind == "untracked" || kind == "dirty" || strings.HasSuffix(kind, "collision") {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != "local data\n" {
					t.Fatalf("lost local file: %q, %v", data, err)
				}
			}
			if !success && f.load(t, "topic").PendingMergeCommit != "" {
				t.Fatal("preflight or hook failure entered merge journal")
			}
		})
	}
}

func TestBranchPolicyConflictAbortAndRetry(t *testing.T) {
	for _, kind := range []string{"abort", "abort failure", "cancelled", "foreign merge", "changed head"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			hostWrite(t, filepath.Join(f.root, "tracked"), "target work\n")
			hostGit(t, f.root, "commit", "-am", "target work")
			target := hostGit(t, f.root, "rev-parse", "HEAD")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := f.app.Runner
			f.app.Runner = cleanupRunFunc(func(runCtx context.Context, p Process) ([]byte, error) {
				args := hostGitArgs(p)
				if p.Name == "git" && len(args) > 1 && args[0] == "merge" {
					if args[1] == "--abort" {
						if kind == "abort failure" {
							return nil, fmt.Errorf("injected abort failure")
						}
						if kind == "foreign merge" || kind == "changed head" {
							t.Fatal("attempted to abort unrelated merge state")
						}
					} else {
						out, err := base.Run(runCtx, p)
						switch kind {
						case "cancelled":
							cancel()
						case "foreign merge":
							hostWrite(t, filepath.Join(f.root, ".git", "MERGE_HEAD"), w.InitialCommit+"\n")
						case "changed head":
							hostGit(t, f.root, "update-ref", "refs/heads/main", w.InitialCommit, target)
						}
						return out, err
					}
				}
				return base.Run(runCtx, p)
			})
			if err := f.app.Run(ctx, Command{Kind: "merge", Name: "topic"}); err == nil {
				t.Fatal("conflict unexpectedly succeeded")
			}
			state := f.load(t, "topic")
			if kind == "abort failure" || kind == "foreign merge" || kind == "changed head" {
				if state.Stage != "merging" || state.PendingMergeHead != target || state.PendingMergePath != f.root || state.PendingMergeCommit == "" {
					t.Fatalf("lost pending merge: %+v", state)
				}
				return
			}
			if state.Stage != "ready" || state.PendingMergeCommit != "" || state.PendingMergeHead != "" || state.RetryMergeTarget != "main" {
				t.Fatalf("abort did not clear attempt: %+v", state)
			}
			if hostGit(t, f.root, "status", "--porcelain") != "" || hostGit(t, f.root, "rev-parse", "HEAD") != target {
				t.Fatal("abort did not restore clean target")
			}
			f.app.Runner = base
			hostWrite(t, filepath.Join(w.Path, "tracked"), "target work\n")
			hostGit(t, w.Path, "commit", "-am", "correct source")
			if err := f.run(t, "merge", "topic", "--keep"); err != nil {
				t.Fatal("retry with corrected source", err)
			}
			if err := f.run(t, "merge", "topic", "--keep"); err != nil {
				t.Fatal("repeat kept merge", err)
			}
		})
	}
}

type policyReader func([]byte) (int, error)

func (read policyReader) Read(data []byte) (int, error) { return read(data) }

type policyFlusher struct {
	bytes.Buffer
	err     error
	flushed bool
}

func (writer *policyFlusher) Flush() error {
	writer.flushed = true
	return writer.err
}

func TestBranchPolicyRemovalPrompt(t *testing.T) {
	for _, kind := range []string{"y", "Y", "y eof", "n", "yes", "blank", "eof", "nil", "input error", "output error", "abort output error", "flush error", "oversized", "dirty yes", "force", "keep", "head drift", "data drift"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			w := policyCommit(t, f)
			before := f.load(t, "topic")
			input := "y\n"
			switch kind {
			case "Y":
				input = "Y\n"
			case "y eof":
				input = "y"
			case "n":
				input = "n\n"
			case "yes":
				input = "yes\n"
			case "blank":
				input = "\n"
			case "eof":
				input = ""
			case "oversized":
				input = strings.Repeat("y", 4097)
			}
			f.app.Stdin = strings.NewReader(input)
			writer := &policyFlusher{}
			f.app.Stdout = writer
			failure := fmt.Errorf("injected confirmation failure")
			switch kind {
			case "nil":
				f.app.Stdin = nil
			case "input error":
				f.app.Stdin = policyReader(func([]byte) (int, error) { return 0, failure })
			case "output error", "abort output error":
				f.app.Stdin = nil
				f.app.Stdout = cleanupReadyFunc(func(data []byte) (int, error) {
					if kind == "output error" || strings.Contains(string(data), "Aborted") {
						return 0, failure
					}
					return len(data), nil
				})
			case "flush error":
				writer.err = failure
			case "dirty yes":
				hostWrite(t, filepath.Join(w.Path, "local"), "local work\n")
			case "head drift", "data drift":
				f.app.Stdin = policyReader(func(data []byte) (int, error) {
					if !writer.flushed {
						t.Fatal("read before prompt was flushed")
					}
					if kind == "head drift" {
						hostGit(t, w.Path, "commit", "--allow-empty", "-m", "new work")
					} else {
						hostWrite(t, filepath.Join(w.Path, "local"), "local work\n")
					}
					return copy(data, "y\n"), nil
				})
			}
			args := []string{"remove", "topic"}
			if kind == "force" {
				args = append(args, "--force")
			}
			if kind == "keep" {
				args = append(args, "--keep-branch")
			}
			err := f.run(t, args...)
			success := slices.Contains([]string{"y", "Y", "y eof", "force", "keep"}, kind)
			decline := slices.Contains([]string{"n", "yes", "blank", "eof", "nil"}, kind)
			if (success || decline) && err != nil || !success && !decline && err == nil {
				t.Fatalf("remove = %v", err)
			}
			if strings.HasSuffix(kind, "error") && !errors.Is(err, failure) {
				t.Fatalf("lost I/O error: %v", err)
			}
			state := f.load(t, "topic")
			if success {
				if state.Stage != "removed" || state.Removal.DiscardCommits != (kind != "force" && kind != "keep") {
					t.Fatalf("wrong authorization: %+v", state.Removal)
				}
				if kind != "force" && kind != "keep" && (!validOID(state.Removal.TargetOID) || !writer.flushed) {
					t.Fatal("consent was not pinned or flushed")
				}
			} else {
				if !reflect.DeepEqual(state, before) || slices.Contains(f.events, "hook:check") || slices.Contains(f.events, "mux:close") {
					t.Fatal("unsuccessful prompt changed lifecycle state/resources")
				}
				if _, err := os.Stat(w.Path); err != nil {
					t.Fatal(err)
				}
				if decline && !strings.Contains(writer.String(), "Aborted") {
					t.Fatal("missing cancellation message")
				}
			}
		})
	}
}

func TestBranchPolicyRemovalConsentRetries(t *testing.T) {
	for _, kind := range []string{"retry", "hook head", "shutdown head", "shutdown data", "target gone", "target drift", "worker", "worker head"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			w := policyCommit(t, f)
			f.app.Stdin = strings.NewReader("y\n")
			var launch CleanupLaunch
			if strings.HasPrefix(kind, "worker") {
				f.mux.caller = true
				f.app.Spawner = cleanupSpawnFunc(func(_ context.Context, captured CleanupLaunch) error { launch = captured; return nil })
			} else if strings.HasPrefix(kind, "shutdown") {
				f.mux.onClose = func(Workspace) {
					if kind == "shutdown head" {
						hostGit(t, w.Path, "commit", "--allow-empty", "-m", "shutdown commit")
					} else {
						hostWrite(t, filepath.Join(w.Path, "untracked-data"), "new data\n")
					}
				}
			} else {
				f.runner.hook = func(Process) error {
					if kind == "hook head" {
						hostGit(t, w.Path, "commit", "--allow-empty", "-m", "hook commit")
						return nil
					}
					return fmt.Errorf("retry hook")
				}
			}
			err := f.run(t, "remove", "topic")
			if strings.HasPrefix(kind, "worker") {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("expected interrupted cleanup")
			}
			state := f.load(t, "topic")
			if state.Removal == nil || !state.Removal.DiscardCommits || state.Removal.Force || state.Removal.Head == "" {
				t.Fatalf("consent not saved: %+v", state)
			}
			f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("retry/worker reprompted"); return 0, io.EOF })
			f.runner.hook = nil
			f.mux.onClose = nil
			switch kind {
			case "target gone":
				hostGit(t, f.root, "switch", "-c", "replacement")
				hostGit(t, f.root, "branch", "-D", "main")
			case "target drift":
				hostGit(t, f.root, "commit", "--allow-empty", "-m", "target advanced")
			case "worker head":
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "after approval")
			}
			if strings.HasPrefix(kind, "worker") {
				// Recreate only the queued handshake; the captured consent is unchanged.
				state.Removal.Job.Status = "queued"
				savePolicyState(t, f, state)
				err = f.app.RunCleanup(context.Background(), launch.Command, cleanupReadyFunc(func(data []byte) (int, error) {
					state.Removal.Job.Status = "armed"
					savePolicyState(t, f, state)
					return len(data), nil
				}))
			} else {
				err = f.run(t, "remove", "topic")
			}
			if kind == "retry" || kind == "worker" {
				if err != nil || f.load(t, "topic").Stage != "removed" {
					t.Fatalf("authorized retry failed: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("cleanup ignored changed approval inputs")
				}
				if _, err := os.Stat(w.Path); err != nil {
					t.Fatal("worktree lost", err)
				}
			}
		})
	}
}

func TestBranchPolicyRefErrorsAndValidation(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	store := hostStateStore(t, f)
	for _, ref := range []string{"-bad", "bad\nref", "bad\x00ref", "topic", "refs/heads/topic"} {
		state := w
		state.BaseRef = ref
		if err := store.validate(state); err == nil {
			t.Fatalf("accepted unsafe base %q", ref)
		}
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"for-each-ref", "merge-base"} {
		t.Run(operation, func(t *testing.T) {
			failure := fmt.Errorf("injected Git transport/ref failure")
			f.app.Stdin = strings.NewReader("y\n")
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == operation {
					return failure
				}
				return nil
			}
			if err := f.run(t, "remove", "topic"); !errors.Is(err, failure) {
				t.Fatalf("Git error became missing/unmerged: %v", err)
			}
			if f.load(t, "topic").Removal != nil || strings.Contains(f.stdout.String(), "Are you sure") {
				t.Fatal("Git error prompted or scheduled cleanup")
			}
		})
	}
}

func TestBranchPolicyPendingMergeStaysPinned(t *testing.T) {
	for _, change := range []string{"base", "explicit target", "source", "target", "checkout"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			hostGit(t, f.root, "branch", "release")
			f.runner.git = func(p Process) error {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == "merge" {
					return fmt.Errorf("interrupted before merge started")
				}
				return nil
			}
			if err := f.run(t, "merge", "topic", "--keep"); err == nil {
				t.Fatal("expected pending merge")
			}
			f.runner.git = nil
			state := f.load(t, "topic")
			state.BaseRef = "release"
			savePolicyState(t, f, state)
			args := []string{"merge", "topic", "--keep"}
			switch change {
			case "explicit target":
				args = append(args, "--into", "release")
			case "source":
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "advanced source")
			case "target":
				hostGit(t, f.root, "commit", "--allow-empty", "-m", "advanced target")
			case "checkout":
				hostGit(t, f.root, "switch", "release")
				hostGit(t, f.root, "worktree", "add", filepath.Join(f.home, "other"), "main")
			}
			err := f.run(t, args...)
			if change == "base" {
				if err != nil || f.load(t, "topic").MergeTarget != "main" {
					t.Fatalf("pending merge retargeted: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("pending merge accepted changed source/target")
				}
				if got := f.load(t, "topic"); got.PendingMergeCommit != state.PendingMergeCommit || got.PendingMergeTarget != "main" {
					t.Fatal("pending merge record changed")
				}
			}
		})
	}
}

func TestBranchPolicyRemovalComparisonRefs(t *testing.T) {
	for _, kind := range []string{"tag", "sha", "missing", "completed", "legacy retry"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			main, feature := featureBase(t, f)
			hostGit(t, f.root, "tag", "baseline", feature)
			base, expected := "refs/tags/baseline", "refs/tags/baseline"
			if kind == "sha" {
				base, expected = feature, feature
			}
			if kind == "missing" {
				base, expected = "main", "refs/heads/main"
			}
			if kind == "completed" {
				base, expected = "main", "refs/heads/feat/workmux"
			}
			if err := f.run(t, "add", "topic", "--base", base, "-b"); err != nil {
				t.Fatal(err)
			}
			state := f.load(t, "topic")
			if kind == "missing" {
				state.BaseRef = "deleted-base"
				savePolicyState(t, f, state)
			}
			if kind == "completed" {
				if err := f.run(t, "merge", "topic", "--into", "feat/workmux", "--keep"); err != nil {
					t.Fatal(err)
				}
			}
			f.stdout.Reset()
			f.runner.hook = func(Process) error { return fmt.Errorf("defer cleanup") }
			if err := f.run(t, "remove", "topic"); err == nil {
				t.Fatal("expected hook failure")
			}
			state = f.load(t, "topic")
			if state.Removal.Target != expected || state.Removal.DiscardCommits || strings.Contains(f.stdout.String(), "Are you sure") {
				t.Fatalf("wrong comparison: %+v, %s", state.Removal, f.stdout.String())
			}
			state.BaseRef = "other-missing-base"
			if kind == "legacy retry" {
				state.Removal.TargetOID = ""
				state.Removal.TargetRefOID = ""
			}
			savePolicyState(t, f, state)
			f.runner.hook = nil
			if err := f.run(t, "remove", "topic"); err != nil {
				t.Fatal("retry did not use captured comparison ref", err)
			}
			if state := f.load(t, "topic"); state.Stage != "removed" || !validOID(state.Removal.TargetOID) || state.Removal.Target != expected {
				t.Fatalf("lost comparison on retry: %+v", state.Removal)
			}
			if hostGit(t, f.root, "rev-parse", "main") != main {
				t.Fatal("removal changed main")
			}
		})
	}
}

func TestBranchPolicyAncestryFailureIsNotConsent(t *testing.T) {
	f := newHostFixture(t, "")
	w := policyCommit(t, f)
	g := gitHost{runner: f.runner}
	repo, err := g.discover(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := g.isAncestor(context.Background(), repo, w.InitialCommit, strings.Repeat("f", 40))
	if merged || err == nil || !gitExit(err, 128) {
		t.Fatalf("bad object was treated as unmerged: %v, %v", merged, err)
	}
}

func TestBranchPolicyExplicitForceRetry(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(fmt.Sprintf("advanced=%v", advanced), func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [build]\n")
			w := policyCommit(t, f)
			f.runner.hook = func(Process) error {
				hostWrite(t, filepath.Join(w.Path, "untracked-build-output"), "build output\n")
				return nil
			}
			f.mux.closeErr = fmt.Errorf("defer window closure")
			if err := f.run(t, "merge", "topic"); err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
				t.Fatalf("expected interrupted window cleanup: %v", err)
			}
			approved := f.load(t, "topic").Removal.Head
			if !f.load(t, "topic").Removal.Force {
				t.Fatal("successful merge did not authorize forced cleanup")
			}
			f.mux.closeErr = nil
			if advanced {
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "new source")
			}
			err := f.run(t, "remove", "topic", "--force")
			if advanced {
				if err == nil {
					t.Fatal("force adopted an advanced source")
				}
				if state := f.load(t, "topic"); !state.Removal.Force || state.Removal.Head != approved || state.Removal.WorktreeRemoved {
					t.Fatal("failed force retry changed authorization")
				}
				latest := hostGit(t, w.Path, "rev-parse", "HEAD")
				if err := f.run(t, "remove", "topic", "--force", "--keep-branch"); err != nil {
					t.Fatal("explicit keep-branch recovery failed", err)
				}
				if hostGit(t, f.root, "rev-parse", "refs/heads/topic") != latest {
					t.Fatal("keep-branch lost the new commit")
				}
			} else if err != nil || f.load(t, "topic").Stage != "removed" || !f.load(t, "topic").Removal.Force {
				t.Fatalf("explicit force did not recover unchanged source: %v", err)
			}
		})
	}
}

func TestBranchPolicyNativeIgnoredProtection(t *testing.T) {
	for _, operation := range []string{"merge", "switch"} {
		for _, file := range []string{".env", "ignored/localdata"} {
			t.Run(operation+"/"+file, func(t *testing.T) {
				f := newHostFixture(t, "")
				initial := hostGit(t, f.root, "rev-parse", "HEAD")
				if err := f.run(t, "add", "topic", "-b"); err != nil {
					t.Fatal(err)
				}
				w := f.load(t, "topic")
				tracking := w.Path
				if operation == "switch" {
					tracking = f.root
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(tracking, file)), 0700); err != nil {
					t.Fatal(err)
				}
				hostWrite(t, filepath.Join(tracking, file), "tracked target content\n")
				hostGit(t, tracking, "add", "-f", file)
				hostGit(t, tracking, "commit", "-m", "tracked ignored path")
				if operation == "switch" {
					hostGit(t, f.root, "switch", "-c", "side", initial)
				}
				injected := false
				f.runner.git = func(p Process) error {
					args := hostGitArgs(p)
					if len(args) > 0 && args[0] == operation {
						if !slices.Contains(args, "--no-overwrite-ignore") {
							t.Fatalf("mutation lacks native ignored-file protection: %q", args)
						}
						if err := os.MkdirAll(filepath.Dir(filepath.Join(f.root, file)), 0700); err != nil {
							t.Fatal(err)
						}
						hostWrite(t, filepath.Join(f.root, file), "late local data\n")
						injected = true
					}
					return nil
				}
				err := f.run(t, "merge", "topic", "--keep")
				if err == nil || !injected || !strings.Contains(err.Error(), file) || strings.Contains(err.Error(), "unknown option") {
					t.Fatalf("mutation did not refuse the late collision: %v", err)
				}
				data, err := os.ReadFile(filepath.Join(f.root, file))
				if err != nil || string(data) != "late local data\n" || hostGit(t, f.root, "rev-parse", "HEAD") != initial {
					t.Fatalf("late ignored data or target HEAD changed: %q, %v", data, err)
				}
				if _, err := os.Stat(filepath.Join(f.root, ".git", "MERGE_HEAD")); !os.IsNotExist(err) || slices.Contains(f.events, "git:merge --abort") {
					t.Fatal("file collision caused an unrelated abort")
				}
				if _, err := os.Stat(w.Path); err != nil {
					t.Fatal("source was removed", err)
				}
			})
		}
	}
}

func TestBranchPolicySwitchSupportsIgnoredProtection(t *testing.T) {
	f := newHostFixture(t, "")
	g := gitHost{runner: f.runner}
	out, err := g.run(context.Background(), f.root, "switch", "-h")
	if err != nil && !gitExit(err, 129) {
		t.Fatal(err)
	}
	t.Logf("%s; switch help lists overwrite-ignore=%v", hostGit(t, f.root, "--version"), bytes.Contains(out, []byte("overwrite-ignore")))
	hostGit(t, f.root, "branch", "release")
	if _, err := g.run(context.Background(), f.root, "switch", "--no-guess", "--no-overwrite-ignore", "--", "release"); err != nil {
		t.Fatal("installed Git does not support protected switching", err)
	}
}

func TestBranchPolicyAtomicComparisonAndDelete(t *testing.T) {
	for _, comparison := range []string{"branch", "tag", "annotated tag", "commit"} {
		for _, race := range []string{"none", "rewind", "delete", "replace same commit"} {
			t.Run(comparison+"/"+race, func(t *testing.T) {
				f := newHostFixture(t, "")
				initial, source := featureBase(t, f)
				hostGit(t, f.root, "branch", "release", source)
				hostGit(t, f.root, "tag", "release", initial)
				base, ref := "refs/heads/release", "refs/heads/release"
				if comparison == "tag" || comparison == "annotated tag" {
					base, ref = "refs/tags/release", "refs/tags/release"
					hostGit(t, f.root, "update-ref", "refs/heads/release", initial)
					hostGit(t, f.root, "tag", "-f", "release", source)
					if comparison == "annotated tag" {
						hostGit(t, f.root, "tag", "-f", "-a", "release", "-m", "original tag", source)
					}
				}
				if comparison == "commit" {
					base = source
				}
				raw := hostGit(t, f.root, "rev-parse", ref)
				if err := f.run(t, "add", "topic", "--base", base, "-b"); err != nil {
					t.Fatal(err)
				}
				called := false
				f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
					args := hostGitArgs(p)
					if p.Name == "git" && len(args) > 0 && args[0] == "update-ref" {
						called = true
						input, err := io.ReadAll(p.Stdin)
						if err != nil {
							t.Fatal(err)
						}
						if !slices.Equal(args, []string{"update-ref", "--no-deref", "--stdin", "-z"}) || !bytes.Contains(input, []byte("delete refs/heads/topic\x00"+source+"\x00")) {
							t.Fatalf("unsafe source deletion: %q, %q", args, input)
						}
						if comparison == "commit" {
							if bytes.Contains(input, []byte("verify ")) {
								t.Fatal("immutable commit comparison verified an unrelated ref")
							}
						} else if !bytes.Contains(input, []byte("verify "+ref+"\x00"+raw+"\x00")) {
							t.Fatalf("target verification missing or used peeled tag OID: %q", input)
						}
						switch race {
						case "rewind":
							hostGit(t, f.root, "update-ref", ref, initial, raw)
						case "delete":
							hostGit(t, f.root, "update-ref", "--no-deref", "-d", ref, raw)
						case "replace same commit":
							if comparison == "annotated tag" {
								hostGit(t, f.root, "tag", "-f", "-a", "release", "-m", "replacement tag", source)
							}
						}
						p.Stdin = bytes.NewReader(input)
					}
					return f.runner.Run(ctx, p)
				})
				err := f.run(t, "remove", "topic")
				if !called {
					t.Fatalf("did not reach final transaction: %v", err)
				}
				fail := comparison != "commit" && (race == "rewind" || race == "delete" || comparison == "annotated tag" && race == "replace same commit")
				if fail {
					if err == nil || hostGit(t, f.root, "rev-parse", "refs/heads/topic") != source {
						t.Fatalf("target race deleted source ref: %v", err)
					}
				} else if err != nil || f.load(t, "topic").Stage != "removed" {
					t.Fatalf("safe deletion failed: %v", err)
				}
			})
		}
	}
}

func TestBranchPolicyLegacyBareRemovalTarget(t *testing.T) {
	for _, kind := range []string{"branch merged", "branch unmerged", "branch missing", "invalid branch"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			initial, feature := featureBase(t, f)
			hostGit(t, f.root, "branch", "release", feature)
			hostGit(t, f.root, "tag", "release", initial)
			if err := f.run(t, "add", "topic", "-b"); err != nil {
				t.Fatal(err)
			}
			f.runner.hook = func(Process) error { return fmt.Errorf("defer cleanup") }
			if err := f.run(t, "remove", "topic"); err == nil {
				t.Fatal("expected pending cleanup")
			}
			state := f.load(t, "topic")
			state.BaseRef = ""
			state.Removal.Target, state.Removal.TargetOID, state.Removal.TargetRefOID = "release", "", ""
			if kind == "invalid branch" {
				state.Removal.Target = "release~0"
			}
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("target_oid")) || bytes.Contains(data, []byte("target_ref_oid")) {
				t.Fatal("fixture is not legacy JSON")
			}
			hostWrite(t, filepath.Join(f.state, state.RepoID, state.ID+".json"), string(data))
			if loaded := f.load(t, "topic"); loaded.Removal.Target != state.Removal.Target || loaded.Removal.TargetOID != "" {
				t.Fatal("legacy JSON did not load")
			}
			if kind == "branch unmerged" || kind == "branch missing" {
				hostGit(t, f.root, "tag", "-f", "release", feature)
				hostGit(t, f.root, "update-ref", "refs/heads/release", initial)
				if kind == "branch missing" {
					hostGit(t, f.root, "update-ref", "-d", "refs/heads/release", initial)
				}
			}
			f.runner.hook = nil
			f.stdout.Reset()
			err = f.run(t, "remove", "topic")
			if kind == "branch merged" {
				if err != nil {
					t.Fatal("legacy target used same-name tag instead of branch", err)
				}
				if state := f.load(t, "topic"); state.Removal.Target != "refs/heads/release" || state.Removal.TargetOID != feature || state.Removal.TargetRefOID != feature {
					t.Fatalf("legacy target not pinned to exact branch: %+v", state.Removal)
				}
			} else if err == nil || hostGit(t, f.root, "rev-parse", "refs/heads/topic") != feature {
				t.Fatalf("legacy target silently used a tag or revision: %v", err)
			}
			if strings.Contains(f.stdout.String(), "Are you sure") {
				t.Fatal("legacy retry reprompted")
			}
		})
	}
}
