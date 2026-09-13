package workmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type recoveryState struct {
	Reason       string             `json:"reason"`
	Root         fileIdentity       `json:"root"`
	Common       fileIdentity       `json:"common"`
	Registration *staleRegistration `json:"registration,omitempty"`
}

type staleRegistration struct {
	Name          string       `json:"name"`
	Parent        fileIdentity `json:"parent"`
	Admin         fileIdentity `json:"admin"`
	Backlink      fileIdentity `json:"backlink"`
	CommonPointer fileIdentity `json:"common_pointer"`
	Head          fileIdentity `json:"head"`
	Quarantine    string       `json:"quarantine,omitempty"`
	Phase         string       `json:"phase,omitempty"`
}

type workspacePresence struct {
	Directory    bool
	Registration *staleRegistration
}

func missingWorkspacePath(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("data exists at the workspace path %s; replacement or unregistered files will not be removed", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	// ENOENT may hide a dangling symlink in an ancestor. Inspect the nearest existing one.
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		_, err := identityAt(parent, true)
		return err
	}
}

func (g gitHost) workspacePresence(ctx context.Context, repo gitRepository, w Workspace) (workspacePresence, error) {
	var result workspacePresence
	if err := repo.validateMain(); err != nil {
		return result, err
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return result, err
	}
	var registered *gitWorktree
	for _, tree := range trees {
		if tree.Branch == w.Branch && tree.Path != w.Path {
			return result, fmt.Errorf("branch %q is checked out at %s; its worktree and managed resources were left unchanged", w.Branch, tree.Path)
		}
		if tree.Path == w.Path {
			registered = &tree
		}
	}
	if info, err := os.Lstat(w.Path); err == nil {
		if !info.IsDir() || registered == nil {
			return result, fmt.Errorf("unregistered or replacement data exists at the workspace path %s; refusing to delete it", w.Path)
		}
		if _, err := g.source(ctx, repo, w); err != nil {
			return result, err
		}
		result.Directory = true
		return result, nil
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if err := missingWorkspacePath(w.Path); err != nil {
		return result, err
	}
	if registered != nil && (registered.Locked || registered.Bare || registered.Branch != w.Branch) {
		return result, fmt.Errorf("missing worktree registration is locked or has changed branch; refusing recovery")
	}
	result.Registration, err = inspectStaleRegistration(w)
	if err != nil {
		return result, err
	}
	if (registered == nil) != (result.Registration == nil) {
		return result, fmt.Errorf("worktree registration changed or could not be identified; retry after inspecting %s", w.Path)
	}
	return result, nil
}

func registrationPointer(admin, name string) (string, error) {
	data, err := controlFile(filepath.Join(admin, name), false)
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(string(data), "\n")
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("invalid stale worktree %s pointer", name)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(admin, value)
	}
	return filepath.Clean(value), nil
}

func inspectStaleRegistration(w Workspace) (*staleRegistration, error) {
	parent := filepath.Join(w.CommonDir, "worktrees")
	if _, err := os.Lstat(parent); os.IsNotExist(err) {
		return nil, noSymlinkPath(w.CommonDir)
	} else if err != nil {
		return nil, err
	}
	parentID, err := identityAt(parent, true)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil, err
	}
	var found *staleRegistration
	for _, entry := range entries {
		admin := filepath.Join(parent, entry.Name())
		if _, err := identityAt(admin, true); err != nil {
			return nil, err
		}
		backlink, err := registrationPointer(admin, "gitdir")
		if err != nil {
			return nil, err
		}
		if backlink != filepath.Join(w.Path, ".git") {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("multiple Git administration directories claim the missing worktree")
		}
		found = &staleRegistration{Name: entry.Name(), Parent: parentID}
		for _, entry := range []struct {
			name string
			dir  bool
			id   *fileIdentity
		}{
			{"", true, &found.Admin}, {"gitdir", false, &found.Backlink},
			{"commondir", false, &found.CommonPointer}, {"HEAD", false, &found.Head},
		} {
			id, err := identityAt(filepath.Join(admin, entry.name), entry.dir)
			if err != nil {
				return nil, err
			}
			*entry.id = id
		}
		if err := verifyStaleRegistration(w, found, admin); err != nil {
			return nil, err
		}
	}
	return found, nil
}

func verifyStaleRegistration(w Workspace, saved *staleRegistration, admin string) error {
	for _, entry := range []struct {
		name string
		dir  bool
		id   fileIdentity
	}{
		{"", true, saved.Admin}, {"gitdir", false, saved.Backlink},
		{"commondir", false, saved.CommonPointer}, {"HEAD", false, saved.Head},
	} {
		if err := verifyIdentityAt(filepath.Join(admin, entry.name), entry.dir, entry.id); err != nil {
			return err
		}
	}
	for name, expected := range map[string]string{"gitdir": filepath.Join(w.Path, ".git"), "commondir": w.CommonDir} {
		data, err := controlFile(filepath.Join(admin, name), false)
		if err != nil {
			return err
		}
		value := strings.TrimSuffix(string(data), "\n")
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid stale worktree %s pointer", name)
		}
		if !filepath.IsAbs(value) {
			// Relative pointers retain their original meaning after quarantine.
			value = filepath.Join(w.CommonDir, "worktrees", saved.Name, value)
		}
		if filepath.Clean(value) != expected {
			return fmt.Errorf("stale worktree %s pointer changed; metadata preserved", name)
		}
	}
	head, err := controlFile(filepath.Join(admin, "HEAD"), false)
	if err != nil {
		return err
	}
	if string(head) != "ref: refs/heads/"+w.Branch+"\n" {
		return fmt.Errorf("stale worktree HEAD does not match its saved branch")
	}
	return safeRegistrationFiles(admin)
}

func safeRegistrationFiles(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return checkRegistrationFiles(root)
}

func checkRegistrationFiles(root *os.Root) error {
	return fs.WalkDir(root.FS(), ".", func(name string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if filepath.Base(name) == "locked" || strings.HasSuffix(name, ".lock") {
			return fmt.Errorf("stale Git metadata is locked: %s", name)
		}
		if !info.IsDir() && (!info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1) {
			return fmt.Errorf("unsafe stale Git control file: %s", name)
		}
		return nil
	})
}

func verifyIdentityAt(path string, directory bool, expected fileIdentity) error {
	id, err := identityAt(path, directory)
	if err != nil {
		return err
	}
	if id != expected {
		return fmt.Errorf("captured recovery identity changed: %s; replacement data preserved", path)
	}
	return nil
}

func captureRecovery(w Workspace, reason string, registration *staleRegistration) (*recoveryState, error) {
	root, err := identityAt(w.Root, true)
	if err != nil {
		return nil, err
	}
	common, err := identityAt(w.CommonDir, true)
	if err != nil {
		return nil, err
	}
	return &recoveryState{Reason: reason, Root: root, Common: common, Registration: registration}, nil
}

func verifyRecovery(w Workspace, recovery *recoveryState) error {
	if err := verifyIdentityAt(w.Root, true, recovery.Root); err != nil {
		return err
	}
	if err := verifyIdentityAt(w.CommonDir, true, recovery.Common); err != nil {
		return err
	}
	return missingWorkspacePath(w.Path)
}

func validateRecovery(removal *removalState) error {
	r := removal.Recovery
	if r == nil {
		return nil
	}
	if r.Reason != "metadata-only orphan" && r.Reason != "explicit orphan recovery" || r.Root.Inode == 0 || r.Common.Inode == 0 {
		return fmt.Errorf("invalid orphan recovery identity or reason")
	}
	if !removal.KeepBranch || !removal.HookDone || removal.BranchDeleted || removal.Force || removal.DiscardCommits || removal.Identity != nil || removal.Head != "" || removal.Target != "" || removal.TargetOID != "" || removal.TargetRefOID != "" || removal.MergeResult != "" || removal.WorktreeRemovalStarted {
		return fmt.Errorf("orphan recovery cannot authorize branch or worktree deletion; pre_remove must be skipped")
	}
	if reg := r.Registration; reg != nil {
		if !safeRelative(reg.Name) || strings.ContainsAny(reg.Name, "/\\") {
			return fmt.Errorf("invalid stale registration name")
		}
		for _, id := range []fileIdentity{reg.Parent, reg.Admin, reg.Backlink, reg.CommonPointer, reg.Head} {
			if id.Inode == 0 {
				return fmt.Errorf("invalid stale registration identity")
			}
		}
		switch reg.Phase {
		case "":
			if reg.Quarantine != "" {
				return fmt.Errorf("unprepared stale registration quarantine")
			}
		case "absent":
			if reg.Quarantine == "" {
				break
			}
			fallthrough
		case "prepared", "quarantined", "deleting", "removed":
			if !strings.HasPrefix(reg.Quarantine, ".cli-workmux-orphan-") || !validID(strings.TrimPrefix(reg.Quarantine, ".cli-workmux-orphan-")) {
				return fmt.Errorf("invalid stale registration quarantine")
			}
		default:
			return fmt.Errorf("invalid stale registration checkpoint")
		}
	}
	return nil
}

func expectedContainer(w Workspace) bool {
	return w.Container != "" || w.SandboxUsed
}

func workspaceHasSandbox(w Workspace) bool { return w.Container != "" || w.SandboxUsed }

func (app *App) orphanResourcesAbsent(ctx context.Context, w Workspace) error {
	window, err := app.Mux.Find(ctx, w)
	if err != nil {
		return err
	}
	present := false
	if expectedContainer(w) {
		if app.Sandbox == nil {
			return fmt.Errorf("saved workspace requires its sandbox implementation")
		}
		present, err = app.Sandbox.Exists(ctx, w)
		if err != nil {
			return err
		}
	}
	if window != "" || present {
		return fmt.Errorf("workspace files missing but managed resources remain; run cli workmux remove %s to finish cleanup", w.Handle)
	}
	return nil
}

func (app *App) reconcileAdd(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	if state.Removal != nil {
		if cleanupActive(state.Removal.Job) {
			return fmt.Errorf("cleanup is already scheduled; retry after its worker finishes or handoff expires")
		}
		if state.Removal.Recovery != nil || state.Removal.WorktreeRemovalStarted || state.Removal.WorktreeRemoved {
			return fmt.Errorf("cleanup is incomplete; run cli workmux remove %s to finish cleanup", state.Handle)
		}
	}
	if err := app.checkCleanupWindow(ctx, *state); err != nil {
		return err
	}
	presence, err := g.workspacePresence(ctx, repo, state.Workspace)
	if err != nil {
		return err
	}
	if presence.Directory {
		return fmt.Errorf("workspace %q already exists at stage %s; use open, or remove to recover a partial add", state.Handle, state.Stage)
	}
	if presence.Registration != nil {
		return fmt.Errorf("workspace files missing but Git registration remains; run cli workmux remove %s to finish cleanup", state.Handle)
	}
	if err := app.orphanResourcesAbsent(ctx, state.Workspace); err != nil {
		return err
	}
	recovery, err := captureRecovery(state.Workspace, "metadata-only orphan", nil)
	if err != nil {
		return err
	}
	// Probe again after inspection, immediately before replacing the old journal.
	if err := app.orphanResourcesAbsent(ctx, state.Workspace); err != nil {
		return err
	}
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, recovery); err != nil {
		return err
	}
	state.Removal = &removalState{Recovery: recovery, KeepBranch: true, HookDone: true, WorktreeRemoved: true, ContainerRemoved: true}
	return retireOrphan(store, state)
}

func (g gitHost) orphanStillMissing(ctx context.Context, repo gitRepository, w Workspace, recovery *recoveryState) error {
	if err := verifyRecovery(w, recovery); err != nil {
		return err
	}
	presence, err := g.workspacePresence(ctx, repo, w)
	if err != nil {
		return err
	}
	if presence.Directory {
		return fmt.Errorf("worktree reappeared during orphan recovery; replacement data preserved")
	}
	if current := presence.Registration; current != nil {
		saved := recovery.Registration
		if saved == nil || saved.Name != current.Name || saved.Admin != current.Admin || saved.Parent != current.Parent || saved.Backlink != current.Backlink || saved.CommonPointer != current.CommonPointer || saved.Head != current.Head || saved.Phase != "" && saved.Phase != "prepared" {
			return fmt.Errorf("worktree registration reappeared or changed during orphan recovery; metadata preserved")
		}
	} else if saved := recovery.Registration; saved != nil {
		if err := missingWorkspacePath(filepath.Join(w.CommonDir, "worktrees", saved.Name)); err != nil {
			return err
		}
		if saved.Quarantine != "" {
			path := filepath.Join(w.CommonDir, saved.Quarantine)
			absent, err := recoveryPathAbsent(path)
			if err != nil {
				return err
			}
			if !absent {
				if saved.Phase == "absent" || saved.Phase == "removed" {
					return fmt.Errorf("quarantined metadata reappeared; refusing to remove it")
				}
				if saved.Phase == "deleting" {
					if err := verifyIdentityAt(path, true, saved.Admin); err != nil {
						return err
					}
				} else if err := verifyStaleRegistration(w, saved, path); err != nil {
					return err
				}
			}
		}
	}
	return verifyRecovery(w, recovery)
}

func recoveryPathAbsent(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := missingWorkspacePath(path); err != nil {
		return false, err
	}
	return true, nil
}

func staleRegistrationAbsent(w Workspace, reg *staleRegistration) (bool, error) {
	absent, err := recoveryPathAbsent(filepath.Join(w.CommonDir, "worktrees", reg.Name))
	if err != nil || !absent || reg.Quarantine == "" {
		return absent, err
	}
	return recoveryPathAbsent(filepath.Join(w.CommonDir, reg.Quarantine))
}

func (app *App) recoverOrphan(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, presence workspacePresence) error {
	if err := app.checkCleanupWindow(ctx, *state); err != nil {
		return err
	}
	if state.Removal == nil || state.Removal.Recovery == nil {
		recovery, err := captureRecovery(state.Workspace, "explicit orphan recovery", presence.Registration)
		if err != nil {
			return err
		}
		state.Removal = &removalState{Recovery: recovery, KeepBranch: true, HookDone: true}
		state.MergeKept = false
		state.Stage = "removing"
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, state.Removal.Recovery); err != nil {
		return err
	}
	if expectedContainer(state.Workspace) {
		if app.Sandbox == nil {
			return fmt.Errorf("saved workspace requires its sandbox implementation")
		}
		if _, err := app.Sandbox.Exists(ctx, state.Workspace); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(app.Stdout, "Orphan recovery for %s: skipping pre_remove because its working directory is missing; any surviving branch is preserved, even with --force.\n", state.Handle); err != nil {
		return err
	}
	return app.cleanup(ctx, g, repo, store, state, false)
}

func retireOrphan(store *stateStore, state *workspaceState) error {
	w := state.Workspace
	removal := *state.Removal
	completed := workspaceState{Workspace: Workspace{ID: w.ID, RepoID: w.RepoID, Root: w.Root, CommonDir: w.CommonDir,
		Path: w.Path, Branch: w.Branch, Handle: w.Handle, Stage: "removed"}, Removal: &removal}
	// Invalidate the worker token and all merge/base/runtime history before deterministic IDs can be reused.
	completed.Removal.Job = nil
	if err := store.save(completed); err != nil {
		return err
	}
	*state = completed
	return nil
}

func (app *App) finishOrphan(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) (string, error) {
	r := state.Removal
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, r.Recovery); err != nil {
		return "identity_failed", err
	}
	if err := removeStaleRegistration(ctx, g, repo, store, state); err != nil {
		return "registration_remove_failed", err
	}
	r.WorktreeRemoved = true
	if err := store.save(*state); err != nil {
		return "checkpoint_failed", err
	}
	if expectedContainer(state.Workspace) {
		if err := app.Sandbox.Remove(ctx, state.Workspace); err != nil {
			return "container_remove_failed", err
		}
	}
	r.ContainerRemoved = true
	if err := app.orphanResourcesAbsent(ctx, state.Workspace); err != nil {
		return "resources_remain", err
	}
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, r.Recovery); err != nil {
		return "identity_failed", err
	}
	if err := retireOrphan(store, state); err != nil {
		return "checkpoint_failed", err
	}
	return "complete", nil
}

func removeStaleRegistration(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) error {
	recovery := state.Removal.Recovery
	reg := recovery.Registration
	if reg == nil {
		return nil
	}
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, recovery); err != nil {
		return err
	}
	if absent, err := staleRegistrationAbsent(state.Workspace, reg); err != nil {
		return err
	} else if absent {
		// External pruning is not our rename/deletion. Record only verified absence.
		if err := g.orphanStillMissing(ctx, repo, state.Workspace, recovery); err != nil {
			return err
		}
		if absent, err := staleRegistrationAbsent(state.Workspace, reg); err != nil || !absent {
			if err == nil {
				err = fmt.Errorf("worktree administration metadata reappeared before its absence checkpoint")
			}
			return err
		}
		reg.Phase = "absent"
		return store.save(*state)
	}
	if reg.Phase == "" {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		reg.Quarantine = ".cli-workmux-orphan-" + hex.EncodeToString(token[:])
		reg.Phase = "prepared"
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if err := g.orphanStillMissing(ctx, repo, state.Workspace, recovery); err != nil {
		return err
	}
	root, err := os.OpenRoot(state.CommonDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if (fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}) != recovery.Common {
		return fmt.Errorf("repository common directory changed before metadata cleanup")
	}
	original := filepath.Join("worktrees", reg.Name)
	quarantine := filepath.Join(state.CommonDir, reg.Quarantine)
	if reg.Phase == "prepared" {
		if _, err := root.Lstat(reg.Quarantine); os.IsNotExist(err) {
			if err := verifyIdentityAt(filepath.Join(state.CommonDir, "worktrees"), true, reg.Parent); err != nil {
				return err
			}
			if err := verifyStaleRegistration(state.Workspace, reg, filepath.Join(state.CommonDir, original)); err != nil {
				return err
			}
			if err := missingWorkspacePath(state.Path); err != nil {
				return err
			}
			// Only metadata is renamed. Git never receives the missing source path for removal.
			if err := root.Rename(original, reg.Quarantine); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := verifyStaleRegistration(state.Workspace, reg, quarantine); err != nil {
			return fmt.Errorf("quarantined metadata retained at %s: %w", quarantine, err)
		}
		if err := syncRegistrationParents(root); err != nil {
			return err
		}
		reg.Phase = "quarantined"
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if _, err := root.Lstat(original); err == nil {
		return fmt.Errorf("worktree administration entry reappeared at %s; metadata preserved", original)
	} else if !os.IsNotExist(err) {
		return err
	}
	if reg.Phase == "quarantined" {
		if err := verifyStaleRegistration(state.Workspace, reg, quarantine); err != nil {
			return err
		}
		reg.Phase = "deleting"
		if err := store.save(*state); err != nil {
			return err
		}
	}
	if reg.Phase == "deleting" {
		if err := g.orphanStillMissing(ctx, repo, state.Workspace, recovery); err != nil {
			return err
		}
		if _, err := root.Lstat(reg.Quarantine); err == nil {
			if err := deleteQuarantinedRegistration(root, reg); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := syncRegistrationParents(root); err != nil {
			return err
		}
		reg.Phase = "removed"
		return store.save(*state)
	}
	if _, err := root.Lstat(reg.Quarantine); err == nil {
		return fmt.Errorf("quarantined metadata reappeared; refusing to remove it")
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func deleteQuarantinedRegistration(common *os.Root, reg *staleRegistration) error {
	root, err := common.OpenRoot(reg.Quarantine)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Stat(".")
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if (fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}) != reg.Admin {
		return fmt.Errorf("quarantined Git metadata was replaced; refusing deletion")
	}
	if err := checkRegistrationFiles(root); err != nil {
		return err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := root.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	current, err := common.Lstat(reg.Quarantine)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("quarantined Git directory changed during cleanup; replacement preserved")
	}
	return common.Remove(reg.Quarantine)
}

func syncRegistrationParents(root *os.Root) error {
	for _, name := range []string{"worktrees", "."} {
		dir, err := root.Open(name)
		if name == "worktrees" && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
