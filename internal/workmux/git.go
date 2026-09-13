package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	protected := make([]string, 0, len(gitProtection)*2+len(args))
	for _, setting := range gitProtection {
		protected = append(protected, "-c", setting)
	}
	protected = append(protected, args...)
	return g.runner.Run(ctx, Process{
		Name: "git", Args: protected, Dir: dir, CleanGitEnv: true,
		Env: []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=false", "SSH_ASKPASS=false",
			"GIT_EDITOR=false", "GIT_SEQUENCE_EDITOR=false", "GIT_PAGER=cat", "PAGER=cat", "GIT_MERGE_AUTOEDIT=no"},
	})
}

func (g gitHost) discover(ctx context.Context, cwd string) (gitRepository, error) {
	var repo gitRepository
	root, err := g.run(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return repo, fmt.Errorf("find non-bare repository: %w", err)
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
	if len(repo.Worktrees) == 0 || repo.Worktrees[0].Bare {
		return repo, fmt.Errorf("bare repositories are not supported")
	}
	repo.Root = repo.Worktrees[0].Path
	if err := repo.validateMain(); err != nil {
		return repo, err
	}
	found := false
	for _, tree := range repo.Worktrees {
		if tree.Path == repo.Current {
			found = true
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
	return parseWorktrees(out)
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
	if root != repo.Root || filepath.Join(root, ".git") != repo.CommonDir {
		return fmt.Errorf("main worktree must own its canonical .git directory; separate Git directories are not supported")
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
	return strings.ReplaceAll(name, "/", "-"), nil
}

func safeName(name string) error {
	if name == "" || strings.HasPrefix(name, "-") || strings.ContainsAny(name, "\x00\r\n") || strings.HasPrefix(name, "@{") {
		return fmt.Errorf("invalid branch or workspace name %q", name)
	}
	return nil
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
	if err := repo.validateTree(tree); err != nil {
		return err
	}
	admin := repo.CommonDir
	if tree.Path != repo.Root {
		data, err := controlFile(filepath.Join(tree.Path, ".git"), false)
		if err != nil {
			return err
		}
		admin = strings.TrimSuffix(strings.TrimPrefix(string(data), "gitdir: "), "\n")
		if !filepath.IsAbs(admin) {
			admin = filepath.Join(tree.Path, admin)
		}
	}
	for _, name := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_LOG", "index.lock"} {
		path := filepath.Join(admin, name)
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("unfinished Git operation in %s (%s)", tree.Path, name)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	out, err := g.run(ctx, tree.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if len(out) != 0 && !allowDirty {
		return fmt.Errorf("worktree is dirty: %s; commit or move changes first (remove --force explicitly discards them)", tree.Path)
	}
	return nil
}

func (g gitHost) target(ctx context.Context, repo gitRepository, into, source string, checkedOut bool) (gitWorktree, error) {
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
		return gitWorktree{}, fmt.Errorf("cannot detect a different local merge target; merge with --into or remove with --force")
	}
	oid, err := g.branchOID(ctx, repo, into)
	if err != nil {
		return gitWorktree{}, err
	}
	if oid == "" {
		return gitWorktree{}, fmt.Errorf("target branch %q does not exist locally", into)
	}
	trees, err := g.worktrees(ctx, repo.Root)
	if err != nil {
		return gitWorktree{}, err
	}
	for _, tree := range trees {
		if tree.Branch == into {
			if tree.Locked {
				return tree, fmt.Errorf("target worktree is locked")
			}
			if err := repo.validateTree(tree); err != nil {
				return tree, err
			}
			if tree.Head != oid {
				return tree, fmt.Errorf("target branch changed while inspecting it")
			}
			return tree, nil
		}
	}
	if checkedOut {
		return gitWorktree{}, fmt.Errorf("target branch %q must be checked out in another worktree", into)
	}
	return gitWorktree{Branch: into, Head: oid}, nil
}

func (g gitHost) ancestor(ctx context.Context, repo gitRepository, source, target string) error {
	if !validOID(source) || !validOID(target) {
		return fmt.Errorf("invalid ancestry commit ID")
	}
	if _, err := g.run(ctx, repo.Root, "merge-base", "--is-ancestor", source, target); err != nil {
		return fmt.Errorf("source is not safely merged into target (or ancestry could not be checked); use explicit remove --force to discard unmerged work: %w", err)
	}
	return nil
}

func identity(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func ignoredFiles(ctx context.Context, g gitHost, dir string) (map[string]string, error) {
	out, err := g.run(ctx, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	files := make(map[string]string)
	for name := range strings.SplitSeq(string(out), "\x00") {
		if name == "" {
			continue
		}
		if !safeRelative(name) {
			return nil, fmt.Errorf("unsafe ignored path returned by Git")
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := noSymlinkPath(filepath.Dir(path)); err != nil {
			return nil, err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			files[name] = "link:" + identity(target)
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			files[name] = fmt.Sprintf("%o:%x", info.Mode().Perm(), hash.Sum(nil))
		default:
			return nil, fmt.Errorf("ignored path cannot be safely removed: %s", name)
		}
	}
	return files, nil
}

func safeRelative(path string) bool {
	return path != "" && path != "." && !filepath.IsAbs(path) && filepath.Clean(path) == path &&
		path != ".." && !strings.HasPrefix(path, "../") && !strings.ContainsAny(path, "\x00\r\n")
}
