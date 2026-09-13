package workmux

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestOpenCodeConfigDirMerge(t *testing.T) {
	for _, test := range []struct{ repo, want string }{
		{"{}", "/global/opencode"},
		{"opencode_config_dir: null", "/global/opencode"},
		{"opencode_config_dir: ~/dotfiles/.opencode", "~/dotfiles/.opencode"},
		{"opencode_config_dir: ''", ""},
	} {
		t.Run(test.repo, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "sandbox:\n  opencode_config_dir: /global/opencode\n")
			writeConfigFixture(t, dir, "project.yaml", "sandbox: {"+strings.Trim(test.repo, "{}")+"}\n")
			config, err := LoadConfig(dir, "project")
			if err != nil || config.Sandbox.OpenCodeConfigDir != test.want {
				t.Fatalf("config = %+v, %v", config.Sandbox, err)
			}
			for _, marshal := range []func(any) ([]byte, error){json.Marshal, yaml.Marshal} {
				data, err := marshal(config.Sandbox)
				if err != nil || strings.Contains(string(data), "opencode_config_dir") != (test.want != "") {
					t.Fatalf("serialized config = %s, %v", data, err)
				}
			}
		})
	}
	for _, value := range []string{"true", "42", "[]", "{}"} {
		if _, err := decodeConfig([]byte("sandbox: {opencode_config_dir: " + value + "}")); err == nil {
			t.Fatalf("accepted nonstring %s", value)
		}
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Panes, []Pane{{Focus: true}, {Command: "clear", Split: "horizontal"}}) {
		t.Fatalf("panes = %#v", config.Panes)
	}
	if config.Sandbox != (SandboxConfig{Image: "localhost/cli-workmux:fedora44"}) || config.PreRemove != nil {
		t.Fatalf("defaults = %#v", config)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("LoadConfig created its config directory: %v", err)
	}
}

func TestLoadConfigEmptyDocuments(t *testing.T) {
	for _, test := range []struct{ name, data string }{
		{"zero bytes", ""},
		{"whitespace and comments", "  \n# no settings\n"},
		{"bare document", "---\n"},
		{"document with comments", "---\n# no settings\n"},
		{"empty mapping", "{}"},
	} {
		t.Run(test.name, func(t *testing.T) {
			layer, err := decodeConfig([]byte(test.data))
			if err != nil || !reflect.DeepEqual(layer, configFile{}) {
				t.Fatalf("empty layer = %#v, %v", layer, err)
			}
			dir, root := t.TempDir(), t.TempDir()
			name, err := repoConfigName(filepath.Base(root))
			if err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, root, "package-lock.json", "{}")
			writeConfigFixture(t, dir, "config.yaml", test.data)
			writeConfigFixture(t, dir, name, test.data)
			config, err := LoadConfigForRepo(dir, root)
			if err != nil {
				t.Fatal(err)
			}
			panes, err := config.SelectPanes("")
			if err != nil || !reflect.DeepEqual(panes, defaultPanes()) || !reflect.DeepEqual(config.PreRemove, []string{nodeModulesCleanupScript}) {
				t.Fatalf("empty documents lost root defaults: %#v, %v", config, err)
			}
			writeConfigFixture(t, dir, "config.yaml", "panes: [{command: inherited}]\npost_create: [inherited]\npre_remove: []")
			config, err = LoadConfigForRepo(dir, root)
			if err != nil || !reflect.DeepEqual(config.Panes, []Pane{{Command: "inherited"}}) || !reflect.DeepEqual(config.PostCreate, []string{"inherited"}) || config.PreRemove == nil || len(config.PreRemove) != 0 {
				t.Fatalf("empty repository layer lost inheritance: %#v, %v", config, err)
			}
			writeConfigFixture(t, dir, "config.yaml", test.data)
			writeConfigFixture(t, dir, name, "panes: []\npost_create: [repository]\npre_remove: []")
			config, err = LoadConfigForRepo(dir, root)
			if err != nil || config.Panes == nil || len(config.Panes) != 0 || !reflect.DeepEqual(config.PostCreate, []string{"repository"}) || config.PreRemove == nil || len(config.PreRemove) != 0 {
				t.Fatalf("empty global layer lost repository overrides: %#v, %v", config, err)
			}
		})
	}
}

func TestLoadConfigInheritance(t *testing.T) {
	dir := t.TempDir()
	for _, fixture := range []struct{ source, destination string }{
		{"global.yaml", "config.yaml"},
		{"repository.yaml", "project.yaml"},
	} {
		data, err := os.ReadFile(filepath.Join("testdata", "config", fixture.source))
		if err != nil {
			t.Fatal(err)
		}
		writeConfigFixture(t, dir, fixture.destination, string(data))
	}
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Panes, []Pane{{Command: "project-editor"}}) {
		t.Fatalf("panes did not replace global panes: %#v", config.Panes)
	}
	for _, list := range []struct {
		name      string
		got, want []string
	}{
		{"copy", config.Files.Copy, []string{"before.env", ".env", "local/*.json", "after.env"}},
		{"symlink", config.Files.Symlink, []string{}},
		{"post_create", config.PostCreate, []string{"before", "prepare", "report", "after"}},
		{"pre_merge", config.PreMerge, []string{}},
		{"pre_remove", config.PreRemove, []string{"project-clean"}},
	} {
		if !reflect.DeepEqual(list.got, list.want) {
			t.Errorf("%s = %#v, want %#v", list.name, list.got, list.want)
		}
	}
	if config.Sandbox.Enabled || config.Sandbox.Image != "registry.example.com/team/dev:stable" {
		t.Fatalf("sandbox inheritance = %#v", config.Sandbox)
	}
	if config.Layouts["review"].Panes[0].Command != "project-review" || !reflect.DeepEqual(config.Layouts["shell"].Panes, []Pane{}) {
		t.Fatalf("layouts = %#v", config.Layouts)
	}
}

func TestLoadConfigSandboxFieldInheritance(t *testing.T) {
	for _, test := range []struct {
		name, repo string
		want       SandboxConfig
	}{
		{"omitted", "{}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1", Target: "all"}},
		{"empty", "sandbox: {}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1", Target: "all"}},
		{"null", "sandbox: {enabled: null, image: null, target: null}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1", Target: "all"}},
		{"false", "sandbox: {enabled: false}", SandboxConfig{Image: "example.com/dev:v1", Target: "all"}},
		{"image", "sandbox: {image: localhost/dev:v2}", SandboxConfig{Enabled: true, Image: "localhost/dev:v2", Target: "all"}},
		{"target", "sandbox: {target: agent}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1", Target: "agent"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "sandbox: {enabled: true, image: example.com/dev:v1, target: all}")
			writeConfigFixture(t, dir, "project.yaml", test.repo)
			config, err := LoadConfig(dir, "project")
			if err != nil || config.Sandbox != test.want {
				t.Fatalf("sandbox = %#v, error = %v, want %#v", config.Sandbox, err, test.want)
			}
		})
	}
}

func TestLoadConfigEmptyAndOmittedLists(t *testing.T) {
	for _, test := range []struct {
		repo string
		want []Pane
	}{
		{"{}", []Pane{{Command: "global"}}},
		{"panes: null", []Pane{{Command: "global"}}},
		{"panes: []", []Pane{}},
		{"panes: [{}]", []Pane{{}}},
		{"panes: [{command: null}]", []Pane{{}}},
	} {
		dir := t.TempDir()
		writeConfigFixture(t, dir, "config.yaml", "panes: [{command: global}]\nfiles: {copy: [.env]}\npost_create: [prepare]")
		writeConfigFixture(t, dir, "project.yaml", test.repo)
		config, err := LoadConfig(dir, "project")
		if err != nil || !reflect.DeepEqual(config.Panes, test.want) {
			t.Fatalf("%s: panes = %#v, error = %v", test.repo, config.Panes, err)
		}
		selected, err := config.SelectPanes("")
		if err != nil || !reflect.DeepEqual(selected, test.want) {
			t.Fatalf("selected = %#v, %v", selected, err)
		}
		if !reflect.DeepEqual(config.Files.Copy, []string{".env"}) || !reflect.DeepEqual(config.PostCreate, []string{"prepare"}) {
			t.Fatalf("omitted lists did not inherit: %#v", config)
		}
	}
}

func TestLoadConfigListSplicing(t *testing.T) {
	for _, field := range []string{"post_create", "pre_merge", "pre_remove", "files.copy", "files.symlink"} {
		t.Run(field, func(t *testing.T) {
			makeList := func(values string) string {
				if name, ok := strings.CutPrefix(field, "files."); ok {
					return "files: {" + name + ": " + values + "}"
				}
				return field + ": " + values
			}
			for _, test := range []struct {
				list string
				want []string
			}{
				{"[replacement]", []string{"replacement"}},
				{"[]", []string{}},
				{"null", []string{"global"}},
				{"[<global>, last]", []string{"global", "last"}},
				{"[first, <global>]", []string{"first", "global"}},
				{"[first, <global>, last, <global>]", []string{"first", "global", "last", "global"}},
			} {
				dir := t.TempDir()
				writeConfigFixture(t, dir, "config.yaml", makeList("[global]"))
				writeConfigFixture(t, dir, "project.yaml", makeList(test.list))
				config, err := LoadConfig(dir, "project")
				if err != nil {
					t.Fatal(err)
				}
				lists := map[string][]string{"post_create": config.PostCreate, "pre_merge": config.PreMerge, "pre_remove": config.PreRemove, "files.copy": config.Files.Copy, "files.symlink": config.Files.Symlink}
				if !reflect.DeepEqual(lists[field], test.want) {
					t.Fatalf("%s = %#v, want %#v", test.list, lists[field], test.want)
				}
			}
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", makeList("[<global>]"))
			if _, err := LoadConfig(dir, "project"); err != nil {
				t.Fatalf("global literal marker: %v", err)
			}
		})
	}
}

func TestLoadConfigParityYAML(t *testing.T) {
	dir := t.TempDir()
	data, err := os.ReadFile("testdata/config/parity.yaml")
	if err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, dir, "config.yaml", string(data))
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Panes) != 3 || config.Panes[1].Target == nil || *config.Panes[1].Target != 0 || !config.Panes[1].SizeSpecified() || !config.Panes[2].Focus {
		t.Fatalf("pane fields = %#v", config.Panes)
	}
	if config.Panes[0].Command != "echo '<global>' '<agent>'" || len(config.Layouts["aliased"].Panes) != 3 || len(config.Layouts["empty"].Panes) != 0 {
		t.Fatalf("aliases/literals = %#v", config)
	}
	if config.Layouts["merged"].Panes[0].Command != "" {
		t.Fatalf("YAML << was applied: %#v", config.Layouts)
	}
	writeConfigFixture(t, dir, "project.yaml", "layouts: {aliased: {panes: []}, added: {panes: [{}]}}")
	config, err = LoadConfig(dir, "project")
	if err != nil || len(config.Layouts) != 4 || len(config.Layouts["aliased"].Panes) != 0 {
		t.Fatalf("map extension/replacement = %#v, %v", config.Layouts, err)
	}
	writeConfigFixture(t, dir, "project.yaml", "layouts: {}")
	config, err = LoadConfig(dir, "project")
	if err != nil || len(config.Layouts) != 3 || len(config.Layouts["aliased"].Panes) != 3 {
		t.Fatalf("reload mutated global values = %#v, %v", config.Layouts, err)
	}
}

func TestLoadConfigRejectsInvalidYAML(t *testing.T) {
	for _, test := range []struct{ data, message string }{
		{"[]", "mapping"}, {"null\n", "mapping"}, {"~\n", "mapping"}, {"panes: [", "yaml:"},
		{"!!null\n", "mapping"}, {"!!null null\n", "mapping"}, {"--- !!null\n", "mapping"},
		{"!<tag:yaml.org,2002:null>\n", "mapping"}, {"''\n", "mapping"},
		{"---\n---\n", "multiple YAML documents"}, {"---\n---\n{}", "multiple YAML documents"},
		{"{}\n---\n", "multiple YAML documents"},
		{"{}\n---\n{}", "multiple YAML documents"}, {"agent: assistant", "unsupported config field agent"},
		{"panes: []\npanes: []", "duplicate key"}, {"panes: [null]", "null list entries"},
		{"post_create: [null]", "null list entries"}, {"post_create: command", "cannot unmarshal"},
		{"sandbox: {enabled: perhaps}", "cannot unmarshal"}, {"sandbox: {target: hooks}", "sandbox.target"},
		{"sandbox: {engine: docker}", "Podman"}, {"sandbox: {container: {runtime: podman}}", "Podman"},
		{"sandbox: {backend: lima}", "Podman"},
		{"panes: [{split: ''}]", "horizontal or vertical"},
		{"panes: [{}, {split: vertical, target: -1}]", "unsigned integer"},
		{"panes: [{}, {split: diagonal}]", "horizontal or vertical"},
		{"panes: [{}, {split: vertical, size: -1}]", "0 and 65535"},
		{"panes: [{}, {split: vertical, size: 65536}]", "0 and 65535"},
		{"panes: [{}, {split: vertical, size: 1.5}]", "integer"},
		{"panes: [{}, {split: vertical, percentage: -1}]", "between 0 and 255"},
		{"panes: [{}, {split: vertical, percentage: 256}]", "between 0 and 255"},
		{"panes: [{focus: null}]", "boolean"}, {"layouts: {review: {}}", "required panes"},
		{"layouts: {review: {panes: null}}", "required panes"},
	} {
		t.Run(test.data, func(t *testing.T) {
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				dir := t.TempDir()
				path := writeConfigFixture(t, dir, filename, test.data)
				_, err := LoadConfig(dir, "project")
				if err == nil || !strings.Contains(err.Error(), test.message) || !strings.Contains(err.Error(), path) {
					t.Fatalf("error = %v, want filename and %q", err, test.message)
				}
			}
		})
	}
}

func TestLoadConfigDefersPaneSemantics(t *testing.T) {
	for _, test := range []struct{ panes, message string }{
		{"[{command: <agent>}]", "<agent> expansion"},
		{"[{command: 'exec <agent> --help'}]", "<agent> expansion"},
		{"[{name: ''}]", "name must not be empty"},
		{"[{name: '  '}]", "name must not be empty"},
		{"[{split: horizontal}]", "first pane"},
		{"[{size: 0}]", "first pane"},
		{"[{percentage: 0}]", "first pane"},
		{"[{percentage: 1}]", "first pane"},
		{"[{target: 0}]", "previously created"},
		{"[{}, {}]", "split direction must"},
		{"[{}, {split: vertical, target: 1}]", "previously created"},
		{"[{}, {split: vertical, percentage: 0}]", "between 1 and 100"},
		{"[{}, {split: vertical, percentage: 101}]", "between 1 and 100"},
		{"[{}, {split: vertical, size: 0, percentage: 30}]", "cannot both"},
		{"[{zoom: true}, {split: vertical, zoom: true}]", "at most one"},
		{"[{}, {split: stacked}]", "horizontal or vertical"},
	} {
		t.Run(test.panes, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "panes: &invalid "+test.panes+"\nlayouts: {unused: {panes: *invalid}, valid: {panes: [{}]}}")
			writeConfigFixture(t, dir, "project.yaml", "panes: [{}]")
			config, err := LoadConfig(dir, "project")
			if err != nil {
				t.Fatalf("overridden global semantics rejected: %v", err)
			}
			if panes, err := config.SelectPanes(""); err != nil || !reflect.DeepEqual(panes, []Pane{{}}) {
				t.Fatalf("effective panes = %#v, %v", panes, err)
			}
			if _, err := config.SelectPanes("unused"); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("selected invalid layout error = %v, want %q", err, test.message)
			}
			writeConfigFixture(t, dir, "project.yaml", "panes: null")
			config, err = LoadConfig(dir, "project")
			if err != nil {
				t.Fatalf("load must leave geometry validation to selection: %v", err)
			}
			if err := config.Validate(); err != nil {
				t.Fatalf("unselected geometry rejected: %v", err)
			}
			if _, err := config.SelectPanes("valid"); err != nil {
				t.Fatalf("valid layout rejected because of unused default panes: %v", err)
			}
			if _, err := config.SelectPanes(""); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("effective invalid panes error = %v, want %q", err, test.message)
			}
			for _, codec := range []struct {
				name      string
				marshal   func(any) ([]byte, error)
				unmarshal func([]byte, any) error
			}{
				{"JSON", json.Marshal, json.Unmarshal},
				{"YAML", yaml.Marshal, yaml.Unmarshal},
			} {
				data, err := codec.marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				var saved Config
				if err := codec.unmarshal(data, &saved); err != nil {
					t.Fatal(err)
				}
				if _, err := saved.SelectPanes("unused"); err == nil || !strings.Contains(err.Error(), test.message) {
					t.Fatalf("%s lost invalid optional values: %v", codec.name, err)
				}
			}
		})
	}
}

func TestLoadConfigRejectsOverriddenTypeErrors(t *testing.T) {
	for _, global := range []string{
		"panes: [{size: -1}]", "panes: [{size: 65536}]",
		"panes: [{percentage: -1}]", "panes: [{percentage: 256}]",
		"panes: [{size: 1.5}]", "panes: [{percentage: '0'}]",
		"panes: [{target: -1}]", "panes: [{split: diagonal}]",
		"panes: [{focus: null}]", "panes: [null]",
		"files: {copy: false}", "files: {copy: [null]}",
		"files: null", "sandbox: null",
		"pre_remove: &null null\nfiles: *null",
		"pre_remove: &null null\nsandbox: *null",
		"layouts: {unused: {}}", "layouts: {unused: {panes: [{percentage: 256}]}}",
		"sandbox: {target: invalid}", "sandbox: {enabled: yes}",
	} {
		t.Run(global, func(t *testing.T) {
			dir := t.TempDir()
			path := writeConfigFixture(t, dir, "config.yaml", global)
			writeConfigFixture(t, dir, "project.yaml", "panes: []\nfiles: {copy: []}\nlayouts: {unused: {panes: []}}\nsandbox: {target: agent}")
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("overridden type error = %v, want global filename", err)
			}
		})
	}
}

func TestLoadConfigDefersFilePatterns(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		for _, pattern := range []string{"[", "a/../secret"} {
			t.Run(field+"/"+pattern, func(t *testing.T) {
				dir := t.TempDir()
				root, destination := provisionTestRoots(t)
				writeConfigFixture(t, dir, "config.yaml", "files: {"+field+": ['"+pattern+"']}")
				writeConfigFixture(t, dir, "project.yaml", "files: {"+field+": []}")
				config, err := LoadConfig(dir, "project")
				if err != nil {
					t.Fatalf("overridden file pattern rejected: %v", err)
				}
				if err := ApplyFiles(t.Context(), root, destination, config.Files, nil); err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, dir, "project.yaml", "files: {"+field+": null}")
				config, err = LoadConfig(dir, "project")
				if err != nil {
					t.Fatalf("unused file pattern rejected during load: %v", err)
				}
				before := provisionSnapshot(t, destination)
				if err := ApplyFiles(t.Context(), root, destination, config.Files, nil); err == nil {
					t.Fatal("effective invalid file pattern accepted")
				}
				assertProvisionUnchanged(t, destination, before)
			})
		}
	}
}

func TestLoadConfigForRepoNodeDefaults(t *testing.T) {
	for _, lock := range []string{"pnpm-lock.yaml", "package-lock.json", "yarn.lock", "none"} {
		t.Run(lock, func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			if lock != "none" {
				writeConfigFixture(t, root, lock, "fixture")
			}
			writeConfigFixture(t, root, "CLAUDE.md", "not an agent selector")
			writeConfigFixture(t, root, ".workmux.yaml", "post_create: [must-not-load]")
			for _, hooks := range []string{"null", "[]", "[custom]"} {
				name, err := repoConfigName(filepath.Base(root))
				if err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, dir, name, "pre_remove: "+hooks)
				config, err := LoadConfigForRepo(dir, root)
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				switch hooks {
				case "null":
					if lock != "none" {
						want = []string{nodeModulesCleanupScript}
					}
				case "[]":
					want = []string{}
				case "[custom]":
					want = []string{"custom"}
				}
				if !reflect.DeepEqual(config.PreRemove, want) || config.PostCreate != nil || !reflect.DeepEqual(config.Panes, defaultPanes()) {
					t.Fatalf("root defaults = %#v, want hooks %#v", config, want)
				}
			}
		})
	}
	for _, text := range []string{"mktemp -d", "-prune -print0", "mv --", "trap - EXIT", "nohup rm -rf"} {
		if !strings.Contains(nodeModulesCleanupScript, text) {
			t.Errorf("cleanup script lacks %q", text)
		}
	}
}

func TestNodeModulesCleanupScript(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not installed")
	}
	root, trash := t.TempDir(), t.TempDir()
	for _, name := range []string{"node_modules/package/data", "frontend app/node_modules/package/data"} {
		writeProvisionFixture(t, root, name, "fixture", 0600)
	}
	writeProvisionFixture(t, root, "frontend app/keep", "keep", 0600)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, bash, "-c", nodeModulesCleanupScript)
	command.Dir = root
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + root, "TMPDIR=" + trash}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cleanup script: %v\n%s", err, output)
	}
	for _, name := range []string{"node_modules", "frontend app/node_modules"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("cleanup retained %s: %v", name, err)
		}
	}
	assertProvisionFile(t, root, "frontend app/keep", "keep")
	for {
		entries, err := os.ReadDir(trash)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("background cleanup did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLoadConfigUnreadable(t *testing.T) {
	for _, name := range []string{"config.yaml", "project.yaml"} {
		dir := t.TempDir()
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("unreadable error = %v", err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("dangling config error = %v", err)
	}
}

func TestLoadConfigRepoNames(t *testing.T) {
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "post_create: [global]")
	for _, test := range []struct{ repo, filename string }{
		{"config", "repo-config.yaml"}, {"repo-config", "repo-repo-config.yaml"},
		{"project", "project.yaml"}, {"project with spaces", "project with spaces.yaml"},
	} {
		writeConfigFixture(t, dir, test.filename, "panes: [{command: '"+test.repo+"'}]")
		config, err := LoadConfig(dir, test.repo)
		if err != nil || config.Panes[0].Command != test.repo || !reflect.DeepEqual(config.PostCreate, []string{"global"}) {
			t.Fatalf("repo %q: %#v, %v", test.repo, config, err)
		}
	}
	for _, repo := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "/root", "name\n", "na\x00me", " name"} {
		if _, err := LoadConfig(dir, repo); err == nil {
			t.Errorf("accepted repo %q", repo)
		}
	}
}

func TestSelectPanes(t *testing.T) {
	target := 0
	config := Config{Panes: []Pane{{Command: "default"}}, Layouts: map[string]Layout{
		"review": {Panes: []Pane{{Focus: true}, {Split: "vertical", Percentage: 100, Zoom: true, Target: &target}}},
		"empty":  {Panes: []Pane{}},
	}}
	panes, err := config.SelectPanes("review")
	if err != nil || !panes[1].Focus {
		t.Fatalf("selected = %#v, %v", panes, err)
	}
	panes[0].Command = "mutated"
	*panes[1].Target = 99
	if config.Layouts["review"].Panes[0].Command != "" || target != 0 {
		t.Fatal("SelectPanes aliased the config")
	}
	panes, err = config.SelectPanes("empty")
	if err != nil || panes == nil || len(panes) != 0 {
		t.Fatalf("explicit empty = %#v, %v", panes, err)
	}
	panes, err = (Config{}).SelectPanes("")
	if err != nil || !reflect.DeepEqual(panes, defaultPanes()) {
		t.Fatalf("defaults = %#v, %v", panes, err)
	}
	if _, err := config.SelectPanes("missing"); err == nil {
		t.Fatal("accepted unknown layout")
	}
}

func TestConfigSerialization(t *testing.T) {
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "panes: [{name: editor}, {split: horizontal, size: 0, target: 0}]\nsandbox: {target: all}")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config, decoded) || decoded.Panes[0].SizeSpecified() || !decoded.Panes[1].SizeSpecified() {
		t.Fatalf("JSON round trip = %#v, data %s", decoded, data)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	data, err = yaml.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	decoded = Config{}
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Panes, decoded.Panes) || decoded.Sandbox.Target != "all" {
		t.Fatalf("YAML round trip = %#v, data %s", decoded, data)
	}
	writeConfigFixture(t, dir, "config.yaml", string(data))
	reloaded, err := LoadConfig(dir, "project")
	if err != nil || !reflect.DeepEqual(config.Panes, reloaded.Panes) {
		t.Fatalf("YAML reload = %#v, %v", reloaded, err)
	}
	data, err = yaml.Marshal(SandboxConfig{Image: "fedora:44", Target: "all"})
	if err != nil || !strings.Contains(string(data), "target: all") || strings.Contains(string(data), "engine") {
		t.Fatalf("sandbox YAML = %s, %v", data, err)
	}
}

func writeConfigFixture(t *testing.T, dir, name, data string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
