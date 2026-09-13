package workmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestGitWorktreePorcelainNUL(t *testing.T) {
	head := strings.Repeat("a", 40)
	input := "worktree /repo with spaces\x00HEAD " + head + "\x00branch refs/heads/main\x00\x00" +
		"worktree /worktree\nwith\"quotes\x00HEAD " + head + "\x00branch refs/heads/feature/topic\x00locked reason\x00\x00"
	got, err := parseWorktrees([]byte(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []gitWorktree{
		{Path: "/repo with spaces", Head: head, Branch: "main"},
		{Path: "/worktree\nwith\"quotes", Head: head, Branch: "feature/topic", Locked: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for _, input := range []string{
		"worktree /repo\nHEAD abc\n", "worktree relative\x00\x00", "HEAD abc\x00",
		"worktree /repo\x00\x00worktree /repo\x00\x00", "worktree /repo\x00branch tags/main\x00\x00",
	} {
		if _, err := parseWorktrees([]byte(input)); err == nil {
			t.Fatalf("accepted invalid porcelain %q", input)
		}
	}
}

func TestGitProtectionAndEnvironment(t *testing.T) {
	runner := &tmuxTestRunner{run: func(p Process) ([]byte, error) { return nil, nil }}
	if _, err := (gitHost{runner: runner}).run(context.Background(), "/repo", "status", "--porcelain=v1"); err != nil {
		t.Fatal(err)
	}
	p := runner.calls[0]
	if !p.CleanGitEnv || p.Dir != "/repo" || p.Name != "git" {
		t.Fatalf("unprotected process: %+v", p)
	}
	for _, setting := range []string{
		"core.fsmonitor=false", "core.hooksPath=/dev/null", "core.editor=false", "sequence.editor=false",
		"commit.gpgSign=false", "tag.gpgSign=false", "gpg.program=/dev/null", "diff.external=", "merge.autoStash=false",
		"interactive.diffFilter=", "uploadpack.packObjectsHook=", "protocol.allow=never",
		"protocol.file.allow=always", "protocol.http.allow=always", "protocol.https.allow=always",
		"protocol.ssh.allow=always", "protocol.git.allow=always",
	} {
		i := slices.Index(p.Args, setting)
		if i < 1 || p.Args[i-1] != "-c" {
			t.Fatalf("missing protected config %s in %q", setting, p.Args)
		}
	}
	for _, value := range []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=false", "SSH_ASKPASS=false", "GIT_EDITOR=false", "GIT_SEQUENCE_EDITOR=false", "GIT_PAGER=cat"} {
		if !slices.Contains(p.Env, value) {
			t.Fatalf("missing %s in environment", value)
		}
	}
	for _, arg := range p.Args {
		if strings.HasPrefix(arg, "filter.") || strings.HasPrefix(arg, "merge.") && arg != "merge.autoStash=false" || arg == "-C" {
			t.Fatalf("unexpected Git override %s", arg)
		}
	}
}

func TestGitDiscoveryCanonicalIdentityAndLinkedPointers(t *testing.T) {
	f := newHostFixture(t, "")
	if err := f.run(t, "add", "feature/topic", "-b"); err != nil {
		t.Fatal(err)
	}
	w := f.load(t, "feature/topic")
	g := gitHost{runner: f.runner}
	main, err := g.discover(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	linked, err := g.discover(context.Background(), w.Path)
	if err != nil {
		t.Fatal(err)
	}
	if main.Root != linked.Root || main.CommonDir != linked.CommonDir || linked.Current != w.Path {
		t.Fatalf("inconsistent repository identity: %+v %+v", main, linked)
	}
	alias := filepath.Join(f.home, "repo-link")
	if err := os.Symlink(f.root, alias); err != nil {
		t.Fatal(err)
	}
	aliased, err := g.discover(context.Background(), alias)
	if err != nil || aliased.CommonDir != main.CommonDir {
		t.Fatalf("canonical alias identity: %+v, %v", aliased, err)
	}
	admin := hostGit(t, w.Path, "rev-parse", "--absolute-git-dir")
	hostWrite(t, filepath.Join(admin, "gitdir"), filepath.Join(f.root, ".git")+"\n")
	if _, err := g.source(context.Background(), main, w.Workspace); err == nil {
		t.Fatal("accepted invalid administration backlink")
	}
}

func TestGitRefusesLinkedControlSymlinksAndHardlinks(t *testing.T) {
	for _, name := range []string{"pointer", "gitdir", "commondir", "HEAD", "config", "branch"} {
		for _, link := range []string{"symlink", "hardlink"} {
			t.Run(name+"/"+link, func(t *testing.T) {
				f := newHostFixture(t, "")
				if err := f.run(t, "add", "topic", "-b"); err != nil {
					t.Fatal(err)
				}
				w := f.load(t, "topic")
				g := gitHost{runner: f.runner}
				repo, err := g.discover(context.Background(), f.root)
				if err != nil {
					t.Fatal(err)
				}
				admin := hostGit(t, w.Path, "rev-parse", "--absolute-git-dir")
				path := filepath.Join(admin, name)
				switch name {
				case "pointer":
					path = filepath.Join(w.Path, ".git")
				case "config":
					path = filepath.Join(repo.CommonDir, "config")
				case "branch":
					path = filepath.Join(repo.CommonDir, "refs", "heads", "topic")
				}
				copy := filepath.Join(f.home, "control")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(copy, data, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if link == "symlink" {
					err = os.Symlink(copy, path)
				} else {
					err = os.Link(copy, path)
				}
				if err != nil {
					t.Fatal(err)
				}
				if _, err := g.source(context.Background(), repo, w.Workspace); err == nil {
					t.Fatal("accepted linked Git control file")
				}
			})
		}
	}
}

func TestGitRefusesBareSubmoduleAndMainRemoval(t *testing.T) {
	f := newHostFixture(t, "")
	bare := filepath.Join(f.home, "bare")
	hostGit(t, f.home, "init", "--bare", bare)
	g := gitHost{runner: f.runner}
	if _, err := g.discover(context.Background(), bare); err == nil {
		t.Fatal("accepted bare repository")
	}
	sub := filepath.Join(f.root, "sub")
	hostGit(t, f.root, "-c", "protocol.file.allow=always", "submodule", "add", f.root, sub)
	if _, err := g.discover(context.Background(), sub); err == nil {
		t.Fatal("accepted submodule repository")
	}
	repo, err := g.discover(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.source(context.Background(), repo, Workspace{Path: f.root, Branch: "main"}); err == nil {
		t.Fatal("accepted main worktree as a managed removal source")
	}
}

func TestGitBranchNamesDoNotExpandOrInject(t *testing.T) {
	f := newHostFixture(t, "")
	g := gitHost{runner: f.runner}
	for _, name := range []string{"-bad", "@{-1}", "bad\nname", "bad\rname", "../escape", "feature/../../escape", "foo.lock", "a//b", "with space", "a\\b"} {
		if _, err := g.branchName(context.Background(), f.root, name); err == nil {
			t.Fatalf("accepted invalid branch %q", name)
		}
	}
	if got, err := g.branchName(context.Background(), f.root, "feature/topic"); err != nil || got != "feature-topic" {
		t.Fatalf("valid branch = %s, %v", got, err)
	}
}

func TestGitTargetDetectionUsesLocalOriginHeadAndRequiresCheckout(t *testing.T) {
	f := newHostFixture(t, "")
	hostGit(t, f.root, "branch", "release")
	hostGit(t, f.root, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/release")
	g := gitHost{runner: f.runner}
	repo, err := g.discover(context.Background(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	if target, err := g.target(context.Background(), repo, "", "topic", false); err != nil || target.Branch != "release" {
		t.Fatalf("target = %+v, %v", target, err)
	}
	if _, err := g.target(context.Background(), repo, "", "topic", true); err == nil {
		t.Fatal("accepted unchecked-out merge target")
	}
}
