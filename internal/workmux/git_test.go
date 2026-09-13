package workmux

import (
	"bytes"
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

func TestInteractiveGitRetainsNonEditorProtectionAndIO(t *testing.T) {
	t.Setenv("GIT_EDITOR", "/configured/editor --wait")
	t.Setenv("GIT_SEQUENCE_EDITOR", "/configured/sequence-editor")
	runner := &tmuxTestRunner{}
	stdin := strings.NewReader("editor input")
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	app := App{Runner: runner, Stdin: stdin, Stdout: &stdout, Stderr: &stderr}
	if err := app.interactiveGit(t.Context(), dir, "commit"); err != nil {
		t.Fatal(err)
	}
	p := runner.calls[0]
	if p.Name != "git" || p.Dir != dir || !p.CleanGitEnv || p.Stdin != stdin || p.Stdout != &stdout || p.Stderr != &stderr {
		t.Fatalf("interactive process lost isolation or injected I/O: %+v", p)
	}
	for _, setting := range gitProtection {
		index := slices.Index(p.Args, setting)
		if strings.HasPrefix(setting, "core.editor=") || strings.HasPrefix(setting, "sequence.editor=") {
			if index != -1 {
				t.Fatalf("interactive editor was disabled by %s", setting)
			}
		} else if index < 1 || p.Args[index-1] != "-c" {
			t.Fatalf("interactive Git dropped protection %s: %q", setting, p.Args)
		}
	}
	for _, value := range []string{"GIT_EDITOR=/configured/editor --wait", "GIT_SEQUENCE_EDITOR=/configured/sequence-editor"} {
		if !slices.Contains(p.Env, value) {
			t.Fatalf("ambient editor was removed: %q", p.Env)
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

func TestGitAcceptsBareAndRefusesSubmoduleAndMainRemoval(t *testing.T) {
	f := newHostFixture(t, "")
	bare := filepath.Join(f.home, "bare")
	hostGit(t, f.home, "init", "--bare", bare)
	g := gitHost{runner: f.runner}
	if repo, err := g.discover(context.Background(), bare); err != nil || repo.Root != bare || repo.CommonDir != bare {
		t.Fatalf("bare repository = %+v, %v", repo, err)
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

func TestGitSeparateAndBareLinkedWorktrees(t *testing.T) {
	for _, kind := range []string{"separate", "bare"} {
		t.Run(kind, func(t *testing.T) {
			f := newHostFixture(t, "")
			root := filepath.Join(t.TempDir(), "repository")
			common := root
			if kind == "bare" {
				hostGit(t, f.root, "clone", "--bare", f.root, root)
			} else {
				common = filepath.Join(t.TempDir(), "metadata")
				hostGit(t, f.root, "clone", "--separate-git-dir", common, f.root, root)
			}
			path := filepath.Join(t.TempDir(), "linked")
			hostGit(t, root, "worktree", "add", "-b", "topic", path)
			g := gitHost{runner: f.runner}
			repo, err := g.discover(t.Context(), root)
			if err != nil || repo.Root != root || repo.CommonDir != common {
				t.Fatalf("discover = %+v, %v", repo, err)
			}
			if _, err := g.source(t.Context(), repo, Workspace{Path: path, Branch: "topic"}); err != nil {
				t.Fatal(err)
			}
			if _, err := g.source(t.Context(), repo, Workspace{Path: root, Branch: "main"}); err == nil {
				t.Fatal("main or bare repository can be removed")
			}
		})
	}
}

func TestWorkspaceSlugCommonNames(t *testing.T) {
	for _, test := range []struct{ name, slug string }{
		{"My Cool Feature", "my-cool-feature"}, {"Feature! @#$%", "feature"},
		{"feature/auth/oauth", "feature-auth-oauth"}, {"Release_1.2", "release-1-2"},
		{"  Workmux Test  ", "workmux-test"}, {"FOO/Bar_Baz", "foo-bar-baz"},
		{"___...!!!", ""},
	} {
		if got := workspaceSlug(test.name); got != test.slug {
			t.Errorf("workspaceSlug(%q) = %q, want %q", test.name, got, test.slug)
		}
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
