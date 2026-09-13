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
)

type provisionEntry struct {
	name    string
	info    fs.FileInfo
	symlink bool
	follow  bool
	link    *string
}

type provisionMatch struct {
	name    string
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
	if sourcePath == targetPath {
		return fmt.Errorf("source and destination roots must differ")
	}
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
			relative := pattern
			if filepath.IsAbs(pattern) {
				relative, err = filepath.Rel(sourcePath, pattern)
				if err != nil || !filepath.IsLocal(relative) {
					return fmt.Errorf("files.%s pattern %q is outside the repository", list.name, pattern)
				}
			}
			names, err := globProvision(ctx, source, relative)
			if err != nil {
				return fmt.Errorf("files.%s pattern %q: %w", list.name, pattern, err)
			}
			for _, name := range names {
				matches = append(matches, provisionMatch{name: name, symlink: list.symlink})
			}
		}
	}
	var plan []provisionEntry
	for _, match := range matches {
		var entries []provisionEntry
		if match.symlink {
			if match.name == "." {
				return fmt.Errorf("files.symlink cannot replace the destination root")
			}
			var info fs.FileInfo
			info, err = provisionSourceInfo(ctx, source, match.name)
			if err == nil {
				entries = []provisionEntry{{name: match.name, info: info, symlink: true}}
			}
		} else {
			entries, err = scanProvisionSource(ctx, source, match.name)
		}
		if err != nil {
			return fmt.Errorf("preflight source %q: %w", match.name, err)
		}
		if len(entries) == 0 {
			continue
		}
		if entries[0].info.IsDir() {
			canonical, err := filepath.EvalSymlinks(filepath.Join(sourcePath, match.name))
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(canonical, targetPath)
			if err != nil {
				return fmt.Errorf("check source directory %q: %w", match.name, err)
			}
			if filepath.IsLocal(rel) {
				return fmt.Errorf("source directory %q contains the destination", match.name)
			}
		}
		plan = append(plan, entries...)
	}
	for _, entry := range plan {
		rel, err := filepath.Rel(filepath.Join(targetPath, entry.name), sourcePath)
		if err != nil || filepath.IsLocal(rel) {
			return fmt.Errorf("destination %q contains the source root", entry.name)
		}
		if err := applyProvisionEntry(ctx, source, target, sourcePath, targetPath, entry); err != nil {
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
			for part := range strings.SplitSeq(pattern, string(filepath.Separator)) {
				if strings.Contains(part, "**") && part != "**" {
					return fmt.Errorf("files.%s pattern %q: ** must be a complete path component", list.name, pattern)
				}
				if _, err := filepath.Match(provisionGlobPart(part), ""); err != nil {
					return fmt.Errorf("files.%s pattern %q: %w", list.name, pattern, err)
				}
			}
		}
	}
	return nil
}

func validateProvisionPath(name string) error {
	if strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("paths must not contain NUL")
	}
	for part := range strings.SplitSeq(name, string(filepath.Separator)) {
		if part == ".." {
			return fmt.Errorf(".. path components are not allowed")
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateProvisionPath(name); err != nil {
		return nil, err
	}
	name, err := provisionSourceName(root, name, false)
	if err != nil {
		return nil, err
	}
	return os.Lstat(name)
}

func provisionSourceName(root *os.Root, name string, follow bool) (string, error) {
	// Authorize the lexical path; configured source symlinks may resolve outside it.
	if err := validateProvisionPath(name); err != nil {
		return "", err
	}
	if !filepath.IsLocal(name) {
		return "", fmt.Errorf("source path %q is outside the repository", name)
	}
	if name == "." {
		return root.Name(), nil
	}
	path := filepath.Join(root.Name(), name)
	if !follow {
		path = filepath.Dir(path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if !follow {
		canonical = filepath.Join(canonical, filepath.Base(name))
	}
	return canonical, nil
}

func provisionSourceStat(root *os.Root, name string) (fs.FileInfo, error) {
	name, err := provisionSourceName(root, name, true)
	if err != nil {
		return nil, err
	}
	return os.Stat(name)
}

func provisionSourceLink(root *os.Root, name string) (string, error) {
	name, err := provisionSourceName(root, name, false)
	if err != nil {
		return "", err
	}
	return os.Readlink(name)
}

func readProvisionDirectory(ctx context.Context, root *os.Root, name string) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := provisionSourceName(root, name, true)
	if err != nil {
		return nil, err
	}
	directory, err := os.OpenRoot(name)
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
	if filepath.Clean(pattern) == "." {
		return []string{"."}, ctx.Err()
	}
	found := make(map[string]bool)
	var ancestors []fs.FileInfo
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
		info, err := provisionSourceStat(root, directory)
		if err != nil {
			return err
		}
		for _, ancestor := range ancestors {
			if os.SameFile(info, ancestor) {
				return nil
			}
		}
		ancestors = append(ancestors, info)
		defer func() { ancestors = ancestors[:len(ancestors)-1] }()
		if parts[0] == "**" {
			// Matching zero directories is not a recursive descent.
			ancestors = ancestors[:len(ancestors)-1]
			if err := visit(directory, parts[1:]); err != nil {
				ancestors = append(ancestors, info)
				return err
			}
			ancestors = append(ancestors, info)
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
				info, err := provisionSourceStat(root, name)
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
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
			matched, err := filepath.Match(provisionGlobPart(parts[0]), entry.Name())
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
			info, err := provisionSourceStat(root, name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
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
	if err := visit(".", strings.Split(filepath.Clean(pattern), string(filepath.Separator))); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(found))
	for name := range found {
		if strings.HasSuffix(pattern, string(filepath.Separator)) {
			info, err := provisionSourceStat(root, name)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				continue
			}
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

func provisionGlobPart(part string) string {
	// Rust glob uses ! for a negated class and treats backslashes literally.
	return strings.ReplaceAll(strings.ReplaceAll(part, `\`, `\\`), "[!", "[^")
}

func scanProvisionSource(ctx context.Context, root *os.Root, name string) ([]provisionEntry, error) {
	return scanProvisionTree(ctx, root, name, true, nil)
}

func scanProvisionTree(ctx context.Context, root *os.Root, name string, top bool, ancestors []fs.FileInfo) ([]provisionEntry, error) {
	info, err := provisionSourceInfo(ctx, root, name)
	if err != nil {
		return nil, err
	}
	entry := provisionEntry{name: name, info: info}
	if info.Mode()&fs.ModeSymlink != 0 {
		if !top {
			link, err := provisionSourceLink(root, name)
			entry.link = &link
			return []provisionEntry{entry}, err
		}
		info, err = provisionSourceStat(root, name)
		if err != nil {
			return nil, err
		}
		entry.info, entry.follow = info, true
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil
	}
	plan := []provisionEntry{entry}
	if !info.IsDir() {
		file, err := openProvisionFile(ctx, root, name, info)
		if err != nil {
			return nil, err
		}
		return plan, file.Close()
	}
	for _, ancestor := range ancestors {
		if os.SameFile(info, ancestor) {
			return nil, fmt.Errorf("source directory cycle at %q", name)
		}
	}
	ancestors = append(ancestors, info)
	entries, err := readProvisionDirectory(ctx, root, name)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		children, err := scanProvisionTree(ctx, root, filepath.Join(name, entry.Name()), false, ancestors)
		if err != nil {
			return nil, err
		}
		plan = append(plan, children...)
	}
	return plan, nil
}

func openProvisionFile(ctx context.Context, root *os.Root, name string, info fs.FileInfo) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, err := provisionSourceName(root, name, true)
	if err != nil {
		return nil, err
	}
	current, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(info, current) {
		return nil, fmt.Errorf("source file %q changed during provisioning", name)
	}
	file, err := os.Open(name)
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

func applyProvisionEntry(ctx context.Context, source, destination *os.Root, sourcePath, destinationPath string, entry provisionEntry) error {
	info, err := provisionSourceInfo(ctx, source, entry.name)
	if err == nil && entry.follow {
		info, err = provisionSourceStat(source, entry.name)
	}
	if err != nil {
		return err
	}
	if !os.SameFile(entry.info, info) {
		return fmt.Errorf("source changed during provisioning")
	}
	var input *os.File
	if !entry.symlink && entry.link == nil && !info.IsDir() {
		input, err = openProvisionFile(ctx, source, entry.name, entry.info)
		if err != nil {
			return err
		}
		defer func() { _ = input.Close() }()
	}
	var link string
	if entry.link != nil {
		link, err = provisionSourceLink(source, entry.name)
		if err != nil || link != *entry.link {
			return fmt.Errorf("source symlink changed during provisioning")
		}
	}
	if entry.symlink {
		link, err = filepath.Rel(filepath.Dir(filepath.Join(destinationPath, entry.name)), filepath.Join(sourcePath, entry.name))
		if err != nil {
			return err
		}
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
	current, err := parent.Lstat(name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	directory := info.IsDir() && !entry.symlink && entry.link == nil
	if err == nil {
		if directory && current.IsDir() {
			return nil
		}
		if err := parent.RemoveAll(name); err != nil {
			return err
		}
	}
	if entry.symlink || entry.link != nil {
		return parent.Symlink(link, name)
	}
	if directory {
		return parent.Mkdir(name, 0700)
	}
	output, err := parent.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, provisionReader{ctx: ctx, reader: input})
	modeErr := output.Chmod(info.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky))
	closeErr := output.Close()
	return errors.Join(copyErr, modeErr, closeErr, ctx.Err())
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
