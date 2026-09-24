package workmux

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func orphanFixture(t *testing.T, sandbox, stale bool) (*hostFixture, workspaceState) {
	t.Helper()
	config := "pre_remove: [must-not-run]\n"
	if sandbox {
		config += "sandbox: {enabled: true}\n"
	}
	f := newHostFixture(t, config)
	if err := f.run(t, "add", "hello-world", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "hello-world")
	if sandbox {
		// Exercise migration/recovery of the old persistent-container records.
		w.Container = "cli-workmux-" + w.ID
		f.sandbox.present = true
		store := hostStateStore(t, f)
		if err := store.save(w); err != nil {
			t.Fatal(err)
		}
		if err := store.unlock(); err != nil {
			t.Fatal(err)
		}
	}
	if stale {
		if err := os.RemoveAll(w.Path); err != nil {
			t.Fatal(err)
		}
	} else {
		hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	}
	f.events = nil
	f.stdout.Reset()
	return f, w
}

func checkOrphanRetired(t *testing.T, state workspaceState) {
	t.Helper()
	if state.Stage != "removed" || state.Removal == nil || state.Removal.Recovery == nil || !state.Removal.KeepBranch || state.Removal.BranchDeleted || state.Removal.Force || state.Removal.Job != nil {
		t.Fatalf("not retired as branch-preserving orphan: %+v", state)
	}
	if state.BaseRef != "" || state.MergedCommit != "" || state.MergeTarget != "" || state.InitialCommit != "" || state.CreatedBranch || state.PendingMergeCommit != "" || state.PendingMergeTarget != "" || state.PendingMergeHead != "" || state.PendingMergePath != "" || state.RetryMergeTarget != "" || state.MergeKept || state.Config.Sandbox.Enabled || state.Container != "" || state.Window != "" || state.Socket != "" || state.ServerPID != 0 || state.SocketInode != 0 || state.SocketDevice != 0 || state.OwnedIgnored != nil {
		t.Fatalf("orphan retained stale creation/merge/runtime history: %+v", state)
	}
}

func checkNoOrphanGitDeletion(t *testing.T, f *hostFixture) {
	t.Helper()
	for _, event := range f.events {
		if strings.HasPrefix(event, "hook:") || strings.HasPrefix(event, "sandbox:hook:") || event == "sandbox:ensure" || strings.HasPrefix(event, "git:") {
			t.Fatalf("orphan recovery ran a hook, ensured a container, or mutated Git: %v", f.events)
		}
	}
}

func TestOrphanReadyRemovePreservesSurvivingRefsAndCanAddAgain(t *testing.T) {
	for _, branch := range []string{"absent", "advanced", "replaced"} {
		for _, flag := range []string{"", "--keep-branch", "--force"} {
			t.Run(branch+flag, func(t *testing.T) {
				f, w := orphanFixture(t, false, false)
				if branch == "absent" || branch == "replaced" {
					hostGit(t, f.root, "branch", "-D", w.Branch)
				}
				oid := ""
				if branch != "absent" {
					hostGit(t, f.root, "commit", "--allow-empty", "-m", "new unrelated branch history")
					oid = hostGit(t, f.root, "rev-parse", "HEAD")
					hostGit(t, f.root, "update-ref", "refs/heads/"+w.Branch, oid)
				}
				store := hostStateStore(t, f)
				w.MergedCommit, w.MergeTarget, w.MergeKept = w.InitialCommit, "main", true
				w.ServerPID, w.SocketDevice, w.SocketInode = 999999, 1, 2
				if err := store.save(w); err != nil {
					t.Fatal(err)
				}
				if err := store.unlock(); err != nil {
					t.Fatal(err)
				}
				args := []string{"remove", w.Branch}
				if flag != "" {
					args = append(args, flag)
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal(err)
				}
				checkNoOrphanGitDeletion(t, f)
				checkOrphanRetired(t, f.load(t, w.Branch))
				if !strings.Contains(f.stdout.String(), "skipping pre_remove") || !strings.Contains(f.stdout.String(), "branch is preserved") {
					t.Fatalf("missing orphan notice: %s", f.stdout.String())
				}
				if err := f.run(t, args...); err != nil {
					t.Fatal("repeat remove", err)
				}
				if branch != "absent" && hostGit(t, f.root, "rev-parse", w.Branch) != oid {
					t.Fatal("surviving ref changed")
				}
				if err := f.run(t, "merge", w.Branch); err == nil || !strings.Contains(err.Error(), "without a recorded merge") {
					t.Fatalf("orphan claimed old merge completion: %v", err)
				}
				if err := f.run(t, "add", w.Branch, "-b"); err != nil {
					t.Fatal("orphan prevented fresh add", err)
				}
				fresh := f.load(t, w.Branch)
				if fresh.Removal != nil || fresh.ServerPID != 0 || fresh.MergedCommit != "" || branch != "absent" && fresh.BaseRef != "" || branch == "absent" && fresh.BaseRef != "main" {
					t.Fatalf("fresh add inherited orphan history: %+v", fresh)
				}
			})
		}
	}
}

func TestAddRetiresOnlyMetadataOrphan(t *testing.T) {
	for _, branch := range []string{"absent", "present", "slug collision"} {
		t.Run(branch, func(t *testing.T) {
			f, w := orphanFixture(t, false, false)
			f.mux.window = ""
			name := w.Branch
			if branch == "absent" {
				hostGit(t, f.root, "branch", "-D", w.Branch)
			}
			if branch == "slug collision" {
				name = "hello/world"
			}
			if err := f.run(t, "add", name, "-b"); err != nil {
				t.Fatal(err)
			}
			fresh := f.load(t, name)
			if fresh.Stage != "ready" || fresh.Branch != name || fresh.Removal != nil || branch == "present" && fresh.BaseRef != "" || branch != "present" && fresh.BaseRef != "main" {
				t.Fatalf("not a fresh add: %+v", fresh)
			}
			for _, event := range f.events {
				if event == "mux:close" || event == "sandbox:exists" || event == "sandbox:stop" || event == "sandbox:remove" || event == "hook:must-not-run" {
					t.Fatalf("add modified orphan resources: %v", f.events)
				}
			}
		})
	}
}

func TestAddOrphanClearsHistoryBeforeFailedFreshSetup(t *testing.T) {
	f, w := orphanFixture(t, false, false)
	f.mux.window = ""
	f.mux.sessionErr = fmt.Errorf("not inside a session")
	store := hostStateStore(t, f)
	w.PendingMergeCommit, w.PendingMergeHead = w.InitialCommit, w.InitialCommit
	w.PendingMergeTarget, w.PendingMergePath, w.RetryMergeTarget = "main", f.root, "main"
	if err := store.save(w); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	if err := f.run(t, "add", w.Branch, "-b"); err == nil {
		t.Fatal("expected session failure")
	}
	checkOrphanRetired(t, f.load(t, w.Branch))
	f.mux.sessionErr = nil
	if err := f.run(t, "add", "hello/world", "-b"); err != nil {
		t.Fatal("retired orphan slug blocked a different branch", err)
	}
}

func TestOrphanAddRefusesResourcesAndRemoveRetriesFailures(t *testing.T) {
	for _, resource := range []string{"window", "live container", "stopped container"} {
		t.Run(resource, func(t *testing.T) {
			f, w := orphanFixture(t, resource != "window", false)
			if resource != "window" {
				f.mux.window = ""
			}
			if resource == "stopped container" {
				if err := f.sandbox.Stop(context.Background(), w.Workspace); err != nil {
					t.Fatal(err)
				}
			}
			f.events = nil
			if err := f.run(t, "add", w.Branch, "-b"); err == nil || !strings.Contains(err.Error(), "workspace files missing but managed resources remain; run cli workmux remove hello-world") {
				t.Fatalf("add did not direct explicit cleanup: %v", err)
			}
			if !reflect.DeepEqual(w, f.load(t, w.Branch)) || slices.Contains(f.events, "mux:close") || slices.Contains(f.events, "sandbox:stop") || slices.Contains(f.events, "sandbox:remove") {
				t.Fatal("add changed the orphan or its resources")
			}
			if resource == "window" {
				f.mux.closeErr = fmt.Errorf("close failed")
			} else {
				f.sandbox.rmErr = fmt.Errorf("remove failed")
			}
			if err := f.run(t, "remove", w.Branch, "--force"); err == nil {
				t.Fatal("expected runtime phase failure")
			}
			failed := f.load(t, w.Branch)
			if failed.Stage != "removing" || failed.Removal.Recovery == nil || failed.Removal.Job.Status != "failed" {
				t.Fatalf("failure lost orphan recovery: %+v", failed)
			}
			f.mux.closeErr, f.sandbox.rmErr = nil, nil
			if err := f.run(t, "remove", w.Branch); err != nil {
				t.Fatal("partial orphan cleanup could not be retried", err)
			}
			checkOrphanRetired(t, f.load(t, w.Branch))
			checkNoOrphanGitDeletion(t, f)
			if f.sandbox.present || f.mux.window != "" {
				t.Fatal("owned resource survived explicit removal")
			}
		})
	}
}

func TestOrphanInspectionErrorsAreNotAbsence(t *testing.T) {
	for _, kind := range []string{"mux ownership", "container ownership", "Podman unavailable", "Git error"} {
		t.Run(kind, func(t *testing.T) {
			f, w := orphanFixture(t, kind != "mux ownership", false)
			f.mux.window, f.sandbox.present = "", false
			failure := fmt.Errorf("inspection failed: %s", kind)
			switch kind {
			case "mux ownership":
				f.mux.findErr, f.mux.quiesceErr = failure, failure
			case "container ownership", "Podman unavailable":
				f.sandbox.existsErr = failure
			case "Git error":
				f.runner.git = func(p Process) error {
					if slices.Equal(hostGitArgs(p), []string{"worktree", "list", "--porcelain", "-z"}) {
						return failure
					}
					return nil
				}
			}
			if err := f.run(t, "add", w.Branch); err == nil || !strings.Contains(err.Error(), "inspection failed") {
				t.Fatalf("add guessed absence: %v", err)
			}
			if !reflect.DeepEqual(w, f.load(t, w.Branch)) {
				t.Fatal("failed inspection retired ready state")
			}
			if err := f.run(t, "remove", w.Branch, "--force"); err == nil || !strings.Contains(err.Error(), "inspection failed") {
				t.Fatalf("remove guessed absence: %v", err)
			}
			if f.load(t, w.Branch).Stage == "removed" || slices.Contains(f.events, "sandbox:remove") || slices.Contains(f.events, "mux:close") {
				t.Fatal("uncertain ownership authorized removal")
			}
		})
	}
}

func TestOrphanReplacementPathsAndMovedBranchesAreUntouched(t *testing.T) {
	for _, kind := range []string{"directory", "symlink", "dangling symlink"} {
		t.Run(kind, func(t *testing.T) {
			f, w := orphanFixture(t, false, false)
			elsewhere := filepath.Join(filepath.Dir(f.root), "legitimate")
			switch kind {
			case "directory":
				if err := os.Mkdir(w.Path, 0700); err != nil {
					t.Fatal(err)
				}
				hostWrite(t, filepath.Join(w.Path, "precious"), "replacement files")
			case "symlink", "dangling symlink":
				target := f.root
				if kind == "dangling symlink" {
					target = elsewhere
				}
				if err := os.Symlink(target, w.Path); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"add", w.Branch}, {"remove", w.Branch, "--force"}, {"close", w.Branch}} {
				if err := f.run(t, args...); err == nil {
					t.Fatalf("unsafe %v returned %v", args, err)
				}
			}
			if f.load(t, w.Branch).Stage != "ready" || f.mux.window == "" {
				t.Fatal("replacement/moved branch changed recorded runtime")
			}
			path := w.Path
			if _, err := os.Lstat(path); err != nil {
				t.Fatal("replacement path was removed", err)
			}
			checkNoOrphanGitDeletion(t, f)
		})
	}
}

func TestOrphanStaleRegistrationIsTargeted(t *testing.T) {
	f, w := orphanFixture(t, false, true)
	other := filepath.Join(filepath.Dir(f.root), "other-stale")
	hostGit(t, f.root, "worktree", "add", "-b", "other", other)
	otherPointer, err := os.ReadFile(filepath.Join(other, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	otherAdmin := strings.TrimSpace(strings.TrimPrefix(string(otherPointer), "gitdir: "))
	otherID, err := identityAt(otherAdmin, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(other); err != nil {
		t.Fatal(err)
	}
	before, err := inspectStaleRegistration(w.Workspace)
	if err != nil || before == nil {
		t.Fatalf("could not find stale registration: %+v, %v", before, err)
	}
	if err := f.run(t, "add", w.Branch, "-b"); err == nil || !strings.Contains(err.Error(), "Git registration remains") {
		t.Fatalf("add pruned stale registration: %v", err)
	}
	f.mux.onClose = func(Workspace) {
		if _, err := os.Stat(filepath.Join(w.CommonDir, "worktrees", before.Name)); err != nil {
			t.Fatal("registration removed before window closure", err)
		}
	}
	if err := f.run(t, "remove", w.Branch, "--force"); err != nil {
		t.Fatal(err)
	}
	checkOrphanRetired(t, f.load(t, w.Branch))
	checkNoOrphanGitDeletion(t, f)
	if _, err := os.Stat(filepath.Join(w.CommonDir, "worktrees", before.Name)); !os.IsNotExist(err) {
		t.Fatalf("stale admin remains: %v", err)
	}
	if err := verifyIdentityAt(otherAdmin, true, otherID); err != nil {
		t.Fatal("other prunable registration was touched", err)
	}
	if hostGit(t, f.root, "rev-parse", w.Branch) != w.InitialCommit {
		t.Fatal("stale registration recovery changed branch")
	}
	if err := f.run(t, "add", w.Branch, "-b"); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanStaleRegistrationRejectsUnsafeControlFiles(t *testing.T) {
	for _, kind := range []string{"locked", "backlink", "commondir", "HEAD", "symlink", "hardlink", "index lock"} {
		t.Run(kind, func(t *testing.T) {
			f, w := orphanFixture(t, false, true)
			reg, err := inspectStaleRegistration(w.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			admin := filepath.Join(w.CommonDir, "worktrees", reg.Name)
			switch kind {
			case "locked":
				hostWrite(t, filepath.Join(admin, "locked"), "preserve")
			case "backlink":
				hostWrite(t, filepath.Join(admin, "gitdir"), filepath.Join(f.root, ".git")+"\n")
			case "commondir":
				hostWrite(t, filepath.Join(admin, "commondir"), "../../elsewhere\n")
			case "HEAD":
				hostWrite(t, filepath.Join(admin, "HEAD"), "ref: refs/heads/main\n")
			case "symlink", "hardlink":
				path := filepath.Join(admin, "index")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink(filepath.Join(w.CommonDir, "index"), path)
				} else {
					err = os.Link(filepath.Join(w.CommonDir, "index"), path)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "index lock":
				hostWrite(t, filepath.Join(admin, "index.lock"), "busy")
			}
			if err := f.run(t, "remove", w.Branch, "--force"); err == nil {
				t.Fatal("unsafe registration was removed")
			}
			if _, err := os.Lstat(admin); err != nil {
				t.Fatal(err)
			}
			if f.mux.window == "" || f.load(t, w.Branch).Stage == "removed" {
				t.Fatal("unsafe registration changed managed runtime")
			}
		})
	}
}

func TestOrphanReplacementBeforeMetadataActionIsPreserved(t *testing.T) {
	f, w := orphanFixture(t, false, true)
	created := false
	f.runner.git = func(p Process) error {
		if !slices.Equal(hostGitArgs(p), []string{"worktree", "list", "--porcelain", "-z"}) || created {
			return nil
		}
		state := f.load(t, w.Branch)
		if state.Removal != nil && state.Removal.Recovery.Registration.Phase == "prepared" {
			created = true
			if err := os.Mkdir(w.Path, 0700); err != nil {
				t.Fatal(err)
			}
			hostWrite(t, filepath.Join(w.Path, "precious"), "new owner")
		}
		return nil
	}
	if err := f.run(t, "remove", w.Branch, "--force"); err == nil || !created {
		t.Fatalf("replacement race did not refuse: %v, %v", err, created)
	}
	data, err := os.ReadFile(filepath.Join(w.Path, "precious"))
	if err != nil || string(data) != "new owner" {
		t.Fatalf("replacement files lost: %s, %v", data, err)
	}
	state := f.load(t, w.Branch)
	if state.Stage != "removing" || state.Removal.Job.Status != "failed" {
		t.Fatalf("replacement lost retry journal: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(w.CommonDir, "worktrees", state.Removal.Recovery.Registration.Name)); err != nil {
		t.Fatal("registration deleted after replacement appeared", err)
	}
}

func TestOrphanMetadataCrashCheckpointsResume(t *testing.T) {
	for _, phase := range []string{"prepared", "renamed before checkpoint", "parent removed", "quarantined", "partly deleted", "deleted before checkpoint"} {
		t.Run(phase, func(t *testing.T) {
			f, w := orphanFixture(t, false, true)
			store := hostStateStore(t, f)
			reg, err := inspectStaleRegistration(w.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := captureRecovery(w.Workspace, "explicit orphan recovery", reg)
			if err != nil {
				t.Fatal(err)
			}
			reg.Quarantine, reg.Phase = ".cli-workmux-orphan-"+strings.Repeat("b", 32), "prepared"
			w.Stage = "removing"
			w.Removal = &removalState{Recovery: recovery, HookDone: true, KeepBranch: true}
			original := filepath.Join(w.CommonDir, "worktrees", reg.Name)
			quarantine := filepath.Join(w.CommonDir, reg.Quarantine)
			if phase != "prepared" {
				if err := os.Rename(original, quarantine); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "quarantined" {
				reg.Phase = "quarantined"
			}
			if phase == "parent removed" {
				if err := os.Remove(filepath.Join(w.CommonDir, "worktrees")); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "partly deleted" || phase == "deleted before checkpoint" {
				reg.Phase = "deleting"
				if err := os.Remove(filepath.Join(quarantine, "HEAD")); err != nil {
					t.Fatal(err)
				}
			}
			if phase == "deleted before checkpoint" {
				if err := os.RemoveAll(quarantine); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.save(w); err != nil {
				t.Fatal(err)
			}
			if err := store.unlock(); err != nil {
				t.Fatal(err)
			}
			if err := f.run(t, "remove", w.Branch); err != nil {
				t.Fatal("could not resume metadata checkpoint", err)
			}
			checkOrphanRetired(t, f.load(t, w.Branch))
			if _, err := os.Lstat(quarantine); !os.IsNotExist(err) {
				t.Fatalf("quarantine remains: %v", err)
			}
		})
	}
}

func TestOrphanActiveWorkerBlocksAndExpiredTokenCannotTouchReAdd(t *testing.T) {
	f := newHostFixture(t, "")
	store, w, command := queuedCleanup(t, f)
	hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", w.Branch}, {"remove", w.Branch, "--force"}} {
		if err := f.run(t, args...); err == nil || !strings.Contains(err.Error(), "already scheduled") {
			t.Fatalf("active worker not blocked: %v", err)
		}
	}
	store = hostStateStore(t, f)
	w.Removal.Job.Deadline = time.Now().Add(-time.Second).UnixNano()
	if err := store.save(w); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	f.mux.window = ""
	if err := f.run(t, "add", w.Branch, "-b"); err != nil {
		t.Fatal("expired cleanup history prevented metadata reconciliation", err)
	}
	fresh := f.load(t, w.Branch)
	var ready bytes.Buffer
	if err := f.app.RunCleanup(context.Background(), command, &ready); err == nil || ready.Len() != 0 {
		t.Fatalf("old worker accepted recreated deterministic ID: %v", err)
	}
	if !reflect.DeepEqual(fresh, f.load(t, w.Branch)) || f.mux.window == "" {
		t.Fatal("expired worker changed new resources")
	}
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatal("expired worker deleted new files", err)
	}
}

func TestOrphanCloseDoesNotRetireStoppedContainer(t *testing.T) {
	f, w := orphanFixture(t, true, false)
	if err := f.run(t, "open", w.Branch); err == nil || !strings.Contains(err.Error(), "use remove") {
		t.Fatalf("open did not explain missing worktree: %v", err)
	}
	if err := f.run(t, "close", w.Branch); err != nil {
		t.Fatal("close could not shut down missing workspace runtime", err)
	}
	if f.load(t, w.Branch).Stage != "closed" || !f.sandbox.present || f.mux.window != "" {
		t.Fatal("close retired container ownership")
	}
	if err := f.run(t, "add", w.Branch); err == nil || !strings.Contains(err.Error(), "managed resources remain") {
		t.Fatalf("stopped container allowed metadata retirement: %v", err)
	}
	if err := f.run(t, "remove", w.Branch); err != nil {
		t.Fatal(err)
	}
}

func TestOrphanPlannedWithoutExpectedContainerDoesNotProbePodman(t *testing.T) {
	f, w := orphanFixture(t, false, false)
	f.mux.window = ""
	store := hostStateStore(t, f)
	w.Stage, w.Container = "planned", ""
	w.Config.Sandbox.Enabled = true
	if err := store.save(w); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	f.app.Sandbox = nil
	if err := f.run(t, "add", w.Branch, "-b"); err != nil {
		t.Fatal("uncreated sandbox required Podman", err)
	}
}

func TestOrphanMetadataFailureRetriesWithoutDeletingReplacement(t *testing.T) {
	f, w := orphanFixture(t, false, true)
	failed := false
	quarantine := ""
	f.runner.git = func(p Process) error {
		if failed || !slices.Equal(hostGitArgs(p), []string{"worktree", "list", "--porcelain", "-z"}) {
			return nil
		}
		state := f.load(t, w.Branch)
		if state.Removal != nil && state.Removal.Recovery.Registration.Phase == "deleting" {
			failed = true
			quarantine = filepath.Join(w.CommonDir, state.Removal.Recovery.Registration.Quarantine)
			if err := os.Symlink(f.root, filepath.Join(quarantine, "unsafe")); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	}
	if err := f.run(t, "remove", w.Branch, "--force"); err == nil || !failed {
		t.Fatalf("unsafe partial metadata operation did not fail: %v", err)
	}
	state := f.load(t, w.Branch)
	if state.Removal.Recovery.Registration.Phase != "deleting" || state.Removal.Job.Status != "failed" || state.Stage == "removed" {
		t.Fatalf("metadata failure lost its checkpoint: %+v", state)
	}
	if err := os.Remove(filepath.Join(quarantine, "unsafe")); err != nil {
		t.Fatal(err)
	}
	f.runner.git = nil
	if err := f.run(t, "remove", w.Branch); err != nil {
		t.Fatal("metadata failure left permanent refusal", err)
	}
	checkOrphanRetired(t, f.load(t, w.Branch))
	if _, err := os.Stat(filepath.Join(f.root, "tracked")); err != nil {
		t.Fatal("quarantine link escaped confinement", err)
	}
}

func TestOrphanStaleRegistrationIdentityCannotBeReplaced(t *testing.T) {
	for _, kind := range []string{"admin", "backlink", "quarantine"} {
		t.Run(kind, func(t *testing.T) {
			f, w := orphanFixture(t, false, true)
			swapped := false
			original := ""
			f.runner.git = func(p Process) error {
				if swapped || !slices.Equal(hostGitArgs(p), []string{"worktree", "list", "--porcelain", "-z"}) {
					return nil
				}
				state := f.load(t, w.Branch)
				if state.Removal == nil {
					return nil
				}
				reg := state.Removal.Recovery.Registration
				if kind == "quarantine" && reg.Phase != "deleting" || kind != "quarantine" && reg.Phase != "prepared" {
					return nil
				}
				swapped = true
				original = filepath.Join(w.CommonDir, "worktrees", reg.Name)
				if kind == "backlink" {
					original = filepath.Join(original, "gitdir")
					data, err := os.ReadFile(original)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(original, original+"-old"); err != nil {
						t.Fatal(err)
					}
					hostWrite(t, original, string(data))
				} else {
					if kind == "quarantine" {
						original = filepath.Join(w.CommonDir, reg.Quarantine)
					}
					if err := os.Rename(original, filepath.Join(w.CommonDir, "saved-original")); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(original, 0700); err != nil {
						t.Fatal(err)
					}
					hostWrite(t, filepath.Join(original, "precious"), "new owner")
				}
				return nil
			}
			if err := f.run(t, "remove", w.Branch, "--force"); err == nil || !swapped {
				t.Fatalf("changed registration did not block recovery: %v", err)
			}
			if _, err := os.Lstat(original); err != nil {
				t.Fatal("replacement admin was removed", err)
			}
			if f.load(t, w.Branch).Stage == "removed" {
				t.Fatal("changed registration was retired")
			}
		})
	}
}

func TestOrphanActualSelfWindowFromSurvivingRoot(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%t", stale), func(t *testing.T) {
			ctx, runner, mux, session := isolatedTmux(t)
			f, w := orphanFixture(t, false, stale)
			gate := filepath.Join(f.home, "start")
			output := filepath.Join(f.home, "output")
			workerPIDPath := filepath.Join(f.home, "worker-pid")
			parentPIDPath := filepath.Join(f.home, "parent-pid")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			paneCommand := []string{"env", "HOME=" + f.home, "WORKMUX_TEST_HOME=" + f.home,
				"WORKMUX_TEST_STATE=" + f.state, "WORKMUX_TEST_CONFIG=" + f.config,
				"WORKMUX_TEST_GATE=" + gate, "WORKMUX_TEST_OUTPUT=" + output,
				"WORKMUX_TEST_PARENT_PID=" + parentPIDPath,
				"WORKMUX_TEST_WORKER_PID=" + workerPIDPath, "WORKMUX_TEST_HOLD_PARENT=1",
				executable, "workmux", "remove", w.Branch, "--force"}
			cwd := freshTmuxWorkspace(w.Workspace)
			cwd.Path = w.Root
			window, err := mux.Create(ctx, session, cwd, []Pane{{}}, [][]string{paneCommand})
			if err != nil {
				t.Fatal(err)
			}
			store := hostStateStore(t, f)
			w.Socket, w.Window = runner.socket, window
			server, err := mux.CaptureServer(ctx, runner.socket)
			if err != nil {
				t.Fatal(err)
			}
			w.rememberServer(server)
			if err := store.save(w); err != nil {
				t.Fatal(err)
			}
			if err := store.unlock(); err != nil {
				t.Fatal(err)
			}
			pidData, err := runner.Run(ctx, Process{Name: "tmux", Args: []string{"list-panes", "-t", window, "-F", "#{pane_pid}"}})
			if err != nil {
				t.Fatal(err)
			}
			panePID, err := strconv.Atoi(strings.TrimSpace(string(pidData)))
			if err != nil || panePID <= 1 {
				t.Fatalf("invalid source pane PID %q", pidData)
			}
			parentPID := waitTestProcessPID(t, ctx, parentPIDPath)
			if parentPID == panePID {
				t.Fatal("configured helper must be distinct from pane supervisor")
			}
			hostWrite(t, gate, "start")
			for {
				state, readErr := readState(filepath.Join(f.state, w.RepoID, w.ID+".json"))
				if readErr == nil && state.Stage == "removed" {
					checkOrphanRetired(t, state)
					break
				}
				if readErr == nil && state.Removal != nil && state.Removal.Job != nil && state.Removal.Job.Status == "failed" {
					data, _ := os.ReadFile(filepath.Join(f.state, w.RepoID, w.ID+"."+state.Removal.Job.Token+".cleanup.log"))
					t.Fatalf("orphan worker failed: %s", data)
				}
				if ctx.Err() != nil {
					data, _ := os.ReadFile(output)
					t.Fatalf("orphan worker timed out: %+v, %v, %s", state, readErr, data)
				}
				time.Sleep(20 * time.Millisecond)
			}
			data, err := os.ReadFile(output)
			if err != nil || !bytes.Contains(data, []byte("cleanup scheduled")) || !bytes.Contains(data, []byte("skipping pre_remove")) || bytes.Contains(data, []byte("merge succeeded")) {
				t.Fatalf("incorrect worker handoff output: %s, %v", data, err)
			}
			if found, err := mux.Find(ctx, w.Workspace); err != nil || found != "" {
				t.Fatalf("owned caller window remains: %s, %v", found, err)
			}
			data, err = os.ReadFile(workerPIDPath)
			if err != nil {
				t.Fatal(err)
			}
			workerPID, err := strconv.Atoi(string(data))
			if err != nil || workerPID <= 1 || workerPID == parentPID || workerPID == panePID {
				t.Fatalf("no detached worker: %s, %v", data, err)
			}
			waitCleanupProcessExit(t, ctx, parentPID)
			waitCleanupProcessExit(t, ctx, panePID)
			waitCleanupProcessExit(t, ctx, workerPID)
			if hostGit(t, f.root, "rev-parse", w.Branch) != w.InitialCommit {
				t.Fatal("orphan worker deleted surviving branch")
			}
		})
	}
}

func TestOrphanLiveServerLostSocketCannotBeForced(t *testing.T) {
	ctx, runner, mux, _ := isolatedTmux(t)
	f := newHostFixture(t, "")
	f.app.Mux = mux
	if err := f.run(t, "add", "topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "topic")
	hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	alias := runner.socket + "-original"
	if err := os.Rename(runner.socket, alias); err != nil {
		t.Fatal(err)
	}
	old := isolatedTmuxRunner{socket: alias, home: runner.home}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = old.Run(cleanup, Process{Name: "tmux", Args: []string{"kill-server"}})
	})
	for _, args := range [][]string{{"add", w.Branch}, {"remove", w.Branch, "--force"}} {
		if err := f.run(t, args...); err == nil || !strings.Contains(err.Error(), "cannot prove") {
			t.Fatalf("lost live server was treated as absent: %v", err)
		}
	}
	if f.load(t, w.Branch).Stage == "removed" {
		t.Fatal("lost live server was retired")
	}
	if out, err := old.Run(ctx, Process{Name: "tmux", Args: []string{"display-message", "-p", "-t", w.Window, "#{window_id}"}}); err != nil || strings.TrimSpace(string(out)) != w.Window {
		t.Fatalf("original live window changed: %s, %v", out, err)
	}
}

func TestOrphanCannotDiscardLegacyJobOnlySocket(t *testing.T) {
	f := newHostFixture(t, "")
	store, w, _ := queuedCleanup(t, f)
	hostGit(t, f.root, "worktree", "remove", "--", w.Path)
	w.Window, w.Socket = "", ""
	w.Removal.Job.Deadline = time.Now().Add(-time.Second).UnixNano()
	if err := store.save(w); err != nil {
		t.Fatal(err)
	}
	if err := store.unlock(); err != nil {
		t.Fatal(err)
	}
	f.mux.window = ""
	for _, args := range [][]string{{"add", w.Branch}, {"remove", w.Branch, "--force"}} {
		if err := f.run(t, args...); err == nil || !strings.Contains(err.Error(), "legacy cleanup lacks") {
			t.Fatalf("legacy job socket was discarded: %v", err)
		}
		if !reflect.DeepEqual(w, f.load(t, w.Branch)) {
			t.Fatal("legacy orphan lost its last ownership evidence")
		}
	}
}

func pruneOrphanRegistration(t *testing.T, w workspaceState) {
	t.Helper()
	reg := w.Removal.Recovery.Registration
	path := filepath.Join(w.CommonDir, "worktrees", reg.Name)
	if reg.Phase == "quarantined" {
		path = filepath.Join(w.CommonDir, reg.Quarantine)
	}
	if err := verifyStaleRegistration(w.Workspace, reg, path); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
}

func failedOrphanRegistration(t *testing.T, phase string) (*hostFixture, workspaceState) {
	t.Helper()
	f, w := orphanFixture(t, true, true)
	f.mux.closeErr = fmt.Errorf("temporary runtime close failure")
	if err := f.run(t, "remove", w.Branch); err == nil {
		t.Fatal("expected runtime failure before metadata cleanup")
	}
	w = f.load(t, w.Branch)
	if w.Removal.Recovery.Registration.Phase != "" || w.Removal.Job.Status != "failed" {
		t.Fatalf("missing interrupted orphan recovery: %+v", w.Removal)
	}
	if phase != "" {
		store := hostStateStore(t, f)
		reg := w.Removal.Recovery.Registration
		reg.Phase, reg.Quarantine = phase, ".cli-workmux-orphan-"+strings.Repeat("f", 32)
		if phase == "quarantined" {
			if err := os.Rename(filepath.Join(w.CommonDir, "worktrees", reg.Name), filepath.Join(w.CommonDir, reg.Quarantine)); err != nil {
				t.Fatal(err)
			}
		}
		if err := store.save(w); err != nil {
			t.Fatal(err)
		}
		if err := store.unlock(); err != nil {
			t.Fatal(err)
		}
	}
	return f, w
}

func TestOrphanExternalPruningCanResumeCleanup(t *testing.T) {
	for _, phase := range []string{"", "prepared", "quarantined"} {
		for _, resourcesGone := range []bool{false, true} {
			t.Run(fmt.Sprintf("phase=%s/resourcesGone=%t", phase, resourcesGone), func(t *testing.T) {
				f, w := failedOrphanRegistration(t, phase)
				pruneOrphanRegistration(t, w)
				if resourcesGone {
					f.mux.window, f.sandbox.present = "", false
				}
				f.mux.closeErr = nil
				f.sandbox.rmErr = fmt.Errorf("temporary container removal failure")
				if err := f.run(t, "remove", w.Branch, "--force"); err == nil || !strings.Contains(err.Error(), "container removal failure") {
					t.Fatalf("external prune prevented subsequent cleanup: %v", err)
				}
				pending := f.load(t, w.Branch)
				reg := pending.Removal.Recovery.Registration
				if reg.Phase != "absent" || reg.Quarantine != w.Removal.Recovery.Registration.Quarantine || pending.Removal.Job.Status != "failed" {
					t.Fatalf("external absence was not durably recorded: %+v", pending.Removal)
				}
				f.sandbox.rmErr = nil
				if err := f.run(t, "remove", w.Branch); err != nil {
					t.Fatal("externally pruned recovery remained stuck", err)
				}
				checkOrphanRetired(t, f.load(t, w.Branch))
				checkNoOrphanGitDeletion(t, f)
				if err := f.run(t, "remove", w.Branch); err != nil {
					t.Fatal("repeat remove failed", err)
				}
				if hostGit(t, f.root, "rev-parse", w.Branch) != w.InitialCommit {
					t.Fatal("external pruning recovery deleted surviving branch")
				}
				if err := f.run(t, "add", w.Branch, "-b"); err != nil {
					t.Fatal("external pruning blocked fresh add", err)
				}
				fresh := f.load(t, w.Branch)
				if fresh.BaseRef != "" || fresh.Removal != nil || fresh.MergedCommit != "" {
					t.Fatal("recreated workspace inherited old cleanup approval")
				}
				var ready bytes.Buffer
				old := CleanupCommand{RepoID: w.RepoID, ID: w.ID, Token: w.Removal.Job.Token}
				if err := f.app.RunCleanup(context.Background(), old, &ready); err == nil || ready.Len() != 0 {
					t.Fatalf("pruning revival accepted old worker token: %v", err)
				}
			})
		}
	}
}

func TestOrphanExternalPruningDoesNotHideReplacementOrInspectionError(t *testing.T) {
	for _, kind := range []string{"source directory", "new registration", "new admin", "moved branch", "dangling admin", "dangling quarantine", "permission"} {
		t.Run(kind, func(t *testing.T) {
			if kind == "permission" && os.Geteuid() == 0 {
				t.Skip("root bypasses directory permission checks")
			}
			f, w := failedOrphanRegistration(t, "prepared")
			pruneOrphanRegistration(t, w)
			reg := w.Removal.Recovery.Registration
			admin := filepath.Join(w.CommonDir, "worktrees", reg.Name)
			preserved := admin
			switch kind {
			case "source directory":
				preserved = w.Path
				if err := os.Mkdir(w.Path, 0700); err != nil {
					t.Fatal(err)
				}
				hostWrite(t, filepath.Join(w.Path, "precious"), "new owner")
			case "new registration":
				preserved = w.Path
				hostGit(t, f.root, "worktree", "add", w.Path, w.Branch)
			case "new admin":
				if err := os.Mkdir(admin, 0700); err != nil {
					t.Fatal(err)
				}
				hostWrite(t, filepath.Join(admin, "precious"), "unverified metadata")
			case "moved branch":
				preserved = filepath.Join(filepath.Dir(f.root), "elsewhere")
				hostGit(t, f.root, "worktree", "add", preserved, w.Branch)
			case "dangling admin", "dangling quarantine":
				if kind == "dangling quarantine" {
					preserved = filepath.Join(w.CommonDir, reg.Quarantine)
				}
				if err := os.Symlink(filepath.Join(f.home, "missing"), preserved); err != nil {
					t.Fatal(err)
				}
			case "permission":
				preserved = filepath.Dir(admin)
				if err := os.Chmod(preserved, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.Chmod(preserved, 0700); err != nil {
						t.Error(err)
					}
				})
			}
			f.mux.closeErr = nil
			if err := f.run(t, "remove", w.Branch, "--force"); err == nil {
				t.Fatal("replacement or inspection error became absence")
			}
			if got := f.load(t, w.Branch); got.Stage == "removed" || got.Removal.Recovery.Registration.Phase != "prepared" {
				t.Fatalf("unverified absence changed recovery phase: %+v", got)
			}
			if _, err := os.Lstat(preserved); err != nil {
				t.Fatal("replacement resource was removed", err)
			}
			if f.mux.window == "" || !f.sandbox.present {
				t.Fatal("ambiguous registration changed runtime resources")
			}
		})
	}
}
