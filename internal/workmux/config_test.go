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

func TestLoadConfigDefaults(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Panes, []Pane{{}}) {
		t.Fatalf("panes = %#v", config.Panes)
	}
	if config.Sandbox.Enabled || config.Sandbox.Image != "localhost/cli-workmux:fedora44" {
		t.Fatalf("sandbox = %#v", config.Sandbox)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("LoadConfig created its config directory: %v", err)
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
		name string
		got  []string
		want []string
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
		t.Fatalf("sandbox inheritance lost a field or explicit false: %#v", config.Sandbox)
	}
	if !reflect.DeepEqual(config.Layouts["review"].Panes, []Pane{{Command: "project-review"}}) {
		t.Fatalf("layout was not replaced: %#v", config.Layouts)
	}
	if !reflect.DeepEqual(config.Layouts["shell"].Panes, []Pane{{}}) {
		t.Fatalf("unmentioned layout was lost: %#v", config.Layouts)
	}
}

func TestLoadConfigSandboxFieldInheritance(t *testing.T) {
	for _, test := range []struct {
		name string
		repo string
		want SandboxConfig
	}{
		{"omitted", "{}", SandboxConfig{true, "example.com/dev:v1"}},
		{"empty object", "sandbox: {}", SandboxConfig{true, "example.com/dev:v1"}},
		{"false", "sandbox: {enabled: false}", SandboxConfig{false, "example.com/dev:v1"}},
		{"image", "sandbox: {image: localhost/dev:v2}", SandboxConfig{true, "localhost/dev:v2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "sandbox: {enabled: true, image: example.com/dev:v1}")
			writeConfigFixture(t, dir, "project.yaml", test.repo)
			config, err := LoadConfig(dir, "project")
			if err != nil {
				t.Fatal(err)
			}
			if config.Sandbox != test.want {
				t.Fatalf("sandbox = %#v, want %#v", config.Sandbox, test.want)
			}
		})
	}
}

func TestLoadConfigEmptyAndOmittedLists(t *testing.T) {
	for _, test := range []struct {
		name string
		repo string
		want []Pane
	}{
		{"omitted", "{}", []Pane{{Command: "global"}}},
		{"empty", "panes: []", []Pane{{}}},
		{"shell", "panes: [{}]", []Pane{{}}},
		{"empty command", "panes: [{command: ''}]", []Pane{{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfigFixture(t, dir, "config.yaml", "panes: [{command: global}]\nfiles: {copy: [.env], symlink: [cache]}\npost_create: [prepare]")
			writeConfigFixture(t, dir, "project.yaml", test.repo)
			config, err := LoadConfig(dir, "project")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(config.Panes, test.want) {
				t.Fatalf("panes = %#v, want %#v", config.Panes, test.want)
			}
			if !reflect.DeepEqual(config.Files, FilesConfig{Copy: []string{".env"}, Symlink: []string{"cache"}}) || !reflect.DeepEqual(config.PostCreate, []string{"prepare"}) {
				t.Fatalf("omitted lists did not inherit: %#v", config)
			}
		})
	}
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "panes: []")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(config.Panes, []Pane{{}}) {
		t.Fatalf("empty global panes = %#v", config.Panes)
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
				{"[<global>, last]", []string{"global", "last"}},
				{"[first, <global>]", []string{"first", "global"}},
				{"[first, <global>, last]", []string{"first", "global", "last"}},
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
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), "only in repository") {
				t.Fatalf("global splice error = %v", err)
			}
			dir = t.TempDir()
			writeConfigFixture(t, dir, "project.yaml", makeList("[<global>, local]"))
			if _, err := LoadConfig(dir, "project"); err != nil {
				t.Fatalf("splice without a global config: %v", err)
			}
		})
	}
}

func TestLoadConfigLayoutReplacement(t *testing.T) {
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "layouts: {review: {panes: [{command: old}]}, retained: {panes: [{command: retained}]}}")
	writeConfigFixture(t, dir, "project.yaml", "layouts: {review: {}, added: {panes: [{command: added}]}}")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Layout{
		"review":   {Panes: []Pane{{}}},
		"retained": {Panes: []Pane{{Command: "retained"}}},
		"added":    {Panes: []Pane{{Command: "added"}}},
	}
	if !reflect.DeepEqual(config.Layouts, want) {
		t.Fatalf("layouts = %#v, want %#v", config.Layouts, want)
	}
	writeConfigFixture(t, dir, "project.yaml", "layouts: {}")
	config, err = LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
	}
	if len(config.Layouts) != 2 || config.Layouts["review"].Panes[0].Command != "old" {
		t.Fatalf("empty layout map did not retain global layouts: %#v", config.Layouts)
	}
}

func TestLoadConfigRejectsInvalidYAML(t *testing.T) {
	for _, test := range []struct{ name, data, message string }{
		{"empty document", "", "empty YAML document"},
		{"comment only", "# defaults\n", "empty YAML document"},
		{"non mapping", "[]", "mapping"},
		{"malformed", "panes: [", "yaml:"},
		{"multiple documents", "{}\n---\n{}", "multiple YAML documents"},
		{"empty second document", "{}\n---\n", "multiple YAML documents"},
		{"unknown", "other: true", "field other"},
		{"agent", "agent: assistant", "field agent"},
		{"nested unknown", "sandbox: {command: podman}", "field command"},
		{"file unknown", "files: {move: [.env]}", "field move"},
		{"layout unknown", "layouts: {review: {command: editor}}", "field command"},
		{"pane unknown", "panes: [{agent: assistant}]", "field agent"},
		{"duplicate", "panes: []\npanes: []", "duplicate key"},
		{"nested duplicate", "sandbox: {enabled: true, enabled: false}", "duplicate key"},
		{"layout duplicate", "layouts: {a: {}, a: {}}", "duplicate key"},
		{"null document", "null", "null or missing"},
		{"empty value", "panes:", "null or missing"},
		{"null list", "files: {copy: null}", "null or missing"},
		{"null pane", "panes: [null]", "null or missing"},
		{"null command", "panes: [{command: null}]", "null or missing"},
		{"null hook", "post_create: [null]", "null or missing"},
		{"null layout", "layouts: {review: null}", "null or missing"},
		{"null bool", "sandbox: {enabled: null}", "null or missing"},
		{"anchor", "panes: &p []", "anchors"},
		{"merge", "<<: {panes: []}", "merge keys"},
		{"numeric key", "1: []", "keys must be strings"},
		{"wrong list", "post_create: command", "cannot unmarshal"},
		{"wrong bool", "sandbox: {enabled: perhaps}", "cannot unmarshal"},
		{"agent placeholder", "panes: [{command: <agent>}]", "<agent> expansion is not supported"},
		{"agent missing space", "panes: [command:<agent>]", "<agent> expansion is not supported"},
		{"agent in command", "panes: [{command: 'exec <agent> --help'}]", "explicit command"},
		{"agent hook", "post_create: [<agent>]", "<agent> expansion"},
		{"global pane", "panes: [{command: <global>}]", "separate repository"},
		{"embedded global", "post_create: ['echo <global>']", "separate repository"},
		{"empty hook", "post_create: ['']", "must not be empty"},
		{"empty pattern", "files: {copy: ['']}", "relative path"},
		{"absolute pattern", "files: {copy: [/tmp/secret]}", "relative path"},
		{"traversal pattern", "files: {symlink: [a/../secret]}", "path components"},
		{"git pattern", "files: {copy: [a/.git/config]}", ".git"},
		{"malformed glob", "files: {copy: ['[']}", "syntax"},
		{"empty image", "sandbox: {image: ''}", "sandbox.image"},
		{"bad image", "sandbox: {image: 'image; touch /tmp/pwn'}", "sandbox.image"},
		{"url image", "sandbox: {image: https://example.com/image}", "sandbox.image"},
		{"short digest", "sandbox: {image: 'image@sha256:abc'}", "sandbox.image"},
		{"legacy podman", "sandbox: {container: {runtime: podman}}", "sandbox always uses Podman; remove container and keep only enabled and image"},
		{"legacy docker", "sandbox: {container: {runtime: docker}}", "sandbox.container"},
		{"legacy empty runtime", "sandbox: {container: {runtime: ''}}", "sandbox.container"},
		{"legacy empty container", "sandbox: {container: {}}", "sandbox.container"},
		{"legacy disabled", "sandbox: {enabled: false, container: {runtime: podman}}", "sandbox.container"},
		{"runtime", "sandbox: {runtime: podman}", "field runtime"},
		{"bad split", "panes: [{}, {split: diagonal}]", "horizontal or vertical"},
		{"first split", "panes: [{split: horizontal}]", "first pane"},
		{"zero size", "panes: [{}, {size: 0}]", "size must be greater"},
		{"negative size", "panes: [{}, {size: -1}]", "size must be greater"},
		{"fractional size", "panes: [{}, {size: 1.5}]", "unsupported YAML scalar type"},
		{"fractional percentage", "panes: [{}, {percentage: 30.5}]", "unsupported YAML scalar type"},
		{"custom scalar tag", "panes: [{command: !custom editor}]", "unsupported YAML scalar type"},
		{"zero percentage", "panes: [{}, {percentage: 0}]", "percentage must be between"},
		{"negative percentage", "panes: [{}, {percentage: -1}]", "percentage must be between"},
		{"large percentage", "panes: [{}, {percentage: 101}]", "percentage must be between"},
		{"both sizes", "panes: [{}, {size: 10, percentage: 30}]", "cannot both"},
		{"multiple focus", "panes: [{focus: true}, {focus: true}]", "at most one"},
		{"multiple zoom", "panes: [{zoom: true}, {zoom: true}]", "at most one"},
		{"layout zero size", "layouts: {review: {panes: [{}, {size: 0}]}}", "size must be greater"},
		{"layout invalid split", "layouts: {review: {panes: [{split: vertical}]}}", "first pane"},
		{"empty layout name", "layouts: {'': {panes: []}}", "layout name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, filename := range []string{"config.yaml", "project.yaml"} {
				dir := t.TempDir()
				path := writeConfigFixture(t, dir, filename, test.data)
				_, err := LoadConfig(dir, "project")
				if err == nil || !strings.Contains(err.Error(), test.message) || !strings.Contains(err.Error(), path) {
					t.Fatalf("%s: error = %v, want filename and %q", filename, err, test.message)
				}
			}
		})
	}
}

func TestLoadConfigUnreadable(t *testing.T) {
	for _, name := range []string{"config.yaml", "project.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
				t.Fatalf("unreadable config error = %v", err)
			}
		})
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.Symlink(filepath.Join(dir, "missing"), path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("dangling config symlink error = %v", err)
	}
	t.Run("permissions", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can read mode 0000 files")
		}
		dir := t.TempDir()
		path := writeConfigFixture(t, dir, "config.yaml", "{}")
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := os.Chmod(path, 0600); err != nil {
				t.Error(err)
			}
		})
		if _, err := LoadConfig(dir, "project"); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("permission error = %v", err)
		}
	})
}

func TestLoadConfigRepoNames(t *testing.T) {
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "post_create: [global]")
	for _, test := range []struct{ repo, filename string }{
		{"config", "repo-config.yaml"},
		{"repo-config", "repo-repo-config.yaml"},
		{"repo-repo-config", "repo-repo-repo-config.yaml"},
		{"repo-project", "repo-repo-project.yaml"},
		{"project", "project.yaml"},
		{"project with spaces", "project with spaces.yaml"},
		{"project..name", "project..name.yaml"},
	} {
		writeConfigFixture(t, dir, test.filename, "panes: [{command: '"+test.repo+"'}]")
	}
	for _, repo := range []string{"config", "repo-config", "repo-repo-config", "repo-project", "project", "project with spaces", "project..name"} {
		config, err := LoadConfig(dir, repo)
		if err != nil {
			t.Fatal(err)
		}
		if config.Panes[0].Command != repo || !reflect.DeepEqual(config.PostCreate, []string{"global"}) {
			t.Fatalf("repo %q resolved incorrectly: %#v", repo, config)
		}
	}
	for _, repo := range []string{"", ".", "..", "../outside", "a/b", `a\b`, "/root", "name\n", "na\rme", "na\x00me", " name", "name "} {
		if _, err := LoadConfig(dir, repo); err == nil || !strings.Contains(err.Error(), "plain basename") {
			t.Errorf("repo %q error = %v", repo, err)
		}
	}
}

func TestSelectPanes(t *testing.T) {
	config := Config{
		Panes: []Pane{{Command: "default"}},
		Layouts: map[string]Layout{
			"review": {Panes: []Pane{{Command: "review"}, {Split: "vertical", Percentage: 100, Focus: true, Zoom: true}}},
			"shell":  {},
		},
	}
	for _, test := range []struct {
		layout string
		want   []Pane
	}{
		{"", config.Panes},
		{"review", config.Layouts["review"].Panes},
		{"shell", []Pane{{}}},
	} {
		panes, err := config.SelectPanes(test.layout)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(panes, test.want) {
			t.Fatalf("layout %q = %#v, want %#v", test.layout, panes, test.want)
		}
		panes[0].Command = "mutated"
		if len(test.want) > 0 && test.want[0].Command == "mutated" {
			t.Fatal("SelectPanes returned an aliased slice")
		}
	}
	if _, err := config.SelectPanes("missing"); err == nil || !strings.Contains(err.Error(), "unknown layout") {
		t.Fatalf("unknown layout error = %v", err)
	}
	panes, err := (Config{}).SelectPanes("")
	if err != nil || !reflect.DeepEqual(panes, []Pane{{}}) {
		t.Fatalf("zero config shell = %#v, %v", panes, err)
	}
	if _, err := (Config{Panes: []Pane{{Command: "<agent>"}}}).SelectPanes(""); err == nil {
		t.Fatal("SelectPanes accepted <agent>")
	}
}

func TestConfigValidate(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*Config)
	}{
		{"negative size", func(c *Config) { c.Panes = []Pane{{}, {Size: -1}} }},
		{"negative percentage", func(c *Config) { c.Panes = []Pane{{}, {Percentage: -1}} }},
		{"both sizes", func(c *Config) { c.Panes = []Pane{{}, {Size: 1, Percentage: 1}} }},
		{"null byte", func(c *Config) { c.Panes = []Pane{{Command: "echo\x00oops"}} }},
		{"placeholder", func(c *Config) { c.PreMerge = []string{"<global>"} }},
		{"empty image", func(c *Config) { c.Sandbox.Image = "" }},
		{"layout", func(c *Config) { c.Layouts["bad"] = Layout{Panes: []Pane{{Split: "vertical"}}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := LoadConfig(t.TempDir(), "project")
			if err != nil {
				t.Fatal(err)
			}
			test.change(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("Validate accepted invalid config")
			}
		})
	}
	for _, image := range []string{"fedora:44", "localhost/cli-workmux:fedora44", "localhost:5000/team/dev:Latest", "docker.io/library/fedora", "ghcr.io/team/image@sha256:" + strings.Repeat("a", 64)} {
		config, err := LoadConfig(t.TempDir(), "project")
		if err != nil {
			t.Fatal(err)
		}
		config.Sandbox.Image = image
		if err := config.Validate(); err != nil {
			t.Errorf("image %q: %v", image, err)
		}
	}
}

func TestConfigSerializationTags(t *testing.T) {
	for _, value := range []any{Config{}, Pane{}, FilesConfig{}, Layout{}, SandboxConfig{}} {
		typeOf := reflect.TypeOf(value)
		for field := range typeOf.Fields() {
			yamlTag, jsonTag := field.Tag.Get("yaml"), field.Tag.Get("json")
			if yamlTag == "" || yamlTag != jsonTag || strings.ToLower(yamlTag) != yamlTag || strings.Contains(yamlTag, ",") {
				t.Errorf("%s.%s tags = %q, %q", typeOf.Name(), field.Name, yamlTag, jsonTag)
			}
		}
	}
	dir := t.TempDir()
	writeConfigFixture(t, dir, "config.yaml", "panes: [{command: editor, focus: true, zoom: true}, {split: horizontal, size: 20}]\npost_create: [prepare]\npre_merge: [test]\npre_remove: [clean]")
	config, err := LoadConfig(dir, "project")
	if err != nil {
		t.Fatal(err)
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
		if strings.Contains(string(data), "container") || strings.Contains(string(data), "runtime") {
			t.Fatalf("%s wrote obsolete sandbox settings: %s", codec.name, data)
		}
		for _, key := range []string{"post_create", "pre_merge", "pre_remove"} {
			if !strings.Contains(string(data), key) {
				t.Fatalf("%s lost key %q: %s", codec.name, key, data)
			}
		}
		var decoded Config
		if err := codec.unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded.Panes, config.Panes) || decoded.Sandbox != config.Sandbox || !reflect.DeepEqual(decoded.PreRemove, config.PreRemove) {
			t.Fatalf("%s round trip changed config: %#v", codec.name, decoded)
		}
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
