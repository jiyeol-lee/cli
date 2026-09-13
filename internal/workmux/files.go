package workmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

type provisionEntry struct {
	name    string
	info    fs.FileInfo
	symlink bool
}

type provisionMatch struct {
	name    string
	pattern string
	symlink bool
}

func ApplyFiles(ctx context.Context, root, destination string, files FilesConfig, stderr io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateFilePatterns(files); err != nil {
		return err
	}
	if len(files.Copy) == 0 && len(files.Symlink) == 0 {
		return nil
	}
	if stderr == nil {
		stderr = io.Discard
	}
	source, sourcePath, err := openProvisionRoot(root, false)
	if err != nil {
		return fmt.Errorf("open source root: %w", err)
	}
	defer func() { _ = source.Close() }()
	target, targetPath, err := openProvisionRoot(destination, true)
	if err != nil {
		return fmt.Errorf("open destination root: %w", err)
	}
	defer func() { _ = target.Close() }()
	var matches []provisionMatch
	for _, list := range []struct {
		name     string
		patterns []string
		symlink  bool
	}{
		{"copy", files.Copy, false},
		{"symlink", files.Symlink, true},
	} {
		for _, pattern := range list.patterns {
			names, err := globProvision(ctx, source, pattern)
			if err != nil {
				return fmt.Errorf("files.%s pattern %q: %w", list.name, pattern, err)
			}
			if len(names) == 0 {
				if _, err := fmt.Fprintf(stderr, "workmux: warning: files.%s pattern %q matched no files\n", list.name, pattern); err != nil {
					return fmt.Errorf("write file warning: %w", err)
				}
			}
			for _, name := range names {
				matches = append(matches, provisionMatch{name: name, pattern: pattern, symlink: list.symlink})
			}
		}
	}
	slices.SortFunc(matches, func(a, b provisionMatch) int { return strings.Compare(a.name, b.name) })
	selected := make(map[string]provisionMatch)
	for _, match := range matches {
		for name := match.name; name != "."; name = filepath.Dir(name) {
			if previous, ok := selected[name]; ok {
				return fmt.Errorf("overlapping file patterns %q and %q at %q and %q", previous.pattern, match.pattern, previous.name, match.name)
			}
		}
		selected[match.name] = match
	}
	var plan []provisionEntry
	for _, match := range matches {
		entries, err := scanProvisionSource(ctx, source, match.name)
		if err != nil {
			return fmt.Errorf("preflight source %q: %w", match.name, err)
		}
		if entries[0].info.IsDir() {
			rel, err := filepath.Rel(filepath.Join(sourcePath, match.name), targetPath)
			if err != nil {
				return fmt.Errorf("check source directory %q: %w", match.name, err)
			}
			if filepath.IsLocal(rel) {
				return fmt.Errorf("source directory %q contains the destination", match.name)
			}
		}
		if match.symlink {
			entries = entries[:1]
			entries[0].symlink = true
		}
		plan = append(plan, entries...)
	}
	for _, entry := range plan {
		if err := preflightProvisionTarget(ctx, target, entry.name); err != nil {
			return fmt.Errorf("preflight destination %q: %w", entry.name, err)
		}
	}
	for _, entry := range plan {
		if err := applyProvisionEntry(ctx, source, target, sourcePath, entry); err != nil {
			return fmt.Errorf("provision %q: %w", entry.name, err)
		}
	}
	return ctx.Err()
}

func validateFilePatterns(files FilesConfig) error {
	for _, list := range []struct {
		name     string
		patterns []string
	}{
		{"copy", files.Copy},
		{"symlink", files.Symlink},
	} {
		for _, pattern := range list.patterns {
			if err := validateProvisionPath(pattern); err != nil {
				return fmt.Errorf("files.%s pattern %q: %w", list.name, pattern, err)
			}
			if strings.Contains(pattern, "<global>") || strings.Contains(pattern, "<agent>") {
				return fmt.Errorf("files.%s pattern %q: unresolved placeholder; only a separate <global> repository list entry is supported", list.name, pattern)
			}
			for part := range strings.SplitSeq(pattern, string(filepath.Separator)) {
				if _, err := filepath.Match(part, ""); err != nil {
					return fmt.Errorf("files.%s pattern %q: %w", list.name, pattern, err)
				}
			}
		}
	}
	return nil
}

func validateProvisionPath(name string) error {
	if strings.TrimSpace(name) == "" || filepath.IsAbs(name) || strings.ContainsRune(name, '\\') || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("expected a relative path without backslashes or control characters")
	}
	for part := range strings.SplitSeq(name, string(filepath.Separator)) {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("empty, . and .. path components are not allowed")
		}
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf(".git paths are not allowed")
		}
	}
	return nil
}

func openProvisionRoot(name string, destination bool) (*os.Root, string, error) {
	if strings.TrimSpace(name) == "" {
		return nil, "", fmt.Errorf("root path must not be empty")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return nil, "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return nil, "", err
	}
	if destination && canonical != absolute {
		return nil, "", fmt.Errorf("destination path must not have symlinked parents: %s", name)
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		return nil, "", err
	}
	if !info.IsDir() {
		return nil, "", fmt.Errorf("not a directory: %s", name)
	}
	root, err := os.OpenRoot(canonical)
	if err != nil {
		return nil, "", err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, "", fmt.Errorf("directory changed while opening %s", name)
	}
	return root, canonical, nil
}

// Pin each directory and check its identity rather than following destination parents.
func openProvisionDirectory(ctx context.Context, root *os.Root, name string, create bool) (*os.Root, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	if name == "." {
		return current, nil
	}
	for part := range strings.SplitSeq(name, string(filepath.Separator)) {
		if err := ctx.Err(); err != nil {
			_ = current.Close()
			return nil, err
		}
		info, err := current.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err = current.Mkdir(part, 0700); err == nil || errors.Is(err, fs.ErrExist) {
				info, err = current.Lstat(part)
			}
		}
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			_ = current.Close()
			return nil, fmt.Errorf("parent %q must be a directory, not a symlink or file", name)
		}
		next, err := current.OpenRoot(part)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			return nil, fmt.Errorf("parent %q changed while opening", name)
		}
		current = next
	}
	return current, nil
}

func provisionSourceInfo(ctx context.Context, root *os.Root, name string) (fs.FileInfo, error) {
	if err := validateProvisionPath(name); err != nil {
		return nil, err
	}
	parent, err := openProvisionDirectory(ctx, root, filepath.Dir(name), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	info, err := parent.Lstat(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing source symlink %q; select its real in-root target instead", name)
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("source %q is not a regular file or directory", name)
	}
	return info, nil
}

func readProvisionDirectory(ctx context.Context, root *os.Root, name string) ([]fs.DirEntry, error) {
	directory, err := openProvisionDirectory(ctx, root, name, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = directory.Close() }()
	file, err := directory.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	entries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, ctx.Err()
}

func globProvision(ctx context.Context, root *os.Root, pattern string) ([]string, error) {
	found := make(map[string]bool)
	var visit func(string, []string) error
	visit = func(directory string, parts []string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(parts) == 0 {
			if directory != "." {
				found[directory] = true
			}
			return nil
		}
		if parts[0] == "**" {
			if err := visit(directory, parts[1:]); err != nil {
				return err
			}
		}
		entries, err := readProvisionDirectory(ctx, root, directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			name := filepath.Join(directory, entry.Name())
			if parts[0] == "**" {
				if len(parts) == 1 {
					found[name] = true
				}
				// Recursive searches do not enter Git metadata, even when it is a file.
				if strings.EqualFold(entry.Name(), ".git") {
					continue
				}
				info, err := provisionSourceInfo(ctx, root, name)
				if err != nil {
					return err
				}
				if info.IsDir() {
					if err := visit(name, parts); err != nil {
						return err
					}
				}
				continue
			}
			matched, err := filepath.Match(parts[0], entry.Name())
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			if len(parts) == 1 {
				found[name] = true
				continue
			}
			info, err := provisionSourceInfo(ctx, root, name)
			if err != nil {
				return err
			}
			if info.IsDir() {
				if err := visit(name, parts[1:]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(".", strings.Split(pattern, string(filepath.Separator))); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(found))
	for name := range found {
		covered := false
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			if found[parent] {
				covered = true
				break
			}
		}
		if !covered {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

func scanProvisionSource(ctx context.Context, root *os.Root, name string) ([]provisionEntry, error) {
	info, err := provisionSourceInfo(ctx, root, name)
	if err != nil {
		return nil, err
	}
	plan := []provisionEntry{{name: name, info: info}}
	if !info.IsDir() {
		file, err := openProvisionFile(ctx, root, name, info)
		if err != nil {
			return nil, err
		}
		return plan, file.Close()
	}
	entries, err := readProvisionDirectory(ctx, root, name)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		children, err := scanProvisionSource(ctx, root, filepath.Join(name, entry.Name()))
		if err != nil {
			return nil, err
		}
		plan = append(plan, children...)
	}
	return plan, nil
}

func openProvisionFile(ctx context.Context, root *os.Root, name string, info fs.FileInfo) (*os.File, error) {
	parent, err := openProvisionDirectory(ctx, root, filepath.Dir(name), false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = parent.Close() }()
	current, err := parent.Lstat(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, fmt.Errorf("source file %q changed during provisioning", name)
	}
	file, err := parent.Open(filepath.Base(name))
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("source file %q changed while opening", name)
	}
	return file, nil
}

func preflightProvisionTarget(ctx context.Context, root *os.Root, name string) error {
	parent, err := openProvisionDirectory(ctx, root, filepath.Dir(name), false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if _, err := parent.Lstat(filepath.Base(name)); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("destination already exists; files are never overwritten")
	}
	return nil
}

func applyProvisionEntry(ctx context.Context, source, destination *os.Root, sourcePath string, entry provisionEntry) error {
	info, err := provisionSourceInfo(ctx, source, entry.name)
	if err != nil {
		return err
	}
	if !os.SameFile(entry.info, info) {
		return fmt.Errorf("source changed during provisioning")
	}
	if entry.symlink {
		if _, err := scanProvisionSource(ctx, source, entry.name); err != nil {
			return err
		}
	}
	var input *os.File
	if !entry.symlink && !info.IsDir() {
		input, err = openProvisionFile(ctx, source, entry.name, entry.info)
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
	}
	parent, err := openProvisionDirectory(ctx, destination, filepath.Dir(entry.name), true)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if err := ctx.Err(); err != nil {
		return err
	}
	name := filepath.Base(entry.name)
	if entry.symlink {
		return parent.Symlink(filepath.Join(sourcePath, entry.name), name)
	}
	if info.IsDir() {
		return parent.Mkdir(name, 0700)
	}
	mode := fs.FileMode(0600)
	if entry.info.Mode().Perm()&0111 != 0 {
		mode = 0700
	}
	output, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, provisionReader{ctx: ctx, reader: input})
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr, ctx.Err())
}

type provisionReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader provisionReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
