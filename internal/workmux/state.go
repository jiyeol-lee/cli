package workmux

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type workspaceState struct {
	Workspace
	InitialCommit      string            `json:"initial_commit,omitempty"`
	OwnedIgnored       map[string]string `json:"owned_ignored,omitempty"`
	PendingMergeCommit string            `json:"pending_merge_commit,omitempty"`
	PendingMergeTarget string            `json:"pending_merge_target,omitempty"`
	MergeKept          bool              `json:"merge_kept,omitempty"`
	Removal            *removalState     `json:"removal,omitempty"`
}

type removalState struct {
	Head                   string           `json:"head,omitempty"`
	Target                 string           `json:"target,omitempty"`
	KeepBranch             bool             `json:"keep_branch,omitempty"`
	Force                  bool             `json:"force,omitempty"`
	HookDone               bool             `json:"hook_done,omitempty"`
	Stopped                bool             `json:"stopped,omitempty"`
	WorktreeRemovalStarted bool             `json:"worktree_removal_started,omitempty"`
	WorktreeRemoved        bool             `json:"worktree_removed,omitempty"`
	BranchDeleted          bool             `json:"branch_deleted,omitempty"`
	ContainerRemoved       bool             `json:"container_removed,omitempty"`
	Identity               *cleanupIdentity `json:"identity,omitempty"`
	Job                    *cleanupJob      `json:"job,omitempty"`
}

type stateStore struct {
	dir  string
	repo gitRepository
	lock *os.File
}

// Legacy container settings are accepted only in saved JSON, never in YAML config.
func (config *SandboxConfig) UnmarshalJSON(data []byte) error {
	var saved struct {
		Enabled   bool   `json:"enabled"`
		Image     string `json:"image"`
		Container *struct {
			Runtime string `json:"runtime"`
		} `json:"container"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return err
	}
	if saved.Enabled && saved.Container != nil && saved.Container.Runtime != "" && saved.Container.Runtime != "podman" {
		return fmt.Errorf("unsupported previous sandbox backend %q in workspace state: only Podman workspaces can be used; recover this workspace with the previous implementation", saved.Container.Runtime)
	}
	*config = SandboxConfig{Enabled: saved.Enabled, Image: saved.Image}
	return nil
}

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("state directory must be an absolute clean path")
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		parent := filepath.Dir(path)
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			if err := privateDirectory(parent); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := noSymlinkPath(parent); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("state directory must be owned by this user and private (0700): %s", path)
	}
	return noSymlinkPath(path)
}

func lockState(base string, repo gitRepository) (*stateStore, error) {
	if err := privateDirectory(base); err != nil {
		return nil, err
	}
	dir := filepath.Join(base, identity(repo.CommonDir))
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "lock")
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, fmt.Errorf("open repository lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := privateFile(file, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("repository is busy (another workmux command holds its lock): %w", err)
	}
	return &stateStore{dir: dir, repo: repo, lock: file}, nil
}

func privateFile(file *os.File, path string) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	link, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Nlink != 1 ||
		stat.Uid != uint32(os.Geteuid()) || !os.SameFile(info, link) {
		return fmt.Errorf("state file must be private (0600), owned, and not symlinked or hardlinked: %s", path)
	}
	return nil
}

func (store *stateStore) unlock() error {
	if store.lock == nil {
		return nil
	}
	file := store.lock
	store.lock = nil
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func validID(id string) bool {
	if len(id) != 32 || strings.ToLower(id) != id {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func workspacePath(root, handle string) string {
	return filepath.Join(filepath.Dir(root), filepath.Base(root)+"__worktrees", handle)
}

func (store *stateStore) validate(state workspaceState) error {
	w := state.Workspace
	if !validID(w.ID) || w.RepoID != identity(store.repo.CommonDir) || w.Root != store.repo.Root || w.CommonDir != store.repo.CommonDir {
		return fmt.Errorf("workspace state belongs to a different repository")
	}
	if err := safeName(w.Branch); err != nil {
		return err
	}
	if w.Handle != strings.ReplaceAll(w.Branch, "/", "-") || !safeRelative(w.Handle) || strings.ContainsAny(w.Handle, "/\\") {
		return fmt.Errorf("invalid workspace handle in state")
	}
	if w.Path != workspacePath(w.Root, w.Handle) || w.Path == w.Root || w.ID != identity(w.CommonDir, w.Path) {
		return fmt.Errorf("workspace path or identity does not match its owned location")
	}
	parent := filepath.Dir(w.Path)
	if _, err := os.Lstat(parent); err == nil {
		if err := noSymlinkPath(parent); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(w.Path); err == nil {
		if err := noSymlinkPath(w.Path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if w.Container != "" && w.Container != "cli-workmux-"+w.ID {
		return fmt.Errorf("invalid owned container name in state")
	}
	if w.Window != "" && !tmuxID(w.Window, '@') {
		return fmt.Errorf("invalid tmux window ID in state")
	}
	if w.Socket != "" && (!filepath.IsAbs(w.Socket) || strings.ContainsAny(w.Socket, "\x00\r\n")) {
		return fmt.Errorf("invalid saved tmux socket path")
	}
	if w.ServerPID != 0 || w.SocketDevice != 0 || w.SocketInode != 0 {
		if w.ServerPID <= 1 || w.SocketInode == 0 || w.Socket == "" {
			return fmt.Errorf("invalid saved tmux server identity")
		}
	}
	switch w.Stage {
	case "planned", "worktree", "files", "sandbox", "ready", "closed", "merging", "merged", "removing", "removed":
	default:
		return fmt.Errorf("unknown workspace recovery stage %q", w.Stage)
	}
	for _, oid := range []string{state.InitialCommit, state.PendingMergeCommit, w.MergedCommit} {
		if oid != "" && !validOID(oid) {
			return fmt.Errorf("invalid saved commit ID")
		}
	}
	for _, branch := range []string{state.PendingMergeTarget, w.MergeTarget} {
		if branch != "" {
			if err := safeName(branch); err != nil {
				return err
			}
		}
	}
	for path := range state.OwnedIgnored {
		if !safeRelative(path) {
			return fmt.Errorf("unsafe copied-file path in state")
		}
	}
	if state.Removal != nil {
		if state.Removal.Head != "" && !validOID(state.Removal.Head) {
			return fmt.Errorf("invalid removal commit ID")
		}
		if state.Removal.Target != "" {
			if err := safeName(state.Removal.Target); err != nil {
				return err
			}
		}
		if state.Removal.WorktreeRemovalStarted && (!state.Removal.HookDone || !state.Removal.Stopped || !validOID(state.Removal.Head)) {
			return fmt.Errorf("worktree removal checkpoint lacks completed hooks, shutdown, or source commit")
		}
		if err := validateCleanupState(w, state.Removal); err != nil {
			return err
		}
	}
	if state.MergeKept && (state.MergedCommit == "" || state.PendingMergeCommit != "" || state.Removal != nil) {
		return fmt.Errorf("retained merge record conflicts with pending work")
	}
	if w.Stage != "removed" {
		if err := w.Config.Validate(); err != nil {
			return fmt.Errorf("invalid saved workspace configuration: %w", err)
		}
		if _, err := w.Config.SelectPanes(w.Layout); err != nil {
			return err
		}
	}
	return nil
}

func (store *stateStore) load() ([]workspaceState, error) {
	if err := store.checkLock(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return nil, err
	}
	var states []workspaceState
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validID(id) {
			return nil, fmt.Errorf("invalid state filename")
		}
		path := filepath.Join(store.dir, entry.Name())
		state, err := readState(path)
		if err != nil {
			return nil, err
		}
		if state.ID != id {
			return nil, fmt.Errorf("state filename does not match workspace ID")
		}
		if err := store.validate(state); err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func readState(path string) (workspaceState, error) {
	var state workspaceState
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return state, fmt.Errorf("open workspace state: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	if err := privateFile(file, path); err != nil {
		return state, err
	}
	info, err := file.Stat()
	if err != nil {
		return state, err
	}
	if info.Size() > 4<<20 {
		return state, fmt.Errorf("workspace state exceeds the 4 MiB limit")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return state, fmt.Errorf("decode workspace state: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return state, fmt.Errorf("workspace state contains trailing data")
	}
	// Preserve older cleanup captures independently before a retry can replace its job.
	if state.Removal != nil && state.Removal.Job != nil {
		state.rememberServer(state.Removal.Job.Window)
	}
	return state, nil
}

func (state *workspaceState) rememberServer(captured CleanupWindow) {
	if captured.ServerPID > 1 && captured.SocketInode != 0 && (state.Socket == "" || state.Socket == captured.Socket) {
		if state.ServerPID != 0 && (state.ServerPID != captured.ServerPID || state.SocketDevice != captured.SocketDevice || state.SocketInode != captured.SocketInode) {
			return
		}
		state.Socket = captured.Socket
		state.ServerPID, state.SocketDevice, state.SocketInode = captured.ServerPID, captured.SocketDevice, captured.SocketInode
		if captured.ID != "" {
			state.Window = captured.ID
		}
	}
}

func (store *stateStore) save(state workspaceState) error {
	if err := store.checkLock(); err != nil {
		return err
	}
	if err := store.validate(state); err != nil {
		return err
	}
	path := filepath.Join(store.dir, state.ID+".json")
	if _, err := os.Lstat(path); err == nil {
		if _, err := readState(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.CreateTemp(store.dir, ".state-*")
	if err != nil {
		return err
	}
	// The temporary file is already renamed and closed on success.
	defer func() { _ = os.Remove(file.Name()) }()
	defer func() { _ = file.Close() }()
	if err := file.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(state); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 4<<20 {
		return fmt.Errorf("workspace state exceeds the 4 MiB limit")
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(store.dir)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func (store *stateStore) checkLock() error {
	if store.lock == nil {
		return fmt.Errorf("repository lock is not held")
	}
	if err := privateDirectory(store.dir); err != nil {
		return err
	}
	return privateFile(store.lock, filepath.Join(store.dir, "lock"))
}
