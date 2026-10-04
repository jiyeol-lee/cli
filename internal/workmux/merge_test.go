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

func TestMergeFastForwardAndAlreadyIntegratedCleanup(t *testing.T) {
	for _, history := range []string{"fast-forward", "identical", "source behind"} {
		t.Run(history, func(t *testing.T) {
			f := newHostFixture(t, "pre_remove: [check]\n")
			if err := f.run(t, "add", "topic"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			if history == "fast-forward" {
				hostWrite(t, filepath.Join(w.Path, "tracked"), "source\n")
				hostGit(t, w.Path, "commit", "-am", "source")
			} else if history == "source behind" {
				hostGit(t, f.root, "commit", "--allow-empty", "-m", "target ahead")
			}
			sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
			want := targetHead
			if history == "fast-forward" {
				want = sourceHead
			}
			f.app.Stdin = strings.NewReader("")
			if err := f.run(t, "merge", "topic"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(f.stdout.String(), "[y/N]") {
				t.Fatal("clean merge prompted")
			}
			if got := hostGit(t, f.root, "rev-parse", "HEAD"); got != want {
				t.Fatalf("target HEAD = %s, want %s", got, want)
			}
			state := f.load(t, "topic")
			if state.Stage != "removed" || state.MergedCommit != sourceHead || state.MergeTarget != "main" || state.Removal == nil || !state.Removal.BranchDeleted {
				t.Fatalf("integration did not complete cleanup: %+v", state)
			}
			if _, err := os.Stat(w.Path); !os.IsNotExist(err) {
				t.Fatalf("source worktree survived: %v", err)
			}
			if got := hostGit(t, f.root, "for-each-ref", "--format=%(refname)", "refs/heads/topic"); got != "" {
				t.Fatal("source branch survived")
			}
		})
	}
}

func TestMergeDivergenceRefusedBeforeDiscardHooksAndSwitch(t *testing.T) {
	for _, checkedOut := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchecked target", true: "checked target"}[checkedOut], func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [must-not-run]\npre_remove: [must-not-run]\n")
			hostWrite(t, filepath.Join(f.root, "target-only"), "target commit\n")
			hostGit(t, f.root, "add", "target-only")
			hostGit(t, f.root, "commit", "-m", "target")
			target := "main"
			if !checkedOut {
				hostGit(t, f.root, "branch", "release")
				target = "release"
			}
			if err := f.run(t, "add", "topic", "--base", "HEAD^"); err != nil {
				t.Fatal(err)
			}
			w := f.load(t, "topic")
			hostWrite(t, filepath.Join(w.Path, "tracked"), "source commit\n")
			hostGit(t, w.Path, "commit", "-am", "source")
			dirtySource(t, w)
			status, index := hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all"), hostGit(t, w.Path, "diff", "--cached", "--binary")
			sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", target)
			journalPath := filepath.Join(f.state, w.RepoID, w.ID+".json")
			journal, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			f.events, f.runner.processes = nil, nil
			f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("divergence prompted source discard"); return 0, io.EOF })
			if err := f.run(t, "merge", "topic", "--into", target); err == nil || !strings.Contains(strings.ToLower(err.Error()), "rebase") || !strings.Contains(strings.ToLower(err.Error()), "manual") {
				t.Fatalf("divergence did not request manual rebase: %v", err)
			}
			if hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", target) != targetHead || hostGit(t, f.root, "branch", "--show-current") != "main" {
				t.Fatal("divergence changed branch heads or checkout")
			}
			if hostGit(t, w.Path, "status", "--porcelain=v1", "--untracked-files=all") != status || hostGit(t, w.Path, "diff", "--cached", "--binary") != index {
				t.Fatal("divergence discarded source work")
			}
			after, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(journal, after) {
				t.Fatalf("divergence changed journal: %v", err)
			}
			for _, event := range f.events {
				if strings.HasPrefix(event, "hook:") || strings.HasPrefix(event, "git:") || event == "mux:capture" || event == "mux:close" {
					t.Fatalf("divergence ran lifecycle: %q", f.events)
				}
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && slices.Contains([]string{"switch", "restore", "clean", "reset", "rebase", "merge"}, args[0]) {
					t.Fatalf("divergence ran Git mutation: %q", args)
				}
			}
		})
	}
}

func TestMergeFreshInvocationSelectsTargetAndCurrentSourceAfterFailure(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		for _, selection := range []string{"default", "previous explicit", "different explicit"} {
			t.Run(map[bool]string{false: "failed", true: "cancelled"}[interrupted]+"/"+selection, func(t *testing.T) {
				f := newHostFixture(t, "pre_merge: [check]\n")
				w := policyCommit(t, f)
				initial := hostGit(t, f.root, "rev-parse", "main")
				releasePath := filepath.Join(t.TempDir(), "release")
				hostGit(t, f.root, "worktree", "add", "-b", "release", releasePath, "main")
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				f.runner.git = func(p Process) error {
					args := hostGitArgs(p)
					if len(args) > 0 && args[0] == "merge" {
						if interrupted {
							cancel()
							return context.Canceled
						}
						return errors.New("injected merge failure")
					}
					return nil
				}
				if err := f.app.Run(ctx, Command{Kind: "merge", Name: "topic", Into: "release"}); err == nil {
					t.Fatal("injected attempt succeeded")
				}
				if state := f.load(t, "topic"); state.Removal != nil || state.MergedCommit != "" {
					t.Fatal("failed attempt recorded successful integration")
				}
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "fresh source")
				freshHead := hostGit(t, w.Path, "rev-parse", "HEAD")
				moved := filepath.Join(t.TempDir(), "moved-release")
				hostGit(t, f.root, "worktree", "move", releasePath, moved)
				f.runner.git, f.runner.processes, f.events = nil, nil, nil
				args, target := []string{"merge", "topic"}, "main"
				if selection == "previous explicit" {
					args, target = append(args, "--into", "release"), "release"
				} else if selection == "different explicit" {
					args = append(args, "--into", "main")
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal("fresh invocation inherited failed attempt", err)
				}
				other := "release"
				if target == "release" {
					other = "main"
				}
				if hostGit(t, f.root, "rev-parse", target) != freshHead || hostGit(t, f.root, "rev-parse", other) != initial {
					t.Fatal("fresh invocation used stale source or wrong target")
				}
				if state := f.load(t, "topic"); state.Stage != "removed" || state.MergedCommit != freshHead || state.MergeTarget != target {
					t.Fatalf("fresh integration recorded stale identity: %+v", state)
				}
				if !slices.Contains(f.events, "hook:check") {
					t.Fatal("fresh invocation skipped pre_merge")
				}
			})
		}
	}
}

func TestMergeFastForwardOverridesRepositoryConfiguration(t *testing.T) {
	for _, test := range []struct{ key, value, target string }{
		{"merge.ff", "false", "main"},
		{"branch.main.mergeOptions", "--no-ff", "main"},
		{"branch.main.mergeOptions", "--squash", "main"},
		{"branch.main.mergeOptions", "--no-ff --squash", "main"},
		{"branch.main.mergeOptions", "-s ours", "main"},
		{"pull.twohead", "ours", "main"},
		{"branch.release=next.mergeOptions", "--squash", "release=next"},
		{"branch.release=next.mergeOptions", "-s ours", "release=next"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			if test.target != "main" {
				hostGit(t, f.root, "switch", "-c", test.target)
			}
			hostGit(t, f.root, "config", test.key, test.value)
			f.runner.processes = nil
			if err := f.run(t, "merge", "topic", "--into", test.target); err != nil {
				t.Fatal(err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "status", "--porcelain=v1") != "" {
				t.Fatal("repository config changed fast-forward integration")
			}
			found := false
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if p.Name == "git" && len(args) > 0 && args[0] == "merge" {
					found = true
					if !slices.Contains(args, "--ff-only") {
						t.Fatalf("merge omitted explicit fast-forward-only: %q", args)
					}
				}
			}
			if !found {
				t.Fatal("source was not integrated with Git merge")
			}
		})
	}
}

func TestMergeRechecksTargetAfterHooks(t *testing.T) {
	for _, change := range []string{"head", "tracked", "ignored", "path"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [change]\n")
			w := policyCommit(t, f)
			targetPath, target := f.root, "main"
			if change == "path" {
				targetPath, target = filepath.Join(t.TempDir(), "release"), "release"
				hostGit(t, f.root, "worktree", "add", "-b", target, targetPath)
			}
			if change == "ignored" {
				hostWrite(t, filepath.Join(w.Path, ".env"), "source tracked env")
				hostGit(t, w.Path, "add", "-f", ".env")
				hostGit(t, w.Path, "commit", "-m", "source env")
			}
			sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, targetPath, "rev-parse", "HEAD")
			f.runner.hook = func(Process) error {
				switch change {
				case "head":
					hostGit(t, targetPath, "commit", "--allow-empty", "-m", "hook target")
					targetHead = hostGit(t, targetPath, "rev-parse", "HEAD")
				case "tracked":
					hostWrite(t, filepath.Join(targetPath, "tracked"), "fresh target work")
				case "ignored":
					hostWrite(t, filepath.Join(targetPath, ".env"), "fresh target secret")
				case "path":
					moved := filepath.Join(t.TempDir(), "moved")
					hostGit(t, f.root, "worktree", "move", targetPath, moved)
					targetPath = moved
				}
				return nil
			}
			f.runner.processes = nil
			if err := f.run(t, "merge", "topic", "--into", target); err == nil {
				t.Fatal("target hook drift accepted")
			}
			if hostGit(t, targetPath, "rev-parse", "HEAD") != targetHead || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead {
				t.Fatal("hook drift was rolled back or source was integrated")
			}
			if change == "tracked" {
				checkFile(t, filepath.Join(targetPath, "tracked"), "fresh target work")
			} else if change == "ignored" {
				checkFile(t, filepath.Join(targetPath, ".env"), "fresh target secret")
			}
			if state := f.load(t, "topic"); state.Removal != nil || state.MergedCommit != "" {
				t.Fatal("target drift began cleanup")
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == "merge" {
					t.Fatal("target drift reached Git merge")
				}
			}
		})
	}
}

func TestMergeCleanupFailureRetainsIntegrationAndCanRetry(t *testing.T) {
	for _, phase := range []string{"hook", "shutdown"} {
		t.Run(phase, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [merge-check]\npre_remove: [remove-check]\n")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			if phase == "hook" {
				f.runner.hook = func(p Process) error {
					if p.Args[1] == "remove-check" {
						return errors.New("cleanup hook failed")
					}
					return nil
				}
			} else {
				f.mux.closeErr = errors.New("shutdown failed")
			}
			if err := f.run(t, "merge", "topic"); err == nil || !strings.Contains(err.Error(), "merge succeeded; cleanup incomplete") {
				t.Fatalf("cleanup failure lost integration result: %v", err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != sourceHead || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead {
				t.Fatal("cleanup failure rolled back integration")
			}
			if state := f.load(t, "topic"); state.MergedCommit != sourceHead || state.MergeTarget != "main" || state.Stage == "removed" {
				t.Fatalf("cleanup failure lost completed merge: %+v", state)
			}
			f.runner.hook, f.mux.closeErr, f.runner.processes = nil, nil, nil
			if err := f.run(t, "merge", "topic"); err != nil {
				t.Fatal("completed merge cleanup could not retry", err)
			}
			if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal == nil || !state.Removal.BranchDeleted {
				t.Fatalf("retry left cleanup unfinished: %+v", state)
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && args[0] == "merge" {
					t.Fatal("cleanup retry repeated integration")
				}
			}
		})
	}
}

func mergeWithReportedResult(t *testing.T, f *hostFixture, result string) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
		out, err := f.runner.Run(ctx, p)
		args := hostGitArgs(p)
		if p.Name == "git" && len(args) > 0 && args[0] == "merge" && err == nil {
			switch result {
			case "Git error":
				return out, errors.New("Git failed after updating target")
			case "cancelled":
				cancel()
				return out, context.Canceled
			case "dirty result":
				hostWrite(t, filepath.Join(f.root, "tracked"), "fresh post-integration work")
			}
		}
		return out, err
	})
	return f.app.Run(ctx, Command{Kind: "merge", Name: "topic"})
}

func TestMergeCommittedIntegrationSurvivesGitErrorAndCancellation(t *testing.T) {
	for _, result := range []string{"Git error", "cancelled", "dirty result"} {
		t.Run(result, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			if err := mergeWithReportedResult(t, f, result); err == nil || !strings.Contains(err.Error(), "merge succeeded; cleanup incomplete") {
				t.Fatalf("verified integration was not retained: %v", err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != sourceHead || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead {
				t.Fatal("Git error rolled back committed integration")
			}
			if state := f.load(t, "topic"); state.MergedCommit != sourceHead || state.Stage == "removed" {
				t.Fatalf("committed integration lost its checkpoint: %+v", state)
			}
			if result == "dirty result" {
				checkFile(t, filepath.Join(f.root, "tracked"), "fresh post-integration work")
				hostGit(t, f.root, "restore", "--", "tracked")
			}
			f.app.Runner, f.runner.processes = f.runner, nil
			if err := f.run(t, "merge", "topic"); err != nil {
				t.Fatal("committed integration cleanup did not retry", err)
			}
			if f.load(t, "topic").Stage != "removed" || hostGit(t, f.root, "rev-parse", "HEAD") != sourceHead {
				t.Fatal("cleanup retry lost integration or retained worktree")
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && (args[0] == "reset" || args[0] == "merge") {
					t.Fatalf("cleanup retry mutated integration: %q", args)
				}
			}
		})
	}
}

func TestMergeCleanupRetryRefusesSourceAndTargetDrift(t *testing.T) {
	for _, changed := range []string{"source", "target"} {
		t.Run(changed, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			integrated := hostGit(t, w.Path, "rev-parse", "HEAD")
			f.mux.closeErr = errors.New("shutdown failed")
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("cleanup failure was not reported")
			}
			checkpoint := f.load(t, "topic")
			if checkpoint.Removal == nil || checkpoint.Removal.Head != integrated || checkpoint.Removal.TargetOID != integrated {
				t.Fatalf("cleanup did not pin integration: %+v", checkpoint)
			}
			path := w.Path
			if changed == "target" {
				path = f.root
			}
			hostGit(t, path, "commit", "--allow-empty", "-m", "fresh "+changed+" work")
			sourceHead, targetHead := hostGit(t, w.Path, "rev-parse", "HEAD"), hostGit(t, f.root, "rev-parse", "HEAD")
			f.mux.closeErr, f.runner.processes = nil, nil
			if err := f.run(t, "merge", "topic"); err == nil {
				t.Fatal("cleanup retry ignored ref drift")
			}
			if hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "HEAD") != targetHead {
				t.Fatal("cleanup retry discarded committed drift")
			}
			if state := f.load(t, "topic"); state.Stage == "removed" || state.MergedCommit != integrated || state.Removal == nil || state.Removal.Head != integrated || state.Removal.TargetOID != integrated || state.Removal.WorktreeRemoved {
				t.Fatalf("cleanup retry replaced its safety checkpoint: %+v", state)
			}
			for _, p := range f.runner.processes {
				args := hostGitArgs(p)
				if len(args) > 0 && (args[0] == "merge" || args[0] == "reset" || args[0] == "update-ref") {
					t.Fatalf("cleanup retry mutated refs after drift: %q", args)
				}
			}
		})
	}
}

func TestMergeRecordedIntegrationRefusesRemovalOfAdvancedSource(t *testing.T) {
	for _, result := range []string{"Git error", "cancelled"} {
		t.Run(result, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			integrated := hostGit(t, w.Path, "rev-parse", "HEAD")
			if err := mergeWithReportedResult(t, f, result); err == nil {
				t.Fatal("Git failure was not reported")
			}
			if state := f.load(t, "topic"); state.MergedCommit != integrated || state.Removal != nil {
				t.Fatalf("integration was not recorded before cleanup: %+v", state)
			}
			hostWrite(t, filepath.Join(w.Path, "fresh"), "new committed source work")
			hostGit(t, w.Path, "add", "fresh")
			hostGit(t, w.Path, "commit", "-m", "fresh source")
			advanced := hostGit(t, w.Path, "rev-parse", "HEAD")
			f.app.Runner, f.runner.processes = f.runner, nil
			if err := f.run(t, "remove", "topic"); err == nil {
				t.Fatal("explicit remove deleted work committed after integration")
			}
			if hostGit(t, f.root, "rev-parse", "main") != integrated || hostGit(t, w.Path, "rev-parse", "HEAD") != advanced {
				t.Fatal("explicit remove changed committed source or target")
			}
			checkFile(t, filepath.Join(w.Path, "fresh"), "new committed source work")
			if state := f.load(t, "topic"); state.MergedCommit != integrated || state.Stage == "removed" || state.Removal != nil && state.Removal.WorktreeRemoved {
				t.Fatalf("explicit remove replaced integration safeguards: %+v", state)
			}
		})
	}
}

func TestMergeRecordedIntegrationRefusesRenamedSource(t *testing.T) {
	for _, test := range []struct{ command, result string }{
		{"merge", "Git error"},
		{"remove", "cancelled"},
	} {
		t.Run(test.command, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			integrated := hostGit(t, w.Path, "rev-parse", "HEAD")
			if err := mergeWithReportedResult(t, f, test.result); err == nil {
				t.Fatal("Git failure was not reported")
			}
			checkpoint := f.load(t, "topic")
			if checkpoint.MergedCommit != integrated || checkpoint.PendingMergeCommit != "" || checkpoint.Removal != nil || f.mux.window == "" {
				t.Fatalf("fixture did not retain recorded integration and source window: %+v", checkpoint)
			}
			window := f.mux.window
			hostGit(t, w.Path, "branch", "-m", "topic", "renamed")
			f.app.Runner = f.runner
			if err := f.run(t, test.command, "renamed"); err == nil {
				t.Fatal("renamed source bypassed recorded integration identity")
			}
			if hostGit(t, f.root, "rev-parse", "main") != integrated || hostGit(t, f.root, "rev-parse", "renamed") != integrated || hostGit(t, w.Path, "branch", "--show-current") != "renamed" {
				t.Fatal("source branch drift changed target integration or source checkout")
			}
			if info, err := os.Stat(w.Path); err != nil || !info.IsDir() || f.mux.window != window {
				t.Fatalf("source branch drift removed worktree or window: %v", err)
			}
			if state := f.load(t, "topic"); state.Branch != "topic" || state.ID != checkpoint.ID || state.MergedCommit != integrated || state.MergeTarget != "main" || state.Removal != nil {
				t.Fatalf("resolver rewrote recorded source identity: %+v", state)
			}
		})
	}
}

func TestMergeUnrecordedNativeIntegrationCannotBecomeFreshAttempt(t *testing.T) {
	for _, failure := range []string{"verification", "completion save"} {
		for _, changed := range []string{"source", "target selection"} {
			t.Run(failure+"/"+changed, func(t *testing.T) {
				f := newHostFixture(t, "")
				w := policyCommit(t, f)
				integrated := hostGit(t, w.Path, "rev-parse", "HEAD")
				initial := hostGit(t, f.root, "rev-parse", "main")
				hostGit(t, f.root, "branch", "release")
				journalPath := filepath.Join(f.state, w.RepoID, w.ID+".json")
				mutated := false
				f.app.Runner = cleanupRunFunc(func(ctx context.Context, p Process) ([]byte, error) {
					args := hostGitArgs(p)
					if mutated && failure == "verification" && p.Name == "git" && len(args) > 0 && args[0] == "merge-base" && slices.Contains(args, "--is-ancestor") {
						return nil, errors.New("post-integration ancestry verification failed")
					}
					out, err := f.runner.Run(ctx, p)
					if p.Name == "git" && len(args) > 0 && args[0] == "merge" && err == nil {
						mutated = true
						if failure == "completion save" {
							if err := os.Chmod(journalPath, 0644); err != nil {
								return out, err
							}
						}
					}
					return out, err
				})
				if err := f.run(t, "merge", "topic"); err == nil {
					t.Fatal("post-integration failure was not reported")
				}
				if failure == "completion save" {
					if err := os.Chmod(journalPath, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if !mutated || hostGit(t, f.root, "rev-parse", "main") != integrated {
					t.Fatal("fixture did not preserve successful native integration")
				}
				checkpoint := f.load(t, "topic")
				if checkpoint.Removal != nil || checkpoint.MergedCommit != integrated && checkpoint.PendingMergeCommit != integrated {
					t.Fatalf("post-integration failure lost source identity: %+v", checkpoint)
				}
				args := []string{"merge", "topic"}
				sourceHead := integrated
				if changed == "source" {
					hostWrite(t, filepath.Join(w.Path, "fresh"), "new source after integration")
					hostGit(t, w.Path, "add", "fresh")
					hostGit(t, w.Path, "commit", "-m", "new source")
					sourceHead = hostGit(t, w.Path, "rev-parse", "HEAD")
				} else {
					args = append(args, "--into", "release")
				}
				f.app.Runner, f.runner.processes = f.runner, nil
				if err := f.run(t, args...); err == nil {
					t.Fatal("successful native integration was forgotten on retry")
				}
				if hostGit(t, f.root, "rev-parse", "main") != integrated || hostGit(t, f.root, "rev-parse", "release") != initial || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead {
					t.Fatal("retry integrated a new source or target after uncertain completion")
				}
				if changed == "source" {
					checkFile(t, filepath.Join(w.Path, "fresh"), "new source after integration")
				}
				if state := f.load(t, "topic"); state.Stage == "removed" || state.Removal != nil && state.Removal.WorktreeRemoved {
					t.Fatal("retry removed source after uncertain completion")
				}
			})
		}
	}
}

func TestLegacyMergeCheckpointsLoadButFreshInvocationUsesCurrentInputs(t *testing.T) {
	for _, strategy := range []string{"merge", "rebase", "squash"} {
		t.Run(strategy, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [fresh-check]\n")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			hostGit(t, f.root, "branch", "release")
			w.Stage, w.MergeStrategy, w.MergeTarget = "merging", strategy, "release"
			w.PendingMergeCommit, w.PendingMergeHead = w.InitialCommit, w.InitialCommit
			w.PendingMergeTarget, w.RetryMergeTarget = "release", "release"
			w.PendingMergePath = filepath.Join(t.TempDir(), "previous-target")
			w.PendingConflict = true
			if strategy == "squash" {
				w.PendingSquashTree = hostGit(t, w.Path, "rev-parse", "HEAD^{tree}")
			}
			savePolicyState(t, f, w)
			loaded := f.load(t, "topic")
			if loaded.MergeStrategy != strategy || loaded.PendingMergeCommit != w.InitialCommit || loaded.PendingMergePath != w.PendingMergePath || !loaded.PendingConflict || loaded.PendingSquashTree != w.PendingSquashTree {
				t.Fatalf("legacy JSON checkpoint was not readable: %+v", loaded)
			}
			f.runner.processes = nil
			if err := f.run(t, "merge", "topic"); err != nil {
				t.Fatal("fresh invocation inherited legacy attempt", err)
			}
			if hostGit(t, f.root, "rev-parse", "main") != sourceHead || hostGit(t, f.root, "rev-parse", "release") != w.InitialCommit {
				t.Fatal("legacy checkpoint dictated source or target")
			}
			if state := f.load(t, "topic"); state.Stage != "removed" || state.PendingMergeCommit != "" || state.PendingSquashTree != "" || state.MergeTarget != "main" || state.MergedCommit != sourceHead {
				t.Fatalf("fresh completion retained unfinished legacy state: %+v", state)
			}
		})
	}
}

func TestLegacyUnfinishedGitRequiresManualResolutionBeforeFreshMerge(t *testing.T) {
	for _, leftover := range []string{"merge", "rebase", "staged squash"} {
		t.Run(leftover, func(t *testing.T) {
			f := newHostFixture(t, "pre_merge: [check]\n")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			hostGit(t, f.root, "branch", "release")
			w.Stage, w.PendingMergeCommit, w.PendingMergeHead = "merging", sourceHead, w.InitialCommit
			w.PendingMergeTarget, w.PendingMergePath, w.MergeStrategy = "main", f.root, "merge"
			marker := filepath.Join(f.root, ".git", "MERGE_HEAD")
			switch leftover {
			case "merge":
				w.PendingConflict = true
				hostWrite(t, marker, sourceHead+"\n")
			case "rebase":
				w.MergeStrategy = "rebase"
				marker = filepath.Join(hostGit(t, w.Path, "rev-parse", "--absolute-git-dir"), "REBASE_HEAD")
				hostWrite(t, marker, sourceHead+"\n")
			case "staged squash":
				w.MergeStrategy = "squash"
				hostGit(t, f.root, "merge", "--squash", "topic")
				w.PendingSquashTree = hostGit(t, f.root, "write-tree")
			}
			savePolicyState(t, f, w)
			index := hostGit(t, f.root, "diff", "--cached", "--binary")
			f.events, f.runner.processes = nil, nil
			f.app.Stdin = policyReader(func([]byte) (int, error) { t.Fatal("unfinished Git work prompted"); return 0, io.EOF })
			if err := f.run(t, "merge", "topic", "--into", "release"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "manual") {
				t.Fatalf("unfinished legacy work did not require manual resolution: %v", err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != w.InitialCommit || hostGit(t, w.Path, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "diff", "--cached", "--binary") != index {
				t.Fatal("unfinished work was resumed or discarded")
			}
			for _, event := range f.events {
				if strings.HasPrefix(event, "hook:") || strings.HasPrefix(event, "git:") || event == "mux:close" {
					t.Fatalf("unfinished work ran lifecycle: %q", f.events)
				}
			}
			if leftover == "staged squash" {
				hostGit(t, f.root, "restore", "--source=HEAD", "--staged", "--worktree", "--", "tracked")
			} else if err := os.Remove(marker); err != nil {
				t.Fatal(err)
			}
			f.app.Stdin = strings.NewReader("")
			if err := f.run(t, "merge", "topic"); err != nil {
				t.Fatal("manual resolution did not permit fresh merge", err)
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != sourceHead || hostGit(t, f.root, "rev-parse", "release") != w.InitialCommit || f.load(t, "topic").Stage != "removed" {
				t.Fatal("fresh merge inherited rejected explicit target")
			}
		})
	}
}

func TestLegacyCompletedSquashCleanupRequiresPinnedProofAndSource(t *testing.T) {
	for _, change := range []string{"none", "source advanced", "unrelated proof"} {
		t.Run(change, func(t *testing.T) {
			f := newHostFixture(t, "")
			w := policyCommit(t, f)
			sourceHead := hostGit(t, w.Path, "rev-parse", "HEAD")
			hostGit(t, f.root, "merge", "--squash", "topic")
			hostGit(t, f.root, "commit", "-m", "completed squash")
			result := hostGit(t, f.root, "rev-parse", "HEAD")
			w.Stage, w.MergeStrategy, w.MergeTarget = "merged", "squash", "main"
			w.MergedCommit, w.MergeResult = sourceHead, result
			if change == "source advanced" {
				hostGit(t, w.Path, "commit", "--allow-empty", "-m", "fresh source work")
			} else if change == "unrelated proof" {
				w.MergeResult = sourceHead
			}
			savePolicyState(t, f, w)
			err := f.run(t, "merge", "topic")
			if change == "none" {
				if err != nil {
					t.Fatal("verified legacy squash cleanup failed", err)
				}
				if state := f.load(t, "topic"); state.Stage != "removed" || state.Removal == nil || !state.Removal.BranchDeleted {
					t.Fatalf("legacy squash left cleanup unfinished: %+v", state)
				}
			} else {
				if err == nil {
					t.Fatal("changed source or invalid proof authorized cleanup")
				}
				if _, err := os.Stat(w.Path); err != nil {
					t.Fatal("unverified legacy squash source removed", err)
				}
				if state := f.load(t, "topic"); state.Stage == "removed" || state.Removal != nil {
					t.Fatal("unverified legacy squash started cleanup")
				}
			}
			if hostGit(t, f.root, "rev-parse", "HEAD") != result {
				t.Fatal("legacy squash cleanup changed target history")
			}
		})
	}
}
