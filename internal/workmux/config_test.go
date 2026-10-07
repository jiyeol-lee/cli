package workmux

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

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

func TestLoadConfigAudio(t *testing.T) {
	for _, test := range []struct {
		name, global, repo string
		want               bool
	}{
		{"default", "{}", "{}", false},
		{"global", "sandbox: {audio: true}", "{}", true},
		{"empty", "sandbox: {audio: true}", "sandbox: {}", true},
		{"null", "sandbox: {audio: true}", "sandbox: {audio: null}", true},
		{"false", "sandbox: {audio: true}", "sandbox: {audio: false}", false},
		{"true", "sandbox: {audio: false}", "sandbox: {audio: true}", true},
		{"independent", "sandbox: {enabled: true, audio: true}", "sandbox: {enabled: false}", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", test.global)
			writeConfigFixture(t, dir, "project.yaml", test.repo)
			config, err := LoadConfig(dir, "project")
			if err != nil || config.Sandbox.Audio != test.want {
				t.Fatalf("audio = %t, %v, want %t", config.Sandbox.Audio, err, test.want)
			}
			data, err := yaml.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			layer, err := decodeConfig(data)
			if err != nil || layer.Sandbox.Audio == nil || *layer.Sandbox.Audio != test.want {
				t.Fatalf("YAML round trip = %s, %v", data, err)
			}
		})
	}
	for _, value := range []string{"yes", "1", "'true'", "[]", "{}"} {
		t.Run(value, func(t *testing.T) {
			if _, err := decodeConfig([]byte("sandbox: {audio: " + value + "}")); err == nil || !strings.Contains(err.Error(), "sandbox.audio as a boolean") {
				t.Fatalf("accepted audio %s: %v", value, err)
			}
		})
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
			if err != nil || !reflect.DeepEqual(panes, defaultPanes()) || config.PreRemove != nil {
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

func TestLoadConfigSandboxPaneDefaultsAfterInheritance(t *testing.T) {
	host := []Pane{{Focus: true}, {Command: "clear", Split: "horizontal"}}
	sandbox := []Pane{{Command: "opencode", Focus: true}, {Split: "horizontal"}}
	for _, test := range []struct {
		name, global, repo string
		want               []Pane
	}{
		{"global enabled", "sandbox: {enabled: true}", "{}", sandbox},
		{"repository enabled", "{}", "sandbox: {enabled: true}", sandbox},
		{"repository disables", "sandbox: {enabled: true}", "sandbox: {enabled: false}", host},
		{"null inherits", "sandbox: {enabled: true}", "sandbox: {enabled: null}\npanes: null", sandbox},
		{"global panes override", "panes: [{command: nvim}]", "sandbox: {enabled: true}", []Pane{{Command: "nvim"}}},
		{"global empty overrides", "panes: []", "sandbox: {enabled: true}", []Pane{}},
		{"repository panes override", "sandbox: {enabled: true}", "panes: [{focus: true}]", []Pane{{Focus: true}}},
		{"repository empty overrides", "sandbox: {enabled: true}", "panes: []", []Pane{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			name, err := repoConfigName(filepath.Base(root))
			if err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, dir, "config.yaml", test.global)
			writeConfigFixture(t, dir, name, test.repo)
			for _, load := range []func() (Config, error){
				func() (Config, error) { return LoadConfig(dir, filepath.Base(root)) },
				func() (Config, error) { return LoadConfigForRepo(dir, root) },
			} {
				config, err := load()
				if err != nil || !reflect.DeepEqual(config.Panes, test.want) {
					t.Fatalf("panes = %#v, %v; want %#v", config.Panes, err, test.want)
				}
				panes, err := config.SelectPanes("")
				if err != nil || !reflect.DeepEqual(panes, test.want) {
					t.Fatalf("selected panes = %#v, %v; want %#v", panes, err, test.want)
				}
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
		{"omitted", "{}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1"}},
		{"empty", "sandbox: {}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1"}},
		{"null", "sandbox: {enabled: null, image: null}", SandboxConfig{Enabled: true, Image: "example.com/dev:v1"}},
		{"false", "sandbox: {enabled: false}", SandboxConfig{Image: "example.com/dev:v1"}},
		{"image", "sandbox: {image: localhost/dev:v2}", SandboxConfig{Enabled: true, Image: "localhost/dev:v2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "sandbox: {enabled: true, image: example.com/dev:v1}")
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
	if config.Layouts["explicit"].Panes[0].Command != "explicit-shell" {
		t.Fatalf("explicit layout = %#v", config.Layouts)
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
		{"sandbox: {enabled: perhaps}", "cannot unmarshal"},
		{"sandbox: {engine: docker}", "Podman"}, {"sandbox: {container: {runtime: podman}}", "Podman"},
		{"sandbox: {backend: lima}", "Podman"},
		{"panes: [{split: ''}]", "horizontal or vertical"},
		{"layouts: {unused: {panes: [{split: ''}]}}", "horizontal or vertical"},
		{"panes: [{}, {split: vertical, target: -1}]", "unsigned integer"},
		{"panes: [{}, {split: diagonal}]", "horizontal or vertical"},
		{"panes: [{}, {split: vertical, size: -1}]", "0 and 65535"},
		{"panes: [{}, {split: vertical, size: 65536}]", "0 and 65535"},
		{"panes: [{}, {split: vertical, size: 1.5}]", "integer"},
		{"panes: [{}, {split: vertical, percentage: -1}]", "between 1 and 100"},
		{"panes: [{}, {split: vertical, percentage: 256}]", "between 1 and 100"},
		{"panes: [{}, {split: vertical, percentage: 1.5}]", "integer"},
		{"panes: [{}, {split: vertical, percentage: '1'}]", "integer"},
		{"panes: [{}, {split: vertical, percentage: true}]", "integer"},
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

func TestLoadConfigRejectsUnknownNestedFields(t *testing.T) {
	for _, test := range []struct{ data, field, line string }{
		{"sandbox:\n  enabeld: true\n", "config.sandbox.enabeld", "line 2"},
		{"files:\n  copi: [.env]\n", "config.files.copi", "line 2"},
		{"panes:\n  - comand: editor\n", "config.panes[0].comand", "line 2"},
		{"layouts:\n  unused:\n    pane: []\n", "config.layouts.unused.pane", "line 3"},
		{"layouts:\n  unused:\n    panes:\n      - comand: editor\n", "config.layouts.unused.panes[0].comand", "line 4"},
		{"files: {copy: &values []}\nsandbox: &settings {enabled: true}\nlayouts: {unused: {panes: [&pane {command: shell}]}}\npanes: [*settings]\n", "config.panes[0].enabled", "line 2"},
	} {
		t.Run(test.field, func(t *testing.T) {
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				dir := t.TempDir()
				path := writeConfigFixture(t, dir, filename, test.data)
				if filename == "config.yaml" {
					writeConfigFixture(t, dir, "project.yaml", "panes: []\nfiles: {copy: []}\nsandbox: {enabled: false}\nlayouts: {unused: {panes: []}}")
				}
				_, err := LoadConfig(dir, "project")
				if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), test.line) {
					t.Fatalf("error = %v, want filename, %s and %s", err, test.field, test.line)
				}
			}
		})
	}
}

func TestConfigValidateUnusedLayouts(t *testing.T) {
	config := Config{Panes: []Pane{{}}, Layouts: map[string]Layout{
		"unused": {Panes: []Pane{{}, {}}},
	}}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "layouts.unused.panes") {
		t.Fatalf("invalid unused layout accepted: %v", err)
	}
}

func TestLoadConfigRejectsInvalidSandboxImageSyntax(t *testing.T) {
	for _, image := range []string{"", "-option", "has space", "has\ttab", "has\nnewline", "has\rcarriage", "has\x00nul"} {
		t.Run(image, func(t *testing.T) {
			quoted, err := json.Marshal(image)
			if err != nil {
				t.Fatal(err)
			}
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				dir := t.TempDir()
				path := writeConfigFixture(t, dir, filename, "sandbox:\n  enabled: true\n  image: "+string(quoted)+"\n")
				if filename == "config.yaml" {
					writeConfigFixture(t, dir, "project.yaml", "sandbox: {enabled: false, image: localhost/valid:tag}")
				}
				_, err := LoadConfig(dir, "project")
				for _, want := range []string{path, "sandbox.image", "line 3", "explicit image name"} {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %v, want %q", err, want)
					}
				}
			}
		})
	}
	config := Config{Sandbox: SandboxConfig{Enabled: true, Image: "localhost/not-installed:tag"}}
	if err := config.Validate(); err != nil {
		t.Fatalf("image syntax validation must not inspect Podman: %v", err)
	}
}

func TestLoadConfigPaneSplitAndPercentageBounds(t *testing.T) {
	for _, split := range []string{"horizontal", "vertical"} {
		for _, test := range []struct {
			name, fields string
			want         Pane
		}{
			{"omitted", "", Pane{}},
			{"null", ", size: null, percentage: null, target: null", Pane{}},
			{"zero size", ", size: 0", Pane{sizeSet: true}},
			{"minimum percentage", ", percentage: 1", Pane{Percentage: 1}},
			{"maximum percentage", ", percentage: 100", Pane{Percentage: 100}},
		} {
			t.Run(split+"/"+test.name, func(t *testing.T) {
				for _, filename := range []string{"config.yaml", "project.yaml"} {
					for _, nested := range []bool{false, true} {
						data := "panes: [{split: null}, {split: " + split + test.fields + "}]"
						layout := ""
						if nested {
							data = "layouts: {unused: {" + data + "}}"
							layout = "unused"
						}
						dir := t.TempDir()
						writeConfigFixture(t, dir, filename, data)
						config, err := LoadConfig(dir, "project")
						if err != nil {
							t.Fatalf("%s nested=%v: %v", filename, nested, err)
						}
						want := test.want
						want.Split = split
						panes, err := config.SelectPanes(layout)
						if err != nil || !reflect.DeepEqual(panes, []Pane{{}, want}) {
							t.Fatalf("%s nested=%v: panes = %#v, %v; want %#v", filename, nested, panes, err, want)
						}
					}
				}
			})
		}
	}
}

func TestLoadConfigPaneErrorLocations(t *testing.T) {
	for _, test := range []struct{ pane, message string }{
		{"size: 1.5", "integer"},
		{"percentage: -1", "between 1 and 100"},
		{"percentage: 0", "between 1 and 100"},
		{"percentage: 256", "between 1 and 100"},
		{"focus: null", "boolean"},
		{"split: diagonal", "horizontal or vertical"},
		{"split: stacked", "horizontal or vertical"},
		{"split: ''", "horizontal or vertical"},
		{"name: ''", "name must not be empty"},
		{"percentage: 101", "between 1 and 100"},
		{"target: 1", "previously created pane"},
	} {
		t.Run(test.pane, func(t *testing.T) {
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				for _, nested := range []bool{false, true} {
					data := "panes:\n  - command: shell\n  - split: vertical\n    " + test.pane + "\n"
					field, line := "panes[1]", "line 3"
					if nested {
						data = "layouts:\n  unused:\n    panes:\n      - command: shell\n      - split: vertical\n        " + test.pane + "\n"
						field, line = "layouts.unused.panes[1]", "line 5"
					}
					// Avoid duplicate split keys in the split-value case.
					if strings.HasPrefix(test.pane, "split:") {
						data = strings.Replace(data, "split: vertical", "command: shell", 1)
					}
					dir := t.TempDir()
					path := writeConfigFixture(t, dir, filename, data)
					if filename == "config.yaml" {
						writeConfigFixture(t, dir, "project.yaml", "panes: []\nlayouts: {unused: {panes: []}}")
					}
					_, err := LoadConfig(dir, "project")
					for _, want := range []string{path, field, line, test.message} {
						if err == nil || !strings.Contains(err.Error(), want) {
							t.Fatalf("error = %v, want %q", err, want)
						}
					}
				}
			}
		})
	}
}

func TestLoadConfigRejectsYAMLMergeKeys(t *testing.T) {
	for _, test := range []struct{ data, field, line string }{
		{"<<: {panes: []}\n", "config.<<", "line 1"},
		{"<<: {unknown_setting: true}\n", "config.<<", "line 1"},
		{"files:\n  <<: {copy: [.env], copi: [secret]}\n", "config.files.<<", "line 2"},
		{"sandbox:\n  <<: {enabled: true, enabeld: true}\n", "config.sandbox.<<", "line 2"},
		{"layouts:\n  <<: {unused: {panes: []}}\n", "config.layouts.<<", "line 2"},
		{"layouts:\n  unused:\n    <<: {panes: [], pane: []}\n", "config.layouts.unused.<<", "line 3"},
		{"panes:\n  - <<: {command: shell, comand: ignored}\n", "config.panes[0].<<", "line 2"},
		{"layouts:\n  unused:\n    panes:\n      - <<: {command: shell}\n", "config.layouts.unused.panes[0].<<", "line 4"},
		{"panes:\n  - '<<': {command: shell}\n", "config.panes[0].<<", "line 2"},
		{"panes: &panes\n  - &pane\n    <<: {command: shell}\nlayouts: {unused: {panes: *panes}, alias: {panes: [*pane]}}\n", "config.panes[0].<<", "line 3"},
	} {
		t.Run(test.data, func(t *testing.T) {
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				dir := t.TempDir()
				path := writeConfigFixture(t, dir, filename, test.data)
				if filename == "config.yaml" {
					writeConfigFixture(t, dir, "project.yaml", "panes: []\nfiles: {copy: []}\nsandbox: {enabled: false}\nlayouts: {unused: {panes: []}}")
				}
				_, err := LoadConfig(dir, "project")
				for _, want := range []string{path, test.field, test.line, "YAML merge keys are unsupported"} {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %v, want %q", err, want)
					}
				}
			}
		})
	}
}

func TestLoadConfigRejectsMaskedSemanticErrors(t *testing.T) {
	for _, test := range []struct{ global, repo, field string }{
		{"panes: [{}, {}]", "panes: [{}]", "panes"},
		{"layouts: {unused: {panes: [{}, {}]}}", "layouts: {unused: {panes: []}}", "layouts.unused.panes"},
		{"files: {copy: ['[']}", "files: {copy: []}", "files.copy"},
		{"files: {symlink: ['a/../secret']}", "files: {symlink: []}", "files.symlink"},
		{"post_create: ['<agent>']", "post_create: []", "post_create"},
		{"pre_merge: ['exec <agent>']", "pre_merge: []", "pre_merge"},
		{"pre_remove: [\"bad\\0command\"]", "pre_remove: []", "pre_remove"},
	} {
		t.Run(test.field, func(t *testing.T) {
			dir := t.TempDir()
			path := writeConfigFixture(t, dir, "config.yaml", test.global)
			writeConfigFixture(t, dir, "project.yaml", test.repo)
			_, err := LoadConfig(dir, "project")
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), test.field) || !strings.Contains(err.Error(), "line 1") {
				t.Fatalf("masked semantic error = %v, want source filename, %s and line", err, test.field)
			}
		})
	}
}

func TestLoadConfigValidatesAllPaneSemantics(t *testing.T) {
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
		{"[{}, {split: vertical, percentage: -1}]", "between 1 and 100"},
		{"[{}, {split: vertical, percentage: 101}]", "between 1 and 100"},
		{"[{}, {split: vertical, percentage: 256}]", "between 1 and 100"},
		{"[{}, {split: vertical, size: 0, percentage: 30}]", "cannot both"},
		{"[{zoom: true}, {split: vertical, zoom: true}]", "at most one"},
		{"[{}, {split: stacked}]", "horizontal or vertical"},
		{"[{}, {split: diagonal}]", "horizontal or vertical"},
	} {
		t.Run(test.panes, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "panes: &invalid "+test.panes+"\nlayouts: {unused: {panes: *invalid}, valid: {panes: [{}]}}")
			writeConfigFixture(t, dir, "project.yaml", "panes: [{}]")
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("masked invalid global panes error = %v, want %q", err, test.message)
			}
			writeConfigFixture(t, dir, "config.yaml", "panes: [{}]\nlayouts: {unused: {panes: "+test.panes+"}}")
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), "layouts.unused.panes") || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("unused invalid layout error = %v, want %q", err, test.message)
			}
			writeConfigFixture(t, dir, "config.yaml", "panes: "+test.panes)
			writeConfigFixture(t, dir, "project.yaml", "panes: null")
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("invalid default panes error = %v, want %q", err, test.message)
			}
			layer, err := decodeConfig([]byte("panes: " + test.panes))
			if err != nil {
				t.Fatal(err)
			}
			config := Config{Layouts: map[string]Layout{"unused": {Panes: decodePanes(*layer.Panes)}}}
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
			writeConfigFixture(t, dir, "project.yaml", "panes: []\nfiles: {copy: []}\nlayouts: {unused: {panes: []}}\nsandbox: {enabled: false}")
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("overridden type error = %v, want global filename", err)
			}
		})
	}
}

func TestLoadConfigValidatesEffectiveFilePatterns(t *testing.T) {
	for _, field := range []string{"copy", "symlink"} {
		for _, pattern := range []string{"[", "a/../secret"} {
			t.Run(field+"/"+pattern, func(t *testing.T) {
				dir := t.TempDir()
				root, destination := provisionTestRoots(t)
				writeConfigFixture(t, dir, "config.yaml", "files: {"+field+": ['"+pattern+"']}")
				writeConfigFixture(t, dir, "project.yaml", "files: {"+field+": []}")
				config, err := LoadConfig(dir, "project")
				if err == nil || !strings.Contains(err.Error(), "config.yaml") {
					t.Fatalf("masked invalid global file pattern accepted: %v", err)
				}
				writeConfigFixture(t, dir, "config.yaml", "files: {"+field+": []}")
				config, err = LoadConfig(dir, "project")
				if err != nil {
					t.Fatal(err)
				}
				if err := ApplyFiles(t.Context(), root, destination, config.Files, nil); err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, dir, "project.yaml", "files: {"+field+": null}")
				writeConfigFixture(t, dir, "config.yaml", "files: {"+field+": ['"+pattern+"']}")
				before := provisionSnapshot(t, destination)
				if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), "files."+field) {
					t.Fatal("effective invalid file pattern accepted")
				}
				assertProvisionUnchanged(t, destination, before)
			})
		}
	}
}

func TestLoadConfigForRepoNeverAddsImplicitHooks(t *testing.T) {
	for _, lock := range []string{"pnpm-lock.yaml", "package-lock.json", "yarn.lock", "none"} {
		t.Run(lock, func(t *testing.T) {
			dir, root := t.TempDir(), t.TempDir()
			if lock != "none" {
				writeConfigFixture(t, root, lock, "fixture")
			}
			writeConfigFixture(t, root, "CLAUDE.md", "not an agent selector")
			writeConfigFixture(t, root, ".workmux.yaml", "post_create: [must-not-load]")
			for _, hooks := range []string{"", "null", "[]", "[custom]"} {
				name, err := repoConfigName(filepath.Base(root))
				if err != nil {
					t.Fatal(err)
				}
				data := "{}"
				if hooks != "" {
					data = "pre_remove: " + hooks
				}
				writeConfigFixture(t, dir, name, data)
				config, err := LoadConfigForRepo(dir, root)
				if err != nil {
					t.Fatal(err)
				}
				var want []string
				switch hooks {
				case "[]":
					want = []string{}
				case "[custom]":
					want = []string{"custom"}
				}
				if !reflect.DeepEqual(config.PreRemove, want) || config.PostCreate != nil || config.PreMerge != nil || !reflect.DeepEqual(config.Panes, defaultPanes()) {
					t.Fatalf("root defaults = %#v, want hooks %#v", config, want)
				}
			}
		})
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
	for _, enabled := range []bool{false, true} {
		config := Config{Sandbox: SandboxConfig{Enabled: enabled}, Layouts: map[string]Layout{
			"omitted": {},
			"empty":   {Panes: []Pane{}},
		}}
		want := defaultPanes()
		if enabled {
			want = []Pane{{Command: "opencode", Focus: true}, {Split: "horizontal"}}
		}
		for _, layout := range []string{"", "omitted", "empty"} {
			expected := want
			if layout == "empty" {
				expected = []Pane{}
			}
			panes, err := config.SelectPanes(layout)
			if err != nil || !reflect.DeepEqual(panes, expected) {
				t.Fatalf("enabled=%v layout=%q panes = %#v, %v", enabled, layout, panes, err)
			}
		}
		config.Panes = []Pane{}
		panes, err := config.SelectPanes("")
		if err != nil || panes == nil || len(panes) != 0 {
			t.Fatalf("enabled=%v explicit empty panes = %#v, %v", enabled, panes, err)
		}
	}
	if _, err := config.SelectPanes("missing"); err == nil {
		t.Fatal("accepted unknown layout")
	}
}

func TestConfigSerialization(t *testing.T) {
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "panes: [{name: editor}, {split: horizontal, size: 0, target: 0}]\nsandbox: {enabled: true}")
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
	if !reflect.DeepEqual(config.Panes, decoded.Panes) || !decoded.Sandbox.Enabled {
		t.Fatalf("YAML round trip = %#v, data %s", decoded, data)
	}
	writeConfigFixture(t, dir, "config.yaml", string(data))
	reloaded, err := LoadConfig(dir, "project")
	if err != nil || !reflect.DeepEqual(config.Panes, reloaded.Panes) {
		t.Fatalf("YAML reload = %#v, %v", reloaded, err)
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
