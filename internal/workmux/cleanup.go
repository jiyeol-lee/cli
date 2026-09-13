package workmux

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const cleanupWait = 5 * time.Second
const cleanupDelay = 300 * time.Millisecond
const cleanupLease = 20 * time.Second

type fileIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type cleanupIdentity struct {
	Root          fileIdentity `json:"root"`
	Common        fileIdentity `json:"common"`
	Path          fileIdentity `json:"path"`
	Admin         fileIdentity `json:"admin"`
	Pointer       fileIdentity `json:"pointer"`
	Backlink      fileIdentity `json:"backlink"`
	CommonPointer fileIdentity `json:"common_pointer"`
	AdminPath     string       `json:"admin_path"`
}

type cleanupJob struct {
	Token    string        `json:"token"`
	Status   string        `json:"status"`
	Deadline int64         `json:"deadline"`
	Window   CleanupWindow `json:"window"`
}

func validateCleanupState(w Workspace, removal *removalState) error {
	if err := validateRecovery(removal); err != nil {
		return err
	}
	if saved := removal.Identity; saved != nil {
		if filepath.Dir(saved.AdminPath) != filepath.Join(w.CommonDir, "worktrees") || !filepath.IsAbs(saved.AdminPath) || filepath.Clean(saved.AdminPath) != saved.AdminPath {
			return fmt.Errorf("invalid captured Git administration path")
		}
		for _, id := range []fileIdentity{saved.Root, saved.Common, saved.Path, saved.Admin, saved.Pointer, saved.Backlink, saved.CommonPointer} {
			if id.Inode == 0 {
				return fmt.Errorf("invalid captured filesystem identity")
			}
		}
	}
	if job := removal.Job; job != nil {
		seen := map[string]bool{job.Window.ID: true}
		for _, other := range job.Window.Others {
			if len(other.Others) != 0 || !tmuxID(other.ID, '@') || seen[other.ID] || other.Token != job.Token || other.Socket != job.Window.Socket || other.ServerPID != job.Window.ServerPID || other.SocketDevice != job.Window.SocketDevice || other.SocketInode != job.Window.SocketInode {
				return fmt.Errorf("invalid duplicate cleanup window identity")
			}
			seen[other.ID] = true
		}
		if !validID(job.Token) || job.Deadline <= 0 || job.Window.Token != job.Token {
			return fmt.Errorf("invalid cleanup handoff token")
		}
		if job.Window.ID != "" && (!tmuxID(job.Window.ID, '@') || !filepath.IsAbs(job.Window.Socket) || strings.ContainsAny(job.Window.Socket, "\x00\r\n")) {
			return fmt.Errorf("invalid captured tmux window")
		}
		if job.Window.ID != "" && w.Socket != "" && job.Window.Socket != w.Socket {
			return fmt.Errorf("cleanup window socket does not match its workspace")
		}
		if job.Window.ServerPID != 0 || job.Window.SocketDevice != 0 || job.Window.SocketInode != 0 {
			if job.Window.ServerPID <= 1 || job.Window.SocketInode == 0 || !filepath.IsAbs(job.Window.Socket) {
				return fmt.Errorf("invalid captured tmux server identity")
			}
			if w.ServerPID != 0 && (w.ServerPID != job.Window.ServerPID || w.SocketDevice != job.Window.SocketDevice || w.SocketInode != job.Window.SocketInode || w.Socket != job.Window.Socket) {
				return fmt.Errorf("cleanup capture conflicts with the saved tmux server identity")
			}
		}
		if !removal.HookDone || removal.Identity == nil && removal.Recovery == nil && !removal.WorktreeRemoved {
			return fmt.Errorf("cleanup handoff lacks completed hooks or filesystem identity")
		}
		switch job.Status {
		case "queued", "armed", "running", "failed", "cancelled", "complete":
		default:
			return fmt.Errorf("invalid cleanup handoff status")
		}
	}
	return nil
}

func identityAt(path string, directory bool) (fileIdentity, error) {
	if err := noSymlinkPath(path); err != nil {
		return fileIdentity{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fileIdentity{}, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if directory && !info.IsDir() || !directory && (!info.Mode().IsRegular() || stat.Nlink != 1) {
		return fileIdentity{}, fmt.Errorf("invalid cleanup control path: %s", path)
	}
	return fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}

func captureCleanupIdentity(repo gitRepository, source gitWorktree) (*cleanupIdentity, error) {
	if source.Path == repo.Root {
		return nil, fmt.Errorf("cannot capture main worktree for removal")
	}
	if err := repo.validateTree(source); err != nil {
		return nil, err
	}
	data, err := controlFile(filepath.Join(source.Path, ".git"), false)
	if err != nil {
		return nil, err
	}
	admin := strings.TrimSuffix(strings.TrimPrefix(string(data), "gitdir: "), "\n")
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(source.Path, admin)
	}
	saved := &cleanupIdentity{AdminPath: filepath.Clean(admin)}
	for _, entry := range []struct {
		path string
		dir  bool
		id   *fileIdentity
	}{
		{repo.Root, true, &saved.Root}, {repo.CommonDir, true, &saved.Common},
		{source.Path, true, &saved.Path}, {saved.AdminPath, true, &saved.Admin},
		{filepath.Join(source.Path, ".git"), false, &saved.Pointer},
		{filepath.Join(saved.AdminPath, "gitdir"), false, &saved.Backlink},
		{filepath.Join(saved.AdminPath, "commondir"), false, &saved.CommonPointer},
	} {
		id, err := identityAt(entry.path, entry.dir)
		if err != nil {
			return nil, err
		}
		*entry.id = id
	}
	return saved, nil
}

func verifyCleanupIdentity(w Workspace, saved *cleanupIdentity, missing bool) error {
	if saved == nil {
		if missing {
			return nil
		}
		return fmt.Errorf("cleanup has no captured worktree identity")
	}
	for _, entry := range []struct {
		path string
		dir  bool
		id   fileIdentity
		tree bool
	}{
		{w.Root, true, saved.Root, false}, {w.CommonDir, true, saved.Common, false},
		{w.Path, true, saved.Path, true}, {saved.AdminPath, true, saved.Admin, true},
		{filepath.Join(w.Path, ".git"), false, saved.Pointer, true},
		{filepath.Join(saved.AdminPath, "gitdir"), false, saved.Backlink, true},
		{filepath.Join(saved.AdminPath, "commondir"), false, saved.CommonPointer, true},
	} {
		if entry.tree && missing {
			continue
		}
		id, err := identityAt(entry.path, entry.dir)
		if err != nil {
			return err
		}
		if id != entry.id {
			return fmt.Errorf("captured cleanup identity changed: %s; replacement data was not removed", entry.path)
		}
	}
	return nil
}

func cleanupActive(job *cleanupJob) bool {
	return job != nil && (job.Status == "queued" || job.Status == "armed" || job.Status == "running") && time.Now().UnixNano() < job.Deadline
}

func verifyRemovalIdentity(state workspaceState) error {
	if recovery := state.Removal.Recovery; recovery != nil {
		return verifyRecovery(state.Workspace, recovery)
	}
	return verifyCleanupIdentity(state.Workspace, state.Removal.Identity, state.Removal.WorktreeRemoved)
}

func cleanupLogPath(store *stateStore, id, token string) string {
	return filepath.Join(store.dir, id+"."+token+".cleanup.log")
}

func openCleanupLog(store *stateStore, id, token string) (*os.File, error) {
	if !validID(id) || !validID(token) {
		return nil, fmt.Errorf("invalid cleanup diagnostic identity")
	}
	if err := privateDirectory(store.dir); err != nil {
		return nil, err
	}
	path := cleanupLogPath(store, id, token)
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_APPEND|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := privateFile(file, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func cleanupDiagnostic(file *os.File, phase string) error {
	// Only fixed phase names belong here, never subprocess output or hook/config contents.
	if file == nil {
		return nil
	}
	if _, err := fmt.Fprintln(file, phase); err != nil {
		return fmt.Errorf("write cleanup diagnostic: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync cleanup diagnostic: %w", err)
	}
	return nil
}

func (app *App) cleanup(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState, merged bool) (resultErr error) {
	if err := app.checkStandalone(ctx, state.Workspace); err != nil {
		return err
	}
	removal := state.Removal
	if err := verifyRemovalIdentity(*state); err != nil {
		return err
	}
	if err := app.checkCleanupWindow(ctx, *state); err != nil {
		return err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	token := hex.EncodeToString(random[:])
	var window CleanupWindow
	var err error
	if mux, ok := app.Mux.(interface {
		CaptureAll(context.Context, Workspace, string) (CleanupWindow, error)
	}); ok {
		window, err = mux.CaptureAll(ctx, state.Workspace, token)
	} else {
		window, err = app.Mux.Capture(ctx, state.Workspace, token)
	}
	if err != nil {
		return err
	}
	state.rememberServer(window)
	job := &cleanupJob{Token: token, Status: "queued", Deadline: time.Now().Add(cleanupLease).UnixNano(), Window: window}
	removal.Job = job
	if err := store.save(*state); err != nil {
		return err
	}
	log, err := openCleanupLog(store, state.ID, token)
	if err != nil {
		job.Status = "failed"
		if saveErr := store.save(*state); saveErr != nil {
			return errors.Join(err, fmt.Errorf("save failed cleanup checkpoint: %w", saveErr))
		}
		return err
	}
	var diagnosticErr error
	// Report the first diagnostic failure without cancelling durable cleanup work.
	diagnose := func(phase string) {
		if diagnosticErr == nil {
			diagnosticErr = cleanupDiagnostic(log, phase)
		}
	}
	defer func() {
		resultErr = errors.Join(resultErr, diagnosticErr)
		if err := log.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close cleanup diagnostic: %w", err))
		}
	}()
	diagnose("queued")
	fail := func(phase string, err error) error {
		job.Status = "failed"
		diagnose(phase)
		if saveErr := store.save(*state); saveErr != nil {
			err = errors.Join(err, fmt.Errorf("save failed cleanup checkpoint: %w", saveErr))
		}
		return fmt.Errorf("cleanup incomplete; retry remove; diagnostics: %s: %w", log.Name(), err)
	}
	if window.Caller && window.ID != "" {
		if app.Spawner == nil {
			return fail("spawn_failed", fmt.Errorf("cleanup spawner is not configured"))
		}
		launch := CleanupLaunch{Command: CleanupCommand{RepoID: state.RepoID, ID: state.ID, Token: token},
			Paths: CleanupPaths{HomeDir: app.HomeDir, StateDir: app.StateDir, ConfigDir: app.ConfigDir}, Root: repo.Root, Stderr: log}
		if err := app.Spawner.Start(ctx, launch); err != nil {
			return fail("handoff_failed", err)
		}
		message := "cleanup scheduled"
		if merged {
			message = "merge succeeded; cleanup scheduled"
		}
		if _, err := fmt.Fprintf(app.Stdout, "%s for %s; diagnostics: %s\n", message, state.Handle, log.Name()); err != nil {
			return fail("handoff_failed", err)
		}
		// A broken stdout may terminate this process with SIGPIPE instead of returning an error.
		job.Status = "armed"
		if err := store.save(*state); err != nil {
			return fail("handoff_failed", err)
		}
		diagnose("armed")
		app.cleanupScheduled = true
		return store.unlock()
	}
	job.Status = "running"
	if err := store.save(*state); err != nil {
		return fail("checkpoint_failed", err)
	}
	if phase, err := app.executeCleanup(ctx, g, repo, store, state); err != nil {
		return fail(phase, err)
	}
	diagnose("complete")
	return nil
}

func (app *App) checkCleanupWindow(ctx context.Context, state workspaceState) error {
	removal := state.Removal
	if removal != nil && removal.Job != nil && state.ServerPID == 0 && (removal.Job.Window.ID != "" || removal.Job.Window.Socket != "" || state.Window != "" || state.Socket != "") {
		if state.Socket == "" && removal.Job.Window.Socket != "" {
			return fmt.Errorf("legacy cleanup lacks a trusted tmux server identity; its captured socket must not be discarded")
		}
		if _, err := app.Mux.CapturedExists(ctx, state.Workspace, removal.Job.Window); err != nil {
			return fmt.Errorf("legacy cleanup lacks a trusted tmux server identity; restore the original connection or recover the record manually after confirming its window/server is closed: %w", err)
		}
	}
	return nil
}

func (app *App) windowGone(ctx context.Context, w Workspace, window CleanupWindow) error {
	timeout := cleanupWait
	if app.cleanupTimeout > 0 {
		timeout = app.cleanupTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		present, err := app.Mux.CapturedExists(ctx, w, window)
		if err != nil {
			return err
		}
		if !present {
			return nil
		}
		if err := cleanupPause(ctx, 25*time.Millisecond); err != nil {
			return fmt.Errorf("source tmux window did not disappear; worktree preserved: %w", err)
		}
	}
}

func cleanupPause(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (app *App) executeCleanup(ctx context.Context, g gitHost, repo gitRepository, store *stateStore, state *workspaceState) (string, error) {
	r := state.Removal
	if err := verifyRemovalIdentity(*state); err != nil {
		return "identity_failed", err
	}
	if r.Recovery != nil {
		if err := g.orphanStillMissing(ctx, repo, state.Workspace, r.Recovery); err != nil {
			return "identity_failed", err
		}
	}
	if workspaceHasSandbox(state.Workspace) && (r.Recovery == nil || expectedContainer(state.Workspace)) {
		if app.Sandbox == nil {
			return "sandbox_stop_failed", fmt.Errorf("saved sandbox implementation is unavailable")
		}
		if err := app.Sandbox.Stop(ctx, state.Workspace); err != nil {
			return "sandbox_stop_failed", err
		}
	}
	timeout := cleanupWait
	if app.cleanupTimeout > 0 {
		timeout = app.cleanupTimeout
	}
	windowCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := app.Mux.CloseCaptured(windowCtx, state.Workspace, r.Job.Window); err != nil {
		return "window_close_failed", err
	}
	if err := app.windowGone(windowCtx, state.Workspace, r.Job.Window); err != nil {
		return "window_wait_failed", err
	}
	r.Stopped = true
	if err := store.save(*state); err != nil {
		return "checkpoint_failed", err
	}
	if r.Recovery != nil {
		return app.finishOrphan(ctx, g, repo, store, state)
	}
	if err := verifyCleanupIdentity(state.Workspace, r.Identity, r.WorktreeRemoved); err != nil {
		return "identity_failed", err
	}
	if !r.WorktreeRemoved {
		source, err := g.source(ctx, repo, state.Workspace)
		if err != nil {
			return "source_validation_failed", err
		}
		if source.Head != r.Head {
			return "source_validation_failed", fmt.Errorf("source ref changed during cleanup; worktree and branch preserved")
		}
		if _, err := app.removalCheck(ctx, g, repo, *state, source, r.Force, r.KeepBranch); err != nil {
			return "worktree_validation_failed", err
		}
		if err := verifyCleanupIdentity(state.Workspace, r.Identity, false); err != nil {
			return "identity_failed", err
		}
		if present, err := app.Mux.CapturedExists(ctx, state.Workspace, r.Job.Window); err != nil || present {
			if err == nil {
				err = fmt.Errorf("source tmux window reappeared before removal")
			}
			return "window_wait_failed", err
		}
		r.WorktreeRemovalStarted = true
		if err := store.save(*state); err != nil {
			return "checkpoint_failed", err
		}
		args := []string{"worktree", "remove", "--force"}
		args = append(args, "--", state.Path)
		if _, err := g.run(ctx, repo.Root, args...); err != nil {
			return "worktree_remove_failed", err
		}
		r.WorktreeRemoved = true
		if err := store.save(*state); err != nil {
			return "checkpoint_failed", err
		}
	}
	if !r.BranchDeleted {
		if !r.KeepBranch {
			if err := app.deleteBranch(ctx, g, repo, *state); err != nil {
				return "branch_remove_failed", err
			}
		}
		r.BranchDeleted = true
		if err := store.save(*state); err != nil {
			return "checkpoint_failed", err
		}
	}
	if !r.ContainerRemoved && workspaceHasSandbox(state.Workspace) {
		if err := app.Sandbox.Remove(ctx, state.Workspace); err != nil {
			return "container_remove_failed", err
		}
	}
	r.ContainerRemoved = true
	r.Job.Status = "complete"
	completed := *state
	completed.Stage, completed.Config, completed.Container = "removed", Config{}, ""
	completed.SandboxUsed = false
	completed.OwnedIgnored = nil
	completed.PendingMergeCommit, completed.PendingMergeTarget = "", ""
	completed.PendingMergeHead, completed.PendingMergePath, completed.RetryMergeTarget = "", "", ""
	completed.PendingSquashTree = ""
	completed.PendingConflict = false
	if err := store.save(completed); err != nil {
		r.Job.Status = "running"
		return "checkpoint_failed", err
	}
	*state = completed
	return "complete", nil
}

func ParseCleanupCommand(args []string) (CleanupCommand, error) {
	if len(args) != 4 || args[0] != "_cleanup" || !validID(args[1]) || !validID(args[2]) || !validID(args[3]) {
		return CleanupCommand{}, fmt.Errorf("invalid private workmux cleanup invocation")
	}
	return CleanupCommand{RepoID: args[1], ID: args[2], Token: args[3]}, nil
}

type ExecCleanupSpawner struct{}

func (ExecCleanupSpawner) Start(ctx context.Context, launch CleanupLaunch) error {
	if _, err := ParseCleanupCommand([]string{"_cleanup", launch.Command.RepoID, launch.Command.ID, launch.Command.Token}); err != nil {
		return err
	}
	if err := validateCleanupPaths(launch.Paths); err != nil {
		return err
	}
	if !filepath.IsAbs(launch.Root) || launch.Stderr == nil {
		return fmt.Errorf("cleanup launch requires a surviving root and private diagnostics file")
	}
	if err := privateFile(launch.Stderr, launch.Stderr.Name()); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()
	inputRead, inputWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = inputRead.Close() }()
	defer func() { _ = inputWrite.Close() }()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = null.Close() }()
	cmd := exec.Command(executable, "workmux", "_cleanup", launch.Command.RepoID, launch.Command.ID, launch.Command.Token)
	cmd.Dir = launch.Root
	cmd.Env = append(os.Environ(), "CLI_WORKMUX_CLEANUP_PROTOCOL=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, null, launch.Stderr
	cmd.ExtraFiles = []*os.File{readyWrite, inputRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached cleanup: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	result := make(chan error, 1)
	go func() {
		if err := errors.Join(readyWrite.Close(), inputRead.Close()); err != nil {
			result <- err
			return
		}
		if err := json.NewEncoder(inputWrite).Encode(launch.Paths); err != nil {
			result <- err
			return
		}
		if err := inputWrite.Close(); err != nil {
			result <- err
			return
		}
		line, err := bufio.NewReader(io.LimitReader(readyRead, 80)).ReadString('\n')
		if err == nil && line != "ready "+launch.Command.Token+"\n" {
			err = fmt.Errorf("invalid cleanup readiness acknowledgement")
		}
		result <- err
	}()
	ctx, cancel := context.WithTimeout(ctx, cleanupWait)
	defer cancel()
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		// The failed helper may already have exited or closed its pipes.
		_ = cmd.Process.Kill()
		_ = inputWrite.Close()
		_ = readyRead.Close()
		<-done
		return fmt.Errorf("cleanup readiness handshake failed: %w", err)
	}
	return nil
}

func validateCleanupPaths(paths CleanupPaths) error {
	for _, path := range []string{paths.HomeDir, paths.StateDir, paths.ConfigDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") || len(path) > 2000 {
			return fmt.Errorf("invalid private cleanup paths")
		}
	}
	return nil
}

func RunCleanupProcess(ctx context.Context, command CleanupCommand, factory func(CleanupPaths) App) error {
	if _, err := ParseCleanupCommand([]string{"_cleanup", command.RepoID, command.ID, command.Token}); err != nil {
		return err
	}
	if factory == nil || os.Getenv("CLI_WORKMUX_CLEANUP_PROTOCOL") != "1" {
		return fmt.Errorf("private cleanup requires its parent handoff pipes")
	}
	for _, fd := range []int{3, 4} {
		var info syscall.Stat_t
		if err := syscall.Fstat(fd, &info); err != nil || info.Mode&syscall.S_IFMT != syscall.S_IFIFO {
			return fmt.Errorf("private cleanup requires its parent handoff pipes")
		}
	}
	ready := os.NewFile(3, "cleanup-ready")
	input := os.NewFile(4, "cleanup-input")
	defer func() { _ = ready.Close() }()
	defer func() { _ = input.Close() }()
	var paths CleanupPaths
	decoded := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(io.LimitReader(input, 8192))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&paths); err != nil {
			decoded <- err
			return
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			decoded <- fmt.Errorf("trailing handoff data")
			return
		}
		decoded <- nil
	}()
	readCtx, cancel := context.WithTimeout(ctx, cleanupWait)
	defer cancel()
	select {
	case err := <-decoded:
		if err != nil {
			return fmt.Errorf("invalid private cleanup handoff")
		}
	case <-readCtx.Done():
		return fmt.Errorf("private cleanup handoff timed out")
	}
	_ = input.Close()
	if err := validateCleanupPaths(paths); err != nil {
		return err
	}
	app := factory(paths)
	app.Stdout, app.Stderr, app.Stdin = io.Discard, io.Discard, nil
	if err := app.RunCleanup(ctx, command, ready); err != nil {
		return fmt.Errorf("workmux cleanup did not complete; inspect its private diagnostic file and retry remove")
	}
	return nil
}

func (app App) RunCleanup(ctx context.Context, command CleanupCommand, ready io.Writer) (resultErr error) {
	if _, err := ParseCleanupCommand([]string{"_cleanup", command.RepoID, command.ID, command.Token}); err != nil {
		return err
	}
	if ready == nil {
		return fmt.Errorf("cleanup requires a readiness channel")
	}
	if err := app.defaults(); err != nil {
		return err
	}
	dir := filepath.Join(app.StateDir, command.RepoID)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("private cleanup journal directory is missing: %w", err)
	}
	if err := privateDirectory(dir); err != nil {
		return err
	}
	state, err := readState(filepath.Join(dir, command.ID+".json"))
	if err != nil {
		return err
	}
	repo := gitRepository{Root: state.Root, CommonDir: state.CommonDir, Current: state.Root}
	store := &stateStore{dir: dir, repo: repo}
	if err := store.validate(state); err != nil {
		return err
	}
	if state.ID != command.ID || state.RepoID != command.RepoID || state.Removal == nil || state.Removal.Job == nil || state.Removal.Job.Token != command.Token {
		return fmt.Errorf("stale cleanup token")
	}
	log, err := openCleanupLog(store, command.ID, command.Token)
	if err != nil {
		return err
	}
	var diagnosticErr, readyErr error
	// Once ready, the parent may arm the job. Report I/O failures after finishing it.
	diagnose := func(phase string) {
		if diagnosticErr == nil {
			diagnosticErr = cleanupDiagnostic(log, phase)
		}
	}
	defer func() {
		resultErr = errors.Join(resultErr, diagnosticErr, readyErr)
		if err := log.Close(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close cleanup diagnostic: %w", err))
		}
	}()
	job := state.Removal.Job
	if job.Status != "queued" || !cleanupActive(job) {
		diagnose("stale_handoff")
		return fmt.Errorf("cleanup is not queued or its handoff expired")
	}
	if err := verifyRemovalIdentity(state); err != nil {
		diagnose("identity_failed")
		return err
	}
	if _, err := fmt.Fprintf(ready, "ready %s\n", command.Token); err != nil {
		diagnose("readiness_failed")
		return err
	}
	if closer, ok := ready.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			readyErr = fmt.Errorf("close cleanup readiness channel: %w", err)
		}
	}
	diagnose("worker_ready")
	deadline := time.Unix(0, job.Deadline)
	if maximum := time.Now().Add(cleanupLease); deadline.After(maximum) {
		deadline = maximum
	}
	handoffCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for {
		if err := handoffCtx.Err(); err != nil {
			diagnose("lock_timeout")
			return err
		}
		store, err = lockState(app.StateDir, repo)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			diagnose("lock_failed")
			return err
		}
		if err := cleanupPause(handoffCtx, 25*time.Millisecond); err != nil {
			diagnose("lock_timeout")
			return err
		}
	}
	defer func() {
		if err := store.unlock(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release repository lock: %w", err))
		}
	}()
	state, err = readState(filepath.Join(store.dir, command.ID+".json"))
	if err != nil {
		diagnose("state_failed")
		return err
	}
	if err := store.validate(state); err != nil {
		diagnose("state_failed")
		return err
	}
	if state.Removal == nil || state.Removal.Job == nil || state.Removal.Job.Token != command.Token || state.Removal.Job.Status != "armed" || !cleanupActive(state.Removal.Job) || handoffCtx.Err() != nil {
		diagnose("stale_handoff")
		return fmt.Errorf("cleanup handoff was cancelled, replaced, or expired")
	}
	job = state.Removal.Job
	job.Status = "running"
	if err := store.save(state); err != nil {
		diagnose("checkpoint_failed")
		return err
	}
	cancel()
	fail := func(phase string, err error) error {
		diagnose(phase)
		job.Status = "failed"
		if saveErr := store.save(state); saveErr != nil {
			return errors.Join(err, fmt.Errorf("save failed cleanup checkpoint: %w", saveErr))
		}
		return err
	}
	if err := verifyRemovalIdentity(state); err != nil {
		return fail("identity_failed", err)
	}
	g := gitHost{runner: app.Runner}
	found, err := g.discover(ctx, state.Root)
	if err != nil {
		return fail("repository_failed", err)
	}
	if found.Root != repo.Root || found.CommonDir != repo.CommonDir {
		return fail("repository_failed", fmt.Errorf("repository identity changed during handoff"))
	}
	if err := cleanupPause(ctx, cleanupDelay); err != nil {
		return fail("handoff_expired", err)
	}
	if phase, err := app.executeCleanup(ctx, g, found, store, &state); err != nil {
		return fail(phase, err)
	}
	diagnose("complete")
	return nil
}
