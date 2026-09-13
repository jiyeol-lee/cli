package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

type gitHost struct{ runner Runner }

type gitWorktree struct {
	Path, Head, Branch string
	Bare, Locked       bool
	Prunable           bool
}

type gitRepository struct {
	Root, CommonDir, Current string
	Worktrees                []gitWorktree
}

var gitProtection = []string{
	"core.fsmonitor=false", "core.hooksPath=/dev/null", "core.editor=false",
	"sequence.editor=false", "core.pager=cat", "core.askPass=false",
	"merge.autoStash=false",
	"commit.gpgSign=false", "tag.gpgSign=false", "gpg.program=/dev/null",
	"gpg.openpgp.program=/dev/null", "gpg.x509.program=/dev/null", "gpg.ssh.program=/dev/null",
	"diff.external=", "interactive.diffFilter=", "uploadpack.packObjectsHook=",
	"protocol.allow=never", "protocol.file.allow=always", "protocol.http.allow=always",
	"protocol.https.allow=always", "protocol.ssh.allow=always", "protocol.git.allow=always",
}

func (g gitHost) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return g.runInput(ctx, dir, nil, args...)
}

func (g gitHost) runInput(ctx context.Context, dir string, input io.Reader, args ...string) ([]byte, error) {
	protected := make([]string, 0, len(gitProtection)*2+len(args))
	for _, setting := range gitProtection {
		protected = append(protected, "-c", setting)
	}
	protected = append(protected, args...)
	return g.runner.Run(ctx, Process{
		Name: "git", Args: protected, Dir: dir, Stdin: input, CleanGitEnv: true,
		Env: []string{"LC_ALL=C", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=false", "SSH_ASKPASS=false",
			"GIT_EDITOR=false", "GIT_SEQUENCE_EDITOR=false", "GIT_PAGER=cat", "PAGER=cat", "GIT_MERGE_AUTOEDIT=no"},
	})
}

func (app *App) interactiveGit(ctx context.Context, dir string, args ...string) error {
	var protected []string
	for _, setting := range gitProtection {
		if strings.HasPrefix(setting, "core.editor=") || strings.HasPrefix(setting, "sequence.editor=") {
			continue
		}
		protected = append(protected, "-c", setting)
	}
	protected = append(protected, args...)
	env := []string{"LC_ALL=C", "WORKMUX_EMPTY_MERGE_OPTIONS="}
	for _, name := range []string{"GIT_EDITOR", "GIT_SEQUENCE_EDITOR"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	_, err := app.Runner.Run(ctx, Process{Name: "git", Args: protected, Dir: dir, CleanGitEnv: true,
		Env: env, Stdin: app.Stdin, Stdout: app.Stdout, Stderr: app.Stderr})
	return err
}

func (app *App) discardSource(ctx context.Context, g gitHost, repo gitRepository, w Workspace, source gitWorktree, beforeDiscard ...func() error) (bool, error) {
	guard := func() error {
		for _, check := range beforeDiscard {
			if err := check(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := g.clean(ctx, repo, source, true); err != nil {
		return false, err
	}
	dirty, err := g.run(ctx, source.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return false, err
	}
	if len(dirty) == 0 {
		return true, nil
	}
	saved, err := captureCleanupIdentity(repo, source)
	if err != nil {
		return false, err
	}
	snapshot, err := g.discardSnapshot(ctx, repo, source)
	if err != nil {
		return false, err
	}
	headFiles, err := g.run(ctx, source.Path, "ls-tree", "-r", "--name-only", "-z", source.Head)
	if err != nil {
		return false, err
	}
	for ignored := range snapshot.untracked {
		if slices.Contains(snapshot.approved, ignored) {
			continue
		}
		for tracked := range strings.SplitSeq(string(headFiles), "\x00") {
			if tracked != "" && (ignored == tracked || strings.HasPrefix(ignored, tracked+"/") || strings.HasPrefix(tracked, ignored+"/")) {
				return false, fmt.Errorf("originally ignored path %q could be overwritten by tracked discard; move it out first", ignored)
			}
		}
	}
	if err := guard(); err != nil {
		return false, err
	}
	approved, err := app.confirmDiscard(ctx, w.Branch)
	if err != nil || !approved {
		return false, err
	}
	checked, err := g.source(ctx, repo, w)
	if err != nil {
		return false, err
	}
	if checked.Head != source.Head {
		return false, fmt.Errorf("source ref changed during discard confirmation; work preserved")
	}
	if err := verifyCleanupIdentity(w, saved, false); err != nil {
		return false, err
	}
	if err := g.clean(ctx, repo, checked, true); err != nil {
		return false, err
	}
	current, err := g.discardSnapshot(ctx, repo, checked)
	if err != nil {
		return false, err
	}
	if current.digest != snapshot.digest {
		return false, fmt.Errorf("source contents changed during discard confirmation; work preserved")
	}
	tracked, err := g.run(ctx, source.Path, "diff", "--name-only", "-z", source.Head, "--")
	if err != nil {
		return false, err
	}
	staged, err := g.run(ctx, source.Path, "diff", "--cached", "--name-only", "-z", source.Head, "--")
	if err != nil {
		return false, err
	}
	if len(tracked) != 0 || len(staged) != 0 {
		if err := guard(); err != nil {
			return false, err
		}
		if _, err := g.run(ctx, source.Path, "restore", "--source="+source.Head, "--staged", "--worktree", "--", ":/"); err != nil {
			return false, fmt.Errorf("discard tracked changes: %w", err)
		}
	}
	if err := verifyCleanupIdentity(w, saved, false); err != nil {
		return false, err
	}
	checked, err = g.source(ctx, repo, w)
	if err != nil || checked.Head != source.Head {
		if err == nil {
			err = fmt.Errorf("source changed during discard")
		}
		return false, err
	}
	if err := g.clean(ctx, repo, checked, true); err != nil {
		return false, err
	}
	// Restoring .gitignore can expose previously ignored files. The same
	// confirmation covers them, but never files changed or created since it.
	remaining, err := g.untrackedSnapshot(ctx, source.Path)
	if err != nil {
		return false, err
	}
	expected := make(map[string][32]byte)
	for path, digest := range snapshot.untracked {
		restored := false
		if slices.Contains(snapshot.approved, path) {
			for tracked := range strings.SplitSeq(string(headFiles), "\x00") {
				if tracked != "" && (path == tracked || strings.HasPrefix(path, tracked+"/")) {
					restored = true
					break
				}
			}
		}
		if !restored {
			expected[path] = digest
		}
	}
	if !sameFileSnapshot(expected, remaining) {
		return false, fmt.Errorf("untracked contents changed during tracked discard; new work preserved")
	}
	if err := g.checkClean(ctx, repo, checked, false, false); err != nil {
		return false, err
	}
	untracked, err := g.run(ctx, source.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return false, err
	}
	paths := slices.Clone(snapshot.approved)
	for path := range strings.SplitSeq(string(untracked), "\x00") {
		if path == "" || slices.Contains(paths, path) {
			continue
		}
		if _, present := remaining[path]; !present {
			return false, fmt.Errorf("new untracked path %q appeared during discard; work preserved", path)
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		if _, present := remaining[path]; !present {
			continue
		}
		checked, err := g.source(ctx, repo, w)
		if err != nil {
			return false, err
		}
		if checked.Head != source.Head {
			return false, fmt.Errorf("source changed before untracked discard; work preserved")
		}
		if err := g.checkClean(ctx, repo, checked, false, false); err != nil {
			return false, err
		}
		current, err := g.untrackedSnapshot(ctx, source.Path)
		if err != nil {
			return false, err
		}
		if !sameFileSnapshot(remaining, current) {
			return false, fmt.Errorf("untracked contents changed before discard; new work preserved")
		}
		if err := guard(); err != nil {
			return false, err
		}
		if _, err := g.run(ctx, source.Path, "clean", "-f", "-x", "--", ":(literal)"+path); err != nil {
			return false, fmt.Errorf("discard untracked file: %w", err)
		}
		delete(remaining, path)
	}
	checked, err = g.source(ctx, repo, w)
	if err != nil {
		return false, err
	}
	if checked.Head != source.Head {
		return false, fmt.Errorf("source changed during discard")
	}
	return true, g.clean(ctx, repo, checked, false)
}

type discardSnapshot struct {
	digest    [32]byte
	untracked map[string][32]byte
	approved  []string
}

func sameFileSnapshot(a, b map[string][32]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for path, digest := range a {
		if current, ok := b[path]; !ok || digest != current {
			return false
		}
	}
	return true
}

func (g gitHost) untrackedSnapshot(ctx context.Context, path string) (map[string][32]byte, error) {
	out, err := g.run(ctx, path, "ls-files", "--others", "-z")
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	files := make(map[string][32]byte)
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name == "" {
			continue
		}
		name = strings.TrimSuffix(name, "/")
		if !safeRelative(name) {
			return nil, fmt.Errorf("unsafe untracked path returned by Git")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return nil, err
		}
		var contents []byte
		switch {
		case info.Mode().IsRegular():
			file, err := root.Open(name)
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if err := errors.Join(copyErr, closeErr); err != nil {
				return nil, err
			}
			contents = hash.Sum(nil)
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(name)
			if err != nil {
				return nil, err
			}
			contents = []byte(target)
		default:
			return nil, fmt.Errorf("cannot authorize discard of nonregular untracked path %q; move it out first", name)
		}
		files[name] = sha256.Sum256(append([]byte(fmt.Sprint(info.Mode())+"\x00"), contents...))
	}
	return files, nil
}

func (g gitHost) discardSnapshot(ctx context.Context, repo gitRepository, source gitWorktree) (discardSnapshot, error) {
	var snapshot discardSnapshot
	hash := sha256.New()
	for _, args := range [][]string{
		{"status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none"},
		{"diff", "--binary", "--no-ext-diff", "--no-textconv", source.Head, "--"},
		{"diff", "--cached", "--binary", "--no-ext-diff", "--no-textconv", source.Head, "--"},
	} {
		out, err := g.run(ctx, source.Path, args...)
		if err != nil {
			return snapshot, err
		}
		_, _ = fmt.Fprintf(hash, "%d\x00", len(out))
		_, _ = hash.Write(out)
	}
	admin, err := repo.treeAdmin(source)
	if err != nil {
		return snapshot, err
	}
	index, err := indexDigest(admin)
	if err != nil {
		return snapshot, err
	}
	_, _ = hash.Write(index[:])
	tracked, err := g.run(ctx, source.Path, "ls-files", "--cached", "-z")
	if err != nil {
		return snapshot, err
	}
	root, err := os.OpenRoot(source.Path)
	if err != nil {
		return snapshot, err
	}
	defer func() { _ = root.Close() }()
	for name := range strings.SplitSeq(string(tracked), "\x00") {
		if name == "" {
			continue
		}
		if !safeRelative(name) {
			return snapshot, fmt.Errorf("unsafe tracked path returned by Git")
		}
		digest, err := trackedFileDigest(root, name)
		if err != nil {
			return snapshot, err
		}
		_, _ = io.WriteString(hash, name+"\x00")
		_, _ = hash.Write(digest[:])
	}
	snapshot.untracked, err = g.untrackedSnapshot(ctx, source.Path)
	if err != nil {
		return snapshot, err
	}
	var names []string
	for name := range snapshot.untracked {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		_, _ = io.WriteString(hash, name+"\x00")
		digest := snapshot.untracked[name]
		_, _ = hash.Write(digest[:])
	}
	out, err := g.run(ctx, source.Path, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return snapshot, err
	}
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			if !safeRelative(name) {
				return snapshot, fmt.Errorf("unsafe discard path returned by Git")
			}
			snapshot.approved = append(snapshot.approved, name)
		}
	}
	_, _ = io.WriteString(hash, "approved\x00")
	_, _ = hash.Write(out)
	copy(snapshot.digest[:], hash.Sum(nil))
	return snapshot, nil
}

func indexDigest(admin string) ([32]byte, error) {
	var digest [32]byte
	root, err := os.OpenRoot(admin)
	if err != nil {
		return digest, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.OpenFile("index", syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return digest, nil
	}
	if err != nil {
		return digest, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return digest, err
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return digest, fmt.Errorf("refuse nonregular or hardlinked source index")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return digest, err
	}
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func trackedFileDigest(root *os.Root, name string) ([32]byte, error) {
	hash := sha256.New()
	err := fs.WalkDir(root.FS(), name, func(path string, _ fs.DirEntry, err error) error {
		if os.IsNotExist(err) && path == name {
			_, _ = io.WriteString(hash, "missing\x00")
			return nil
		}
		if err != nil {
			return err
		}
		info, err := root.Lstat(path)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%d\x00", path, info.Mode(), info.Size())
		switch {
		case info.IsDir():
			return nil
		case info.Mode().IsRegular():
			file, err := root.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(hash, file)
			return errors.Join(copyErr, file.Close())
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(hash, target)
			return nil
		default:
			return fmt.Errorf("cannot authorize discard of nonregular tracked path %q", path)
		}
	})
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest, err
}

func (g gitHost) discover(ctx context.Context, cwd string) (gitRepository, error) {
	var repo gitRepository
	root, err := g.run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		bare, bareErr := g.run(ctx, cwd, "rev-parse", "--is-bare-repository")
		if bareErr != nil || strings.TrimSpace(string(bare)) != "true" {
			return repo, fmt.Errorf("find repository: %w", err)
		}
		root, err = g.run(ctx, cwd, "rev-parse", "--absolute-git-dir")
		if err != nil {
			return repo, err
		}
	}
	repo.Current, err = canonicalDirectory(strings.TrimSuffix(string(root), "\n"))
	if err != nil {
		return repo, err
	}
	super, err := g.run(ctx, repo.Current, "rev-parse", "--show-superproject-working-tree")
	if err != nil {
		return repo, err
	}
	if len(bytes.TrimSpace(super)) != 0 {
		return repo, fmt.Errorf("submodule repositories are not supported")
	}
	common, err := g.run(ctx, repo.Current, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repo, err
	}
	repo.CommonDir, err = canonicalDirectory(strings.TrimSuffix(string(common), "\n"))
	if err != nil {
		return repo, err
	}
	if err := checkGitDirectory(repo.CommonDir); err != nil {
		return repo, err
	}
	repo.Worktrees, err = g.worktrees(ctx, repo.Current)
	if err != nil {
		return repo, err
	}
	if len(repo.Worktrees) == 0 {
		return repo, fmt.Errorf("repository has no worktree records")
	}
	repo.Root = repo.Worktrees[0].Path
	if repo.Root == repo.CommonDir && !repo.Worktrees[0].Bare && repo.Current != repo.Root {
		return repo, fmt.Errorf("cannot locate the main worktree of a separate Git directory from this linked worktree; run from the main worktree or configure Git's core.worktree")
	}
	if err := repo.validateMain(); err != nil {
		return repo, err
	}
	found := false
	for _, tree := range repo.Worktrees {
		if tree.Path == repo.Current {
			found = true
			if tree.Bare {
				continue
			}
			if err := repo.validateTree(tree); err != nil {
				return repo, err
			}
		}
	}
	if !found {
		return repo, fmt.Errorf("current directory is not a registered worktree")
	}
	return repo, nil
}

func (g gitHost) worktrees(ctx context.Context, dir string) ([]gitWorktree, error) {
	out, err := g.run(ctx, dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}
	trees, err := parseWorktrees(out)
	if err != nil {
		return nil, err
	}
	if len(trees) > 0 && !trees[0].Bare {
		if data, err := controlFile(filepath.Join(dir, ".git"), true); err == nil && len(data) > 0 {
			pointer, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), "gitdir: ")
			if ok && !strings.ContainsAny(pointer, "\x00\r\n") {
				if !filepath.IsAbs(pointer) {
					pointer = filepath.Join(dir, pointer)
				}
				if filepath.Clean(pointer) == trees[0].Path {
					trees[0].Path = dir
				}
			}
		}
	}
	return trees, nil
}

func parseWorktrees(data []byte) ([]gitWorktree, error) {
	var trees []gitWorktree
	var tree *gitWorktree
	seen := make(map[string]bool)
	if len(data) != 0 && data[len(data)-1] != 0 {
		return nil, fmt.Errorf("invalid NUL-delimited worktree list")
	}
	for field := range strings.SplitSeq(string(data), "\x00") {
		key, value, _ := strings.Cut(field, " ")
		switch key {
		case "":
			tree = nil
		case "worktree":
			if tree != nil || !filepath.IsAbs(value) || filepath.Clean(value) != value || seen[value] {
				return nil, fmt.Errorf("invalid worktree path in Git output")
			}
			seen[value] = true
			trees = append(trees, gitWorktree{Path: value})
			tree = &trees[len(trees)-1]
		default:
			if tree == nil {
				return nil, fmt.Errorf("invalid worktree record")
			}
			switch key {
			case "HEAD":
				tree.Head = value
			case "branch":
				if !strings.HasPrefix(value, "refs/heads/") {
					return nil, fmt.Errorf("invalid worktree branch")
				}
				tree.Branch = strings.TrimPrefix(value, "refs/heads/")
			case "bare":
				tree.Bare = true
			case "locked":
				tree.Locked = true
			case "prunable":
				tree.Prunable = true
			}
		}
	}
	return trees, nil
}

func canonicalDirectory(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("expected an absolute directory without control characters")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve directory %s: %w", path, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", path)
	}
	return filepath.Clean(real), nil
}

func noSymlinkPath(path string) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if real != filepath.Clean(path) {
		return fmt.Errorf("refuse symlink in control path %s", path)
	}
	return nil
}

func controlFile(path string, optional bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if optional && os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect Git control file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, fmt.Errorf("refuse symlink, hardlink, or nonregular Git control file %s", path)
	}
	if err := noSymlinkPath(path); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) || opened.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, fmt.Errorf("changed Git control file during validation: %s", path)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("oversized Git pointer file: %s", path)
	}
	return io.ReadAll(io.LimitReader(file, (1<<20)+1))
}

func checkGitDirectory(path string) error {
	if err := noSymlinkPath(path); err != nil {
		return err
	}
	for _, name := range []string{"HEAD", "config", "config.worktree", "index", "packed-refs", "shallow"} {
		p := filepath.Join(path, name)
		info, err := os.Lstat(p)
		if os.IsNotExist(err) && name != "HEAD" {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return fmt.Errorf("refuse symlink, hardlink, or nonregular Git control file %s", p)
		}
	}
	for _, name := range []string{"refs", "objects", "worktrees"} {
		p := filepath.Join(path, name)
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := noSymlinkPath(p); err != nil {
			return err
		}
	}
	return nil
}

func (repo gitRepository) validateMain() error {
	root, err := canonicalDirectory(repo.Root)
	if err != nil {
		return err
	}
	if root != repo.Root {
		return fmt.Errorf("main repository path is not canonical")
	}
	if root != repo.CommonDir && filepath.Join(root, ".git") != repo.CommonDir {
		data, err := controlFile(filepath.Join(root, ".git"), false)
		if err != nil {
			return err
		}
		pointer, ok := strings.CutPrefix(strings.TrimSuffix(string(data), "\n"), "gitdir: ")
		if !ok || strings.ContainsAny(pointer, "\x00\r\n") {
			return fmt.Errorf("invalid main Git directory pointer")
		}
		if !filepath.IsAbs(pointer) {
			pointer = filepath.Join(root, pointer)
		}
		if filepath.Clean(pointer) != repo.CommonDir {
			return fmt.Errorf("main Git directory pointer changed")
		}
	}
	return checkGitDirectory(repo.CommonDir)
}

func (repo gitRepository) validateTree(tree gitWorktree) error {
	if tree.Bare || tree.Prunable {
		return fmt.Errorf("worktree is bare or prunable: %s", tree.Path)
	}
	if err := repo.validateMain(); err != nil {
		return err
	}
	if tree.Path == repo.Root {
		return nil
	}
	if err := noSymlinkPath(tree.Path); err != nil {
		return err
	}
	pointer, err := controlFile(filepath.Join(tree.Path, ".git"), false)
	if err != nil {
		return err
	}
	line := strings.TrimSuffix(string(pointer), "\n")
	if !strings.HasPrefix(line, "gitdir: ") || strings.ContainsAny(line, "\x00\r\n") {
		return fmt.Errorf("invalid linked worktree .git pointer")
	}
	admin := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(tree.Path, admin)
	}
	admin = filepath.Clean(admin)
	if filepath.Dir(admin) != filepath.Join(repo.CommonDir, "worktrees") {
		return fmt.Errorf("linked worktree points outside repository administration directory")
	}
	if err := checkGitDirectory(admin); err != nil {
		return err
	}
	for name, expected := range map[string]string{"commondir": repo.CommonDir, "gitdir": filepath.Join(tree.Path, ".git")} {
		data, err := controlFile(filepath.Join(admin, name), false)
		if err != nil {
			return err
		}
		value := strings.TrimSuffix(string(data), "\n")
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid worktree %s pointer", name)
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(admin, value)
		}
		if filepath.Clean(value) != expected {
			return fmt.Errorf("worktree %s pointer does not match repository", name)
		}
	}
	return nil
}

func (g gitHost) branchName(ctx context.Context, dir, name string) (string, error) {
	if err := safeName(name); err != nil {
		return "", err
	}
	out, err := g.run(ctx, dir, "check-ref-format", "--branch", name)
	if err != nil {
		return "", fmt.Errorf("invalid branch %q: %w", name, err)
	}
	if strings.TrimSuffix(string(out), "\n") != name {
		return "", fmt.Errorf("branch shorthand is not allowed: %q", name)
	}
	return workspaceSlug(name), nil
}

func safeName(name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "@{") {
		return fmt.Errorf("invalid branch or workspace name %q", name)
	}
	return nil
}

func safeComparisonRef(ref, source string) error {
	if err := safeName(ref); err != nil {
		return err
	}
	if strings.TrimPrefix(ref, "refs/heads/") == source {
		return fmt.Errorf("source cannot be its own comparison ref")
	}
	return nil
}

func gitExit(err error, code int) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == code
}

func (g gitHost) localBase(ctx context.Context, repo gitRepository, ref, source string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if err := safeComparisonRef(ref, source); err != nil {
		return "", err
	}
	branch, full := strings.CutPrefix(ref, "refs/heads/")
	if !full {
		branch = ref
		if strings.HasPrefix(ref, "refs/") || ref == "HEAD" {
			return "", nil
		}
	}
	if _, err := g.run(ctx, repo.Root, "check-ref-format", "refs/heads/"+branch); err != nil {
		if gitExit(err, 1) {
			return "", nil
		}
		return "", err
	}
	oid, err := g.branchOID(ctx, repo, branch)
	if err != nil || oid == "" {
		return "", err
	}
	return branch, nil
}

func validOID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func (g gitHost) resolve(ctx context.Context, dir, ref string) (string, error) {
	if err := safeName(ref); err != nil {
		return "", err
	}
	out, err := g.run(ctx, dir, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve commit %q: %w", ref, err)
	}
	oid := strings.TrimSpace(string(out))
	if !validOID(oid) {
		return "", fmt.Errorf("invalid commit ID returned by Git")
	}
	return oid, nil
}

type gitComparison struct {
	Ref, Commit, RefOID string
}

func (g gitHost) comparison(ctx context.Context, dir, ref string) (gitComparison, error) {
	if err := safeName(ref); err != nil {
		return gitComparison{}, err
	}
	out, err := g.run(ctx, dir, "rev-parse", "--symbolic-full-name", "--verify", "--end-of-options", ref)
	if err != nil {
		return gitComparison{}, err
	}
	full := strings.TrimSuffix(string(out), "\n")
	if full == "" || full == "HEAD" && ref == "HEAD" {
		// Revision expressions and object IDs are pinned to immutable commits.
		// A named ref must never silently become an object-only comparison.
		objectPrefix := len(ref) >= 4 && len(ref) <= 64 && strings.Trim(ref, "0123456789abcdefABCDEF") == ""
		if _, err := g.run(ctx, dir, "check-ref-format", "refs/heads/"+ref); err == nil && !objectPrefix && ref != "HEAD" {
			return gitComparison{}, fmt.Errorf("comparison ref %q is ambiguous or not a direct ref", ref)
		} else if err != nil && !gitExit(err, 1) {
			return gitComparison{}, err
		}
		oid, err := g.resolve(ctx, dir, ref)
		return gitComparison{Ref: oid, Commit: oid}, err
	}
	if !strings.HasPrefix(full, "refs/") || safeName(full) != nil {
		return gitComparison{}, fmt.Errorf("invalid full comparison ref returned by Git")
	}
	if _, err := g.run(ctx, dir, "check-ref-format", full); err != nil {
		return gitComparison{}, err
	}
	out, err = g.run(ctx, dir, "rev-parse", "--verify", "--end-of-options", full)
	if err != nil {
		return gitComparison{}, err
	}
	raw := strings.TrimSpace(string(out))
	if !validOID(raw) {
		return gitComparison{}, fmt.Errorf("invalid comparison ref object ID")
	}
	commit, err := g.resolve(ctx, dir, raw)
	return gitComparison{Ref: full, Commit: commit, RefOID: raw}, err
}

func (g gitHost) branchOID(ctx context.Context, repo gitRepository, branch string) (string, error) {
	if _, err := g.branchName(ctx, repo.Root, branch); err != nil {
		return "", err
	}
	ref := "refs/heads/" + branch
	path := filepath.Join(repo.CommonDir, filepath.FromSlash(ref))
	parent := filepath.Dir(path)
	for {
		if _, err := os.Lstat(parent); os.IsNotExist(err) {
			parent = filepath.Dir(parent)
			continue
		} else if err != nil {
			return "", err
		}
		if err := noSymlinkPath(parent); err != nil {
			return "", err
		}
		break
	}
	if _, err := controlFile(path, true); err != nil {
		return "", err
	}
	out, err := g.run(ctx, repo.Root, "for-each-ref", "--format=%(refname)%00%(objectname)", ref)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		name, oid, ok := strings.Cut(line, "\x00")
		if ok && name == ref {
			if !validOID(oid) {
				return "", fmt.Errorf("invalid branch commit ID")
			}
			return oid, nil
		}
	}
	return "", nil
}

func (g gitHost) source(ctx context.Context, repo gitRepository, w Workspace) (gitWorktree, error) {
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return gitWorktree{}, err
	}
	for _, tree := range trees {
		if tree.Path != w.Path {
			continue
		}
		if tree.Path == repo.Root || tree.Branch != w.Branch || tree.Locked {
			return tree, fmt.Errorf("managed worktree is main, locked, or has changed branch")
		}
		if err := repo.validateTree(tree); err != nil {
			return tree, err
		}
		oid, err := g.branchOID(ctx, repo, w.Branch)
		if err != nil {
			return tree, err
		}
		if oid == "" || oid != tree.Head {
			return tree, fmt.Errorf("worktree HEAD no longer matches its branch")
		}
		return tree, nil
	}
	return gitWorktree{}, fmt.Errorf("managed worktree is missing: %s; use remove to recover partial cleanup", w.Path)
}

func (g gitHost) clean(ctx context.Context, repo gitRepository, tree gitWorktree, allowDirty bool) error {
	return g.checkClean(ctx, repo, tree, allowDirty, true)
}

func (repo gitRepository) treeAdmin(tree gitWorktree) (string, error) {
	if err := repo.validateTree(tree); err != nil {
		return "", err
	}
	admin := repo.CommonDir
	if tree.Path != repo.Root {
		data, err := controlFile(filepath.Join(tree.Path, ".git"), false)
		if err != nil {
			return "", err
		}
		admin = strings.TrimSuffix(strings.TrimPrefix(string(data), "gitdir: "), "\n")
		if !filepath.IsAbs(admin) {
			admin = filepath.Join(tree.Path, admin)
		}
	}
	return admin, nil
}

func (g gitHost) checkClean(ctx context.Context, repo gitRepository, tree gitWorktree, allowDirty, untracked bool) error {
	if tree.Locked {
		return fmt.Errorf("worktree is locked: %s", tree.Path)
	}
	admin, err := repo.treeAdmin(tree)
	if err != nil {
		return err
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_LOG", "index.lock", "HEAD.lock"} {
		path := filepath.Join(admin, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("unfinished Git operation in %s (%s)", tree.Path, name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	for _, name := range []string{"HEAD.lock", "config.lock", "packed-refs.lock", "shallow.lock", filepath.Join("refs", "heads", tree.Branch+".lock")} {
		if _, err := os.Lstat(filepath.Join(repo.CommonDir, name)); err == nil {
			return fmt.Errorf("Git metadata is locked (%s)", name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	entries, err := g.run(ctx, tree.Path, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(string(entries), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 3 || entry[1] != ' ' {
			return fmt.Errorf("invalid index entry returned by Git")
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			return fmt.Errorf("index entry %q uses assume-unchanged or skip-worktree; inspect its hidden edits and clear the flag manually before retrying", entry[2:])
		}
	}
	files := "--untracked-files=no"
	if untracked {
		files = "--untracked-files=all"
	}
	out, err := g.run(ctx, tree.Path, "status", "--porcelain=v1", "-z", files, "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if len(out) != 0 && !allowDirty {
		return fmt.Errorf("worktree is dirty: %s; changes appeared after confirmation or preflight; work preserved, retry after inspecting them", tree.Path)
	}
	return nil
}

func (g gitHost) targetBranch(ctx context.Context, repo gitRepository, into, source string) (gitWorktree, error) {
	if into == "" {
		out, err := g.run(ctx, repo.Root, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
		if err == nil {
			ref := strings.TrimSpace(string(out))
			if candidate, ok := strings.CutPrefix(ref, "refs/remotes/origin/"); ok {
				oid, err := g.branchOID(ctx, repo, candidate)
				if err != nil {
					return gitWorktree{}, err
				}
				if oid != "" && candidate != source {
					into = candidate
				}
			}
		} else if !gitExit(err, 1) {
			return gitWorktree{}, err
		}
		for _, candidate := range []string{"main", "master"} {
			if into != "" {
				break
			}
			oid, err := g.branchOID(ctx, repo, candidate)
			if err != nil {
				return gitWorktree{}, err
			}
			if oid != "" && candidate != source {
				into = candidate
			}
		}
	}
	if into == "" || into == source {
		return gitWorktree{}, fmt.Errorf("cannot detect a different local merge target; merge with --into")
	}
	oid, err := g.branchOID(ctx, repo, into)
	if err != nil {
		return gitWorktree{}, err
	}
	if oid == "" {
		return gitWorktree{}, fmt.Errorf("target branch %q does not exist locally", into)
	}
	return gitWorktree{Branch: into, Head: oid}, nil
}

func (g gitHost) target(ctx context.Context, repo gitRepository, into, source string, checkedOut bool) (gitWorktree, error) {
	target, err := g.targetBranch(ctx, repo, into, source)
	if err != nil {
		return target, err
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return gitWorktree{}, err
	}
	var found *gitWorktree
	for _, tree := range trees {
		if tree.Branch == target.Branch {
			if found != nil {
				return gitWorktree{}, fmt.Errorf("target branch is checked out more than once")
			}
			if tree.Locked {
				return tree, fmt.Errorf("target worktree is locked")
			}
			if err := repo.validateTree(tree); err != nil {
				return tree, err
			}
			if tree.Head != target.Head {
				return tree, fmt.Errorf("target branch changed while inspecting it")
			}
			found = &tree
		}
	}
	if found != nil {
		return *found, nil
	}
	if checkedOut {
		return gitWorktree{}, fmt.Errorf("target branch %q must be checked out in another worktree", target.Branch)
	}
	return target, nil
}

func (g gitHost) ancestor(ctx context.Context, repo gitRepository, source, target string) error {
	merged, err := g.isAncestor(ctx, repo, source, target)
	if err != nil {
		return err
	}
	if !merged {
		return fmt.Errorf("source is not safely merged into target")
	}
	return nil
}

func (g gitHost) placeTarget(ctx context.Context, repo gitRepository, target gitWorktree, source string, beforeSwitch ...func() error) (gitWorktree, error) {
	if target.Path != "" {
		return target, nil
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return target, err
	}
	if len(trees) == 0 || trees[0].Path != repo.Root || trees[0].Branch == source {
		return target, fmt.Errorf("main worktree identity changed or contains the source branch")
	}
	main := trees[0]
	if err := g.checkClean(ctx, repo, main, false, false); err != nil {
		return target, err
	}
	rootID, err := identityAt(repo.Root, true)
	if err != nil {
		return target, err
	}
	commonID, err := identityAt(repo.CommonDir, true)
	if err != nil {
		return target, err
	}
	if err := g.protectIgnored(ctx, main, target.Head); err != nil {
		return target, err
	}
	checked, err := g.target(ctx, repo, target.Branch, source, false)
	if err != nil {
		return target, err
	}
	if checked.Head != target.Head || checked.Path != "" {
		return target, fmt.Errorf("target changed before switching main worktree")
	}
	for _, check := range beforeSwitch {
		if err := check(); err != nil {
			return target, err
		}
	}
	if err := g.checkClean(ctx, repo, main, false, false); err != nil {
		return target, err
	}
	if _, err := g.run(ctx, repo.Root, "switch", "--no-guess", "--no-overwrite-ignore", "--", target.Branch); err != nil {
		return target, fmt.Errorf("switch main worktree to %q: %w", target.Branch, err)
	}
	for path, expected := range map[string]fileIdentity{repo.Root: rootID, repo.CommonDir: commonID} {
		current, err := identityAt(path, true)
		if err != nil {
			return target, err
		}
		if current != expected {
			return target, fmt.Errorf("repository identity changed during switch")
		}
	}
	checked, err = g.target(ctx, repo, target.Branch, source, true)
	if err != nil {
		return target, err
	}
	if checked.Head != target.Head || checked.Path != repo.Root {
		return target, fmt.Errorf("target changed during switch")
	}
	return checked, nil
}

// Preflight gives path diagnostics; the native Git flag protects the later mutation.
func (g gitHost) protectIgnored(ctx context.Context, tree gitWorktree, next string) error {
	if !validOID(tree.Head) || !validOID(next) {
		return fmt.Errorf("invalid commit for ignored-file check")
	}
	changed, err := g.run(ctx, tree.Path, "diff", "--name-only", "--no-renames", "-z", tree.Head, next, "--")
	if err != nil {
		return err
	}
	ignored, err := g.run(ctx, tree.Path, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil {
		return err
	}
	for path := range strings.SplitSeq(string(ignored), "\x00") {
		path = strings.TrimSuffix(path, "/")
		if path == "" {
			continue
		}
		if !safeRelative(path) {
			return fmt.Errorf("unsafe ignored path returned by Git")
		}
		for change := range strings.SplitSeq(string(changed), "\x00") {
			if change != "" && (path == change || strings.HasPrefix(path, change+"/") || strings.HasPrefix(change, path+"/")) {
				return fmt.Errorf("ignored file or directory %q could be overwritten in %s; move it out first", path, tree.Path)
			}
		}
	}
	return nil
}

func (g gitHost) fastForward(ctx context.Context, repo gitRepository, source, target gitWorktree) (bool, error) {
	included, err := g.isAncestor(ctx, repo, source.Head, target.Head)
	if err != nil || included {
		return included, err
	}
	forward, err := g.isAncestor(ctx, repo, target.Head, source.Head)
	if err != nil {
		return false, err
	}
	if !forward {
		return false, fmt.Errorf("source %q and target %q have diverged; manually rebase %q onto %q, then rerun merge", source.Branch, target.Branch, source.Branch, target.Branch)
	}
	return false, nil
}

func (g gitHost) completedMergeAttempt(ctx context.Context, repo gitRepository, state workspaceState) (gitWorktree, error) {
	if state.PendingMergeTarget == "" || state.PendingMergeHead == "" {
		return gitWorktree{}, fmt.Errorf("previous merge lacks a complete source/target checkpoint")
	}
	includedBefore, err := g.isAncestor(ctx, repo, state.PendingMergeCommit, state.PendingMergeHead)
	if err != nil || includedBefore {
		// Older no-op checkpoints cannot prove that Git performed integration.
		return gitWorktree{}, err
	}
	target, err := g.target(ctx, repo, state.PendingMergeTarget, state.Branch, false)
	if err != nil || target.Head == state.PendingMergeHead {
		return gitWorktree{}, err
	}
	included, err := g.isAncestor(ctx, repo, state.PendingMergeCommit, target.Head)
	if err != nil {
		return gitWorktree{}, err
	}
	if included {
		if err := g.ancestor(ctx, repo, state.PendingMergeHead, target.Head); err != nil {
			return gitWorktree{}, err
		}
		return target, nil
	}
	if state.MergeStrategy == "squash" || state.MergeStrategy == "rebase" {
		// Manually resolved legacy operations need not contain the original source.
		return gitWorktree{}, nil
	}
	return gitWorktree{}, fmt.Errorf("previous merge target changed without a verified integration")
}

func captureGitIdentity(repo gitRepository, tree gitWorktree) (map[string]fileIdentity, error) {
	admin, err := repo.treeAdmin(tree)
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{repo.Root: true, repo.CommonDir: true, tree.Path: true, admin: true}
	if tree.Path != repo.Root {
		paths[filepath.Join(tree.Path, ".git")] = false
		paths[filepath.Join(admin, "gitdir")] = false
		paths[filepath.Join(admin, "commondir")] = false
	}
	if repo.Root != repo.CommonDir && filepath.Join(repo.Root, ".git") != repo.CommonDir {
		paths[filepath.Join(repo.Root, ".git")] = false
	}
	saved := make(map[string]fileIdentity, len(paths))
	for path, directory := range paths {
		id, err := identityAt(path, directory)
		if err != nil {
			return nil, err
		}
		saved[path] = id
	}
	return saved, nil
}

func verifyGitIdentity(saved map[string]fileIdentity) error {
	for path, expected := range saved {
		if err := noSymlinkPath(path); err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if (fileIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}) != expected {
			return fmt.Errorf("Git worktree identity changed: %s; work preserved", path)
		}
	}
	return nil
}

func (g gitHost) isAncestor(ctx context.Context, repo gitRepository, source, target string) (bool, error) {
	if !validOID(source) || !validOID(target) {
		return false, fmt.Errorf("invalid ancestry commit ID")
	}
	if _, err := g.run(ctx, repo.Root, "merge-base", "--is-ancestor", source, target); err != nil {
		if gitExit(err, 1) {
			return false, nil
		}
		return false, fmt.Errorf("check ancestry: %w", err)
	}
	return true, nil
}

func identity(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func safeRelative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, "../") && !strings.ContainsAny(path, "\x00\r\n")
}
