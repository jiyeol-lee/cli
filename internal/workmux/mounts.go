package workmux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

type sandboxMount struct {
	Source   string
	Target   string
	ReadOnly bool
	Snapshot bool
	Content  []byte `json:",omitempty"`
}

type sandboxMountInput struct {
	Path   string
	Device uint64
	Inode  uint64
	Mode   uint32
	UID    uint32
	GID    uint32
	Digest string
}

type sandboxMountPlan struct {
	Mounts      []sandboxMount
	Env         []string
	Inputs      []sandboxMountInput
	Fingerprint string
}

type sandboxGitIdentity struct {
	Worktree string
	Common   string
	Admin    string
	Pointer  string
}

type sandboxConfigEntry struct {
	Key      string
	Value    string
	HasValue bool
}

func sandboxMountPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("sandbox mount path must be a clean absolute path other than /: %q", path)
	}
	for _, ch := range path {
		if ch < ' ' || ch == 127 || ch == ',' || ch == '"' {
			return fmt.Errorf("unsafe character in sandbox mount path %q", path)
		}
	}
	return nil
}

func sandboxWithin(parent, path string) bool {
	return parent == path || strings.HasPrefix(path, parent+string(filepath.Separator))
}

func sandboxOverlap(a, b string) bool {
	return sandboxWithin(a, b) || sandboxWithin(b, a)
}

func sandboxCanonical(path string) error {
	if err := sandboxMountPath(path); err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve sandbox path %s: %w", path, err)
	}
	if canonical != path {
		return fmt.Errorf("sandbox policy and mount sources must not use symbolic links: %s", path)
	}
	return nil
}

func sandboxDirectory(path string, create, private bool) error {
	if err := sandboxMountPath(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		parent := filepath.Dir(path)
		if parent != "/" {
			if err := sandboxDirectory(parent, true, false); err != nil {
				return err
			}
		}
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create sandbox directory: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("inspect sandbox directory %s: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox directory must not be a symbolic link or file: %s", path)
	}
	if err := sandboxCanonical(path); err != nil {
		return err
	}
	if private && (info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid())) {
		return fmt.Errorf("sandbox private directory must be owned by the current user with mode 0700: %s", path)
	}
	return nil
}

func sandboxRegular(path string) (os.FileInfo, error) {
	if err := sandboxCanonical(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Git policy file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return nil, fmt.Errorf("invalid Git policy: must be a regular file without hard links: %s", path)
	}
	return info, nil
}

func sandboxPolicyFile(path string, create bool) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) && create {
		if err := sandboxDirectory(filepath.Dir(path), false, false); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil && !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create Git policy file: %w", err)
		}
		if file != nil {
			if err := file.Close(); err != nil {
				return err
			}
		}
	}
	_, err := sandboxRegular(path)
	return err
}

func sandboxPointer(path, prefix string) (string, error) {
	if _, err := sandboxRegular(path); err != nil {
		return "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Git pointer: %w", err)
	}
	line := strings.TrimSuffix(string(content), "\n")
	if !strings.HasPrefix(line, prefix) {
		return "", fmt.Errorf("invalid Git pointer %s", path)
	}
	value := strings.TrimPrefix(line, prefix)
	if value == "" || strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("invalid Git pointer: must contain a single nonempty path: %s", path)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Dir(path) + string(filepath.Separator) + value
	}
	resolved, err := filepath.EvalSymlinks(value)
	if err != nil {
		return "", fmt.Errorf("resolve Git pointer %s: %w", path, err)
	}
	if resolved != filepath.Clean(value) {
		return "", fmt.Errorf("invalid Git pointer: must not resolve through symbolic links: %s", path)
	}
	if err := sandboxCanonical(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func discoverSandboxGit(worktree, common string) (sandboxGitIdentity, error) {
	identity := sandboxGitIdentity{Worktree: worktree, Common: common, Pointer: filepath.Join(worktree, ".git")}
	if err := sandboxDirectory(worktree, false, false); err != nil {
		return identity, err
	}
	if err := sandboxDirectory(common, false, false); err != nil {
		return identity, err
	}
	admin, err := sandboxPointer(identity.Pointer, "gitdir: ")
	if err != nil {
		return identity, fmt.Errorf("sandbox requires a valid linked Git worktree: %w", err)
	}
	if filepath.Dir(admin) != filepath.Join(common, "worktrees") {
		return identity, fmt.Errorf("linked Git admin directory must be a direct child of %s/worktrees", common)
	}
	if err := sandboxDirectory(admin, false, false); err != nil {
		return identity, err
	}
	resolved, err := sandboxPointer(filepath.Join(admin, "commondir"), "")
	if err != nil || resolved != common {
		return identity, fmt.Errorf("linked Git commondir does not match %s: %v", common, err)
	}
	backlink, err := sandboxPointer(filepath.Join(admin, "gitdir"), "")
	if err != nil || backlink != identity.Pointer {
		return identity, fmt.Errorf("linked Git backlink does not match %s: %v", identity.Pointer, err)
	}
	identity.Admin = admin
	return identity, nil
}

func (c *Containers) mountPlan(ctx context.Context, w Workspace, image string, create bool) (sandboxMountPlan, error) {
	var plan sandboxMountPlan
	if err := ctx.Err(); err != nil {
		return plan, err
	}
	identity, err := discoverSandboxGit(w.Path, w.CommonDir)
	if err != nil {
		return plan, err
	}
	if sandboxWithin(w.Path, w.Root) || sandboxOverlap(w.Path, w.CommonDir) {
		return plan, fmt.Errorf("writable worktree overlaps main worktree or Git metadata")
	}
	if err := sandboxDirectory(w.Root, false, false); err != nil {
		return plan, err
	}
	rootGit := filepath.Join(w.Root, ".git")
	if rootGit != w.CommonDir {
		rootAdmin, err := sandboxPointer(rootGit, "gitdir: ")
		if err != nil {
			return plan, fmt.Errorf("main worktree identity: %w", err)
		}
		if rootAdmin != w.CommonDir {
			if _, err := discoverSandboxGit(w.Root, w.CommonDir); err != nil {
				return plan, fmt.Errorf("main worktree identity: %w", err)
			}
		}
	}
	if err := c.credentialMounts(ctx, w, &plan, create); err != nil {
		return plan, err
	}
	for _, entry := range []struct {
		path string
		ro   bool
	}{{w.Root, true}, {w.CommonDir, true}, {w.Path, false}} {
		if err := plan.bind(entry.path, entry.ro); err != nil {
			return plan, err
		}
	}
	if err := plan.gitBoundary(ctx, c, identity, create); err != nil {
		return plan, err
	}
	identityEnv, err := c.gitIdentityEnv(ctx, w.Path)
	if err != nil {
		return plan, err
	}
	plan.Env = append([]string{
		"HOME=/tmp", "XDG_DATA_HOME=/tmp/.local/share", "XDG_CONFIG_HOME=/tmp/.config",
		"XDG_STATE_HOME=/tmp/.local/state", "XDG_CACHE_HOME=/tmp/.cache",
		"PATH=/tmp/.local/bin:/usr/local/bin:/usr/bin:/bin",
	}, identityEnv...)
	slices.SortFunc(plan.Mounts, func(a, b sandboxMount) int { return strings.Compare(a.Target, b.Target) })
	slices.SortFunc(plan.Inputs, func(a, b sandboxMountInput) int { return strings.Compare(a.Path, b.Path) })
	data, err := json.Marshal(struct {
		Policy, Image, Engine, State string
		UID, GID                     int
		Plan                         sandboxMountPlan
	}{sandboxPolicy, image, "podman", c.StateDir, os.Getuid(), os.Getgid(), plan})
	if err != nil {
		return plan, err
	}
	plan.Fingerprint = fmt.Sprintf("%x", sha256.Sum256(data))
	key := sandboxWorkspaceKey(w)
	for i := range plan.Mounts {
		if plan.Mounts[i].Snapshot {
			plan.Mounts[i].Source = filepath.Join(c.StateDir, "containers", key, plan.Fingerprint, "config-"+strconv.Itoa(i))
		}
	}
	return plan, nil
}

func sandboxWorkspaceKey(w Workspace) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(w.RepoID+"\x00"+w.ID)))
}

func (c *Containers) openCodeConfigDir(config SandboxConfig) (string, error) {
	if path := config.OpenCodeConfigDir; path != "" {
		if strings.HasPrefix(path, "~/") {
			if err := sandboxMountPath(c.HomeDir); err != nil {
				return "", err
			}
			path = c.HomeDir + "/" + path[2:]
		}
		if err := sandboxMountPath(path); err != nil {
			return "", fmt.Errorf("sandbox.opencode_config_dir: %w", err)
		}
		return path, nil
	}
	path := c.getenv("XDG_CONFIG_HOME")
	if path == "" {
		if err := sandboxMountPath(c.HomeDir); err != nil {
			return "", err
		}
		path = filepath.Join(c.HomeDir, ".config")
	}
	if err := sandboxMountPath(path); err != nil {
		return "", err
	}
	return filepath.Join(path, "opencode"), nil
}

func (c *Containers) credentialMounts(ctx context.Context, w Workspace, plan *sandboxMountPlan, create bool) error {
	if err := sandboxDirectory(c.HomeDir, false, false); err != nil {
		return fmt.Errorf("sandbox home: %w", err)
	}
	if err := sandboxMountPath(c.StateDir); err != nil {
		return err
	}
	data := c.getenv("XDG_DATA_HOME")
	if data == "" {
		data = filepath.Join(c.HomeDir, ".local", "share")
	}
	state := c.getenv("XDG_STATE_HOME")
	if state == "" {
		state = filepath.Join(c.HomeDir, ".local", "state")
	}
	config, err := c.openCodeConfigDir(w.Config.Sandbox)
	if err != nil {
		return err
	}
	for _, path := range []string{data, config, state} {
		if err := sandboxMountPath(path); err != nil {
			return err
		}
	}
	data = filepath.Join(data, "opencode")
	state = filepath.Join(state, "opencode")
	privatePaths := []string{data, config, state, c.StateDir}
	for i, path := range privatePaths {
		for _, other := range privatePaths[i+1:] {
			if sandboxOverlap(path, other) {
				return fmt.Errorf("OpenCode and private sandbox paths overlap: %s and %s", path, other)
			}
		}
		for _, repo := range []string{w.Root, w.Path, w.CommonDir} {
			if sandboxOverlap(repo, path) || sandboxWithin(repo, c.HomeDir) {
				return fmt.Errorf("OpenCode, home or private state overlaps repository mounts: %s", repo)
			}
		}
	}
	for _, target := range []string{"/tmp/.local/share/opencode", "/tmp/.config/opencode", "/tmp/.local/state/opencode"} {
		for _, repo := range []string{w.Root, w.Path, w.CommonDir} {
			if sandboxOverlap(repo, target) {
				return fmt.Errorf("OpenCode guest mount overlaps repository: %s", repo)
			}
		}
	}
	if err := sandboxDirectory(c.StateDir, create, true); err != nil {
		return err
	}
	for i, path := range []string{data, config, state} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i == 1 {
			exists, err := sandboxOptional(path)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
		}
		if err := sandboxDirectory(path, create && i != 1, false); err != nil {
			return err
		}
		target := "/tmp/.local/share/opencode"
		if i == 1 {
			target = "/tmp/.config/opencode"
		} else if i == 2 {
			target = "/tmp/.local/state/opencode"
		}
		plan.Mounts = append(plan.Mounts, sandboxMount{Source: path, Target: target, ReadOnly: i == 1})
		if err := plan.input(path, false); err != nil {
			return err
		}
	}
	return nil
}

func (plan *sandboxMountPlan) input(path string, content bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect mount input %s: %w", path, err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	input := sandboxMountInput{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, Mode: uint32(info.Mode()), UID: stat.Uid, GID: stat.Gid}
	if content && info.Mode().IsRegular() {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read mount input: %w", err)
		}
		input.Digest = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	plan.Inputs = append(plan.Inputs, input)
	return nil
}

func (plan *sandboxMountPlan) bind(path string, ro bool) error {
	if err := sandboxCanonical(path); err != nil {
		return err
	}
	for _, mount := range plan.Mounts {
		if mount.Target == path {
			if mount.ReadOnly != ro {
				return fmt.Errorf("conflicting sandbox mounts at %s", path)
			}
			return nil
		}
	}
	plan.Mounts = append(plan.Mounts, sandboxMount{Source: path, Target: path, ReadOnly: ro})
	return plan.input(path, ro)
}

func (plan *sandboxMountPlan) policyTree(ctx context.Context, path string) error {
	return sandboxWalk(ctx, path, func(path string, entry fs.DirEntry) error {
		if entry.IsDir() {
			if err := sandboxDirectory(path, false, false); err != nil {
				return err
			}
		} else if _, err := sandboxRegular(path); err != nil {
			return err
		}
		return plan.input(path, true)
	})
}

func sandboxOptional(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (plan *sandboxMountPlan) gitBoundary(ctx context.Context, c *Containers, identity sandboxGitIdentity, create bool) error {
	if err := plan.worktreePointers(ctx, identity.Worktree); err != nil {
		return err
	}
	if err := plan.bind(identity.Pointer, true); err != nil {
		return err
	}
	for _, name := range []string{"objects", "refs", "logs", "rr-cache"} {
		path := filepath.Join(identity.Common, name)
		exists, err := sandboxOptional(path)
		if err != nil {
			return err
		}
		if exists {
			if err := sandboxDirectory(path, false, false); err != nil {
				return err
			}
			if err := plan.bind(path, false); err != nil {
				return err
			}
		}
	}
	if err := plan.bind(identity.Admin, false); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(identity.Common, "objects", "info"), filepath.Join(identity.Admin, "hooks"), filepath.Join(identity.Admin, "info"), filepath.Join(identity.Admin, "objects", "info"), filepath.Join(identity.Admin, "modules"), filepath.Join(identity.Admin, "worktrees")} {
		if err := sandboxDirectory(path, create, false); err != nil {
			return err
		}
		if filepath.Base(path) != "modules" && filepath.Base(path) != "worktrees" {
			if err := plan.policyTree(ctx, path); err != nil {
				return err
			}
		}
		if err := plan.bind(path, true); err != nil {
			return err
		}
	}
	for _, name := range []string{"hooks", "info"} {
		path := filepath.Join(identity.Common, name)
		exists, err := sandboxOptional(path)
		if err != nil {
			return err
		}
		if exists {
			if err := plan.policyTree(ctx, path); err != nil {
				return fmt.Errorf("protect Git %s: %w", name, err)
			}
		}
	}
	for _, name := range []string{"gitdir", "commondir"} {
		if err := plan.bind(filepath.Join(identity.Admin, name), true); err != nil {
			return err
		}
	}
	configs := []string{filepath.Join(identity.Common, "config"), filepath.Join(identity.Admin, "config"), filepath.Join(identity.Admin, "config.worktree")}
	commonWorktree := filepath.Join(identity.Common, "config.worktree")
	exists, err := sandboxOptional(commonWorktree)
	if err != nil {
		return err
	}
	if exists {
		configs = append(configs, commonWorktree)
	}
	for _, path := range configs {
		if err := sandboxPolicyFile(path, create && filepath.Dir(path) == identity.Admin); err != nil {
			return err
		}
		if err := plan.configSnapshot(ctx, c, identity, path); err != nil {
			return err
		}
	}
	for _, root := range []string{identity.Common, identity.Admin} {
		if err := plan.moduleRoots(ctx, c, identity, filepath.Join(root, "modules"), create); err != nil {
			return err
		}
	}
	return sandboxOtherAdmins(ctx, identity)
}

func sandboxOtherAdmins(ctx context.Context, identity sandboxGitIdentity) error {
	root := filepath.Join(identity.Common, "worktrees")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(root, entry.Name())
		if err := sandboxDirectory(path, false, false); err != nil {
			return err
		}
		for _, name := range []string{"config", "config.worktree", "gitdir", "commondir"} {
			file := filepath.Join(path, name)
			exists, err := sandboxOptional(file)
			if err != nil {
				return err
			}
			if exists {
				if _, err := sandboxRegular(file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (plan *sandboxMountPlan) worktreePointers(ctx context.Context, worktree string) error {
	return sandboxWalk(ctx, worktree, func(path string, entry fs.DirEntry) error {
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return fs.SkipDir
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if _, err := sandboxRegular(path); err != nil {
				return err
			}
			return plan.bind(path, true)
		}
		return nil
	})
}

func sandboxWalk(ctx context.Context, root string, visit func(string, fs.DirEntry) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if err := visit(path, entry); err != nil {
			return err
		}
		return ctx.Err()
	})
}

func parseSandboxConfig(data []byte) ([]sandboxConfigEntry, error) {
	var entries []sandboxConfigEntry
	if len(data) == 0 {
		return entries, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("missing NUL terminator in Git config output")
	}
	for raw := range bytes.SplitSeq(data[:len(data)-1], []byte{0}) {
		key, value, hasValue := strings.Cut(string(raw), "\n")
		if !strings.Contains(key, ".") || strings.ContainsAny(key, "\r\n\x00") {
			return nil, fmt.Errorf("invalid Git config key %q", key)
		}
		entries = append(entries, sandboxConfigEntry{key, value, hasValue})
	}
	return entries, nil
}

func sandboxConfigQuote(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`, "\b", `\b`).Replace(value) + `"`
}

func serializeSandboxConfig(entries []sandboxConfigEntry) ([]byte, error) {
	var output strings.Builder
	for _, entry := range entries {
		key := strings.ToLower(entry.Key)
		if key == "include.path" || strings.HasPrefix(key, "includeif.") || key == "core.worktree" {
			continue
		}
		first, last := strings.IndexByte(entry.Key, '.'), strings.LastIndexByte(entry.Key, '.')
		if first < 1 || last == len(entry.Key)-1 {
			return nil, fmt.Errorf("invalid Git config key %q", entry.Key)
		}
		section, variable := entry.Key[:first], entry.Key[last+1:]
		for _, name := range []string{section, variable} {
			for _, ch := range name {
				if ch != '-' && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') {
					return nil, fmt.Errorf("invalid Git config key %q", entry.Key)
				}
			}
		}
		output.WriteString("[" + section)
		if first != last {
			subsection := entry.Key[first+1 : last]
			if strings.ContainsAny(subsection, "\r\n\t") {
				return nil, fmt.Errorf("unsupported Git config subsection %q", subsection)
			}
			output.WriteString(" " + sandboxConfigQuote(subsection))
		}
		output.WriteString("]\n\t" + variable)
		if entry.HasValue {
			output.WriteString(" = " + sandboxConfigQuote(entry.Value))
		}
		output.WriteByte('\n')
	}
	return []byte(output.String()), nil
}

func (plan *sandboxMountPlan) configSnapshot(ctx context.Context, c *Containers, identity sandboxGitIdentity, path string) error {
	entries, err := c.gitConfig(ctx, path)
	if err != nil {
		return err
	}
	if err := plan.configPolicy(ctx, c, identity, path, entries, make(map[string]bool)); err != nil {
		return err
	}
	content, err := serializeSandboxConfig(entries)
	if err != nil {
		return err
	}
	plan.Mounts = append(plan.Mounts, sandboxMount{Source: path, Target: path, ReadOnly: true, Snapshot: true, Content: content})
	return plan.input(path, true)
}

func (c *Containers) gitConfig(ctx context.Context, path string) ([]sandboxConfigEntry, error) {
	if _, err := sandboxRegular(path); err != nil {
		return nil, err
	}
	args := make([]string, 0, len(gitProtection)*2+6)
	for _, setting := range gitProtection {
		args = append(args, "-c", setting)
	}
	args = append(args, "config", "--file", path, "--null", "--list", "--no-includes")
	out, err := c.Runner.Run(ctx, Process{Name: "git", Args: args, Dir: "/", CleanGitEnv: true, Env: []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}})
	if err != nil {
		return nil, fmt.Errorf("inspect Git config %s: %w", path, err)
	}
	return parseSandboxConfig(out)
}

func (plan *sandboxMountPlan) configPolicy(ctx context.Context, c *Containers, identity sandboxGitIdentity, path string, entries []sandboxConfigEntry, visited map[string]bool) error {
	if visited[path] {
		return nil
	}
	visited[path] = true
	for _, entry := range entries {
		key := strings.ToLower(entry.Key)
		include := key == "include.path" || strings.HasPrefix(key, "includeif.") && strings.HasSuffix(key, ".path")
		if !include {
			if err := plan.executablePolicy(ctx, identity, path, key, entry.Value); err != nil {
				return err
			}
			continue
		}
		value := entry.Value
		if value == "" || strings.ContainsAny(value, "*?[%") {
			return fmt.Errorf("git config include path cannot be protected safely: %q", value)
		}
		if strings.HasPrefix(value, "~/") {
			value = c.HomeDir + string(filepath.Separator) + value[2:]
		}
		if !filepath.IsAbs(value) {
			value = filepath.Dir(path) + string(filepath.Separator) + value
		}
		value, err := sandboxResolvePolicy(value)
		if err != nil {
			return fmt.Errorf("git config include target must exist and be safe before sandbox startup: %w", err)
		}
		included, err := c.gitConfig(ctx, value)
		if err != nil {
			return fmt.Errorf("git config include target must exist and be safe before sandbox startup: %w", err)
		}
		if err := plan.configPolicy(ctx, c, identity, value, included, visited); err != nil {
			return err
		}
		if sandboxWithin(identity.Worktree, value) || sandboxWithin(identity.Common, value) {
			if err := plan.bind(value, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func sandboxResolvePolicy(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve Git policy %s: %w", path, err)
	}
	if resolved != filepath.Clean(path) {
		return "", fmt.Errorf("git policy must not resolve through symbolic links: %s", path)
	}
	if err := sandboxMountPath(resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

func (plan *sandboxMountPlan) moduleRoots(ctx context.Context, c *Containers, identity sandboxGitIdentity, path string, create bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exists, err := sandboxOptional(path)
	if err != nil || !exists {
		return err
	}
	if err := sandboxDirectory(path, false, false); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			if err := plan.moduleRoot(ctx, c, identity, filepath.Join(path, entry.Name()), create); err != nil {
				return err
			}
		}
	}
	return nil
}

func sandboxGitPathType(path string, directory bool) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("git metadata must not be a symbolic link: %s", path)
	}
	if directory {
		return info.IsDir(), nil
	}
	return info.Mode().IsRegular(), nil
}

func (plan *sandboxMountPlan) moduleRoot(ctx context.Context, c *Containers, identity sandboxGitIdentity, path string, create bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := sandboxDirectory(path, false, false); err != nil {
		return err
	}
	isRoot, err := sandboxGitPathType(filepath.Join(path, "config"), false)
	if err != nil {
		return err
	}
	if !isRoot {
		head, err := sandboxGitPathType(filepath.Join(path, "HEAD"), false)
		if err != nil {
			return err
		}
		objects, err := sandboxGitPathType(filepath.Join(path, "objects"), true)
		if err != nil {
			return err
		}
		isRoot = head && objects
	}
	if isRoot {
		if err := plan.bind(path, false); err != nil {
			return err
		}
		for _, name := range []string{"hooks", "info", "objects/info", "modules", "worktrees"} {
			dir := filepath.Join(path, name)
			if err := sandboxDirectory(dir, create, false); err != nil {
				return err
			}
			if name != "modules" && name != "worktrees" {
				if err := plan.policyTree(ctx, dir); err != nil {
					return err
				}
			}
			if err := plan.bind(dir, true); err != nil {
				return err
			}
		}
		for _, name := range []string{"config", "config.worktree"} {
			file := filepath.Join(path, name)
			if err := sandboxPolicyFile(file, create); err != nil {
				return err
			}
			if err := plan.configSnapshot(ctx, c, identity, file); err != nil {
				return err
			}
		}
		for _, name := range []string{"gitdir", "commondir"} {
			file := filepath.Join(path, name)
			exists, err := sandboxOptional(file)
			if err != nil {
				return err
			}
			if exists {
				if _, err := sandboxRegular(file); err != nil {
					return err
				}
				if err := plan.bind(file, true); err != nil {
					return err
				}
			}
		}
		return plan.moduleRoots(ctx, c, identity, filepath.Join(path, "modules"), create)
	}
	return plan.moduleRoots(ctx, c, identity, path, create)
}

func (plan *sandboxMountPlan) executablePolicy(ctx context.Context, identity sandboxGitIdentity, config, key, value string) error {
	hooks := key == "core.hookspath"
	executable := hooks || key == "core.fsmonitor" || strings.HasPrefix(key, "diff.") && (strings.HasSuffix(key, ".command") || strings.HasSuffix(key, ".textconv")) || strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".smudge") || strings.HasSuffix(key, ".process")) || strings.HasPrefix(key, "merge.") && strings.HasSuffix(key, ".driver")
	if !executable || key == "core.fsmonitor" && (value == "true" || value == "false") {
		return nil
	}
	token := value
	if !hooks && key != "core.fsmonitor" {
		fields := strings.Fields(strings.TrimLeft(value, "!"))
		if len(fields) == 0 {
			return nil
		}
		token = fields[0]
	}
	token = strings.Trim(token, "'\"")
	if token == "" || strings.ContainsAny(token, "$`%") {
		return nil
	}
	candidates := []string{token}
	if !filepath.IsAbs(token) {
		candidates = []string{identity.Worktree + string(filepath.Separator) + token, filepath.Dir(config) + string(filepath.Separator) + token}
	}
	for _, path := range candidates {
		exists, err := sandboxOptional(path)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		path, err = sandboxResolvePolicy(path)
		if err != nil {
			return err
		}
		if !sandboxWithin(identity.Worktree, path) && !sandboxWithin(identity.Common, path) {
			continue
		}
		if sandboxWithin(path, identity.Worktree) || sandboxWithin(path, identity.Admin) {
			return fmt.Errorf("executable Git policy overlaps writable Git root: %s", path)
		}
		if err := plan.policyTree(ctx, path); err != nil {
			return err
		}
		if err := plan.bind(path, true); err != nil {
			return err
		}
		if hooks {
			break
		}
	}
	return nil
}

func (c *Containers) gitIdentityEnv(ctx context.Context, worktree string) ([]string, error) {
	output, err := (gitHost{runner: c.Runner}).run(ctx, "/", "-C", worktree, "config", "--null", "--get-regexp", `^user\.(name|email)$`)
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("read Git user identity: %w", err)
	}
	entries, err := parseSandboxConfig(output)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, entry := range entries {
		values[strings.ToLower(entry.Key)] = entry.Value
	}
	var env []string
	for _, entry := range []struct{ key, author, committer string }{
		{"user.name", "GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"},
		{"user.email", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"},
	} {
		if value := values[entry.key]; value != "" {
			env = append(env, entry.author+"="+value, entry.committer+"="+value)
		}
	}
	return env, nil
}

func (plan sandboxMountPlan) snapshots(create bool) error {
	for _, mount := range plan.Mounts {
		if !mount.Snapshot {
			continue
		}
		if err := sandboxDirectory(filepath.Dir(mount.Source), create, true); err != nil {
			return fmt.Errorf("sandbox snapshot directory: %w", err)
		}
		if create {
			file, err := os.OpenFile(mount.Source, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("create sandbox config snapshot: %w", err)
			}
			if file != nil {
				_, writeErr := file.Write(mount.Content)
				closeErr := file.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					return fmt.Errorf("write sandbox config snapshot: %w", err)
				}
			}
		}
		info, err := sandboxRegular(mount.Source)
		if err != nil {
			return fmt.Errorf("sandbox snapshot changed; remove the container and its private snapshot directory before recreating: %w", err)
		}
		if info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return fmt.Errorf("sandbox config snapshot must be owned by the current user with mode 0600: %s", mount.Source)
		}
		data, err := os.ReadFile(mount.Source)
		if err != nil || !bytes.Equal(data, mount.Content) {
			return fmt.Errorf("sandbox config snapshot changed; remove the container and its private snapshot directory before recreating: %s", mount.Source)
		}
	}
	return nil
}
