package workmux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Panes      []Pane            `yaml:"panes" json:"panes"`
	Files      FilesConfig       `yaml:"files" json:"files"`
	Layouts    map[string]Layout `yaml:"layouts" json:"layouts"`
	PostCreate []string          `yaml:"post_create" json:"post_create"`
	PreMerge   []string          `yaml:"pre_merge" json:"pre_merge"`
	PreRemove  []string          `yaml:"pre_remove" json:"pre_remove"`
	Sandbox    SandboxConfig     `yaml:"sandbox" json:"sandbox"`
}

type Pane struct {
	Command    string `yaml:"command" json:"command"`
	Split      string `yaml:"split" json:"split"`
	Size       int    `yaml:"size" json:"size"`
	Percentage int    `yaml:"percentage" json:"percentage"`
	Focus      bool   `yaml:"focus" json:"focus"`
	Zoom       bool   `yaml:"zoom" json:"zoom"`
}

type FilesConfig struct {
	Copy    []string `yaml:"copy" json:"copy"`
	Symlink []string `yaml:"symlink" json:"symlink"`
}

type Layout struct {
	Panes []Pane `yaml:"panes" json:"panes"`
}

type SandboxConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Image   string `yaml:"image" json:"image"`
}

type configFile struct {
	Panes      *[]configPane           `yaml:"panes"`
	Files      *configFiles            `yaml:"files"`
	Layouts    map[string]configLayout `yaml:"layouts"`
	PostCreate *[]string               `yaml:"post_create"`
	PreMerge   *[]string               `yaml:"pre_merge"`
	PreRemove  *[]string               `yaml:"pre_remove"`
	Sandbox    *configSandbox          `yaml:"sandbox"`
}

type configPane struct {
	Command    string `yaml:"command"`
	Split      string `yaml:"split"`
	Size       *int   `yaml:"size"`
	Percentage *int   `yaml:"percentage"`
	Focus      bool   `yaml:"focus"`
	Zoom       bool   `yaml:"zoom"`
}

type configFiles struct {
	Copy    *[]string `yaml:"copy"`
	Symlink *[]string `yaml:"symlink"`
}

type configLayout struct {
	Panes []configPane `yaml:"panes"`
}

type configSandbox struct {
	Enabled *bool   `yaml:"enabled"`
	Image   *string `yaml:"image"`
}

func LoadConfig(configDir, repoName string) (Config, error) {
	name, err := repoConfigName(repoName)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Panes:   []Pane{{}},
		Layouts: make(map[string]Layout),
		Sandbox: SandboxConfig{Image: "localhost/cli-workmux:fedora44"},
	}
	for i, name := range []string{"config.yaml", name} {
		path := filepath.Join(configDir, name)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			// A dangling link is an unreadable config, not a missing config.
			if _, linkErr := os.Lstat(path); errors.Is(linkErr, os.ErrNotExist) {
				continue
			}
		}
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		layer, err := decodeConfig(data)
		if err == nil {
			err = mergeConfig(&config, layer, i == 1)
		}
		if err == nil {
			err = config.Validate()
		}
		if err != nil {
			return Config{}, fmt.Errorf("load config %s: %w", path, err)
		}
	}
	return config, nil
}

func repoConfigName(name string) (string, error) {
	if name == "" || name == "." || name == ".." || strings.TrimSpace(name) != name ||
		strings.ContainsAny(name, `/\`) || strings.ContainsFunc(name, unicode.IsControl) {
		return "", fmt.Errorf("invalid repository name %q: expected a plain basename", name)
	}
	// Escape the reserved name and every escape-prefixed name, so the mapping is injective.
	if name == "config" || strings.HasPrefix(name, "repo-") {
		name = "repo-" + name
	}
	return name + ".yaml", nil
}

func decodeConfig(data []byte) (configFile, error) {
	var node yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&node); err != nil {
		if errors.Is(err, io.EOF) {
			return configFile{}, fmt.Errorf("empty YAML document: use {} for defaults")
		}
		return configFile{}, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return configFile{}, err
		}
		return configFile{}, fmt.Errorf("multiple YAML documents are not allowed")
	}
	if err := validateConfigNode(&node, "config"); err != nil {
		return configFile{}, err
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return configFile{}, fmt.Errorf("config must be a YAML mapping; use {} for defaults")
	}
	var layer configFile
	decoder = yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&layer); err != nil {
		return configFile{}, err
	}
	return layer, nil
}

func validateConfigNode(node *yaml.Node, path string) error {
	if node.Tag == "!!null" {
		return fmt.Errorf("%s at line %d: null or missing values are not allowed; omit a key to inherit, use [] to clear a list, or use {} for an empty config", path, node.Line)
	}
	if node.Kind == yaml.AliasNode || node.Anchor != "" || node.Tag == "!!merge" {
		return fmt.Errorf("%s at line %d: YAML anchors, aliases and merge keys are not supported", path, node.Line)
	}
	if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, "<agent>") {
		return fmt.Errorf("%s at line %d: %w", path, node.Line, validateCommand(node.Value))
	}
	if node.Kind == yaml.ScalarNode && node.Tag != "!!str" && node.Tag != "!!int" && node.Tag != "!!bool" {
		return fmt.Errorf("%s at line %d: unsupported YAML scalar type %s; use strings, integers or booleans", path, node.Line, node.Tag)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag != "!!str" || key.Kind != yaml.ScalarNode {
				return fmt.Errorf("%s at line %d: mapping keys must be strings; YAML merge keys are not supported", path, key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("%s at line %d: duplicate key %q", path, key.Line, key.Value)
			}
			seen[key.Value] = true
			if path == "config[0].sandbox" && key.Value == "container" {
				return fmt.Errorf("sandbox.container at line %d is no longer supported: sandbox always uses Podman; remove container and keep only enabled and image", key.Line)
			}
			if err := validateConfigNode(value, path+"."+key.Value); err != nil {
				return err
			}
		}
		return nil
	}
	for i, child := range node.Content {
		if err := validateConfigNode(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func mergeConfig(config *Config, layer configFile, repo bool) error {
	var err error
	if layer.Panes != nil {
		config.Panes, err = decodePanes(*layer.Panes)
		if err != nil {
			return fmt.Errorf("panes: %w", err)
		}
	}
	for name, layout := range layer.Layouts {
		panes, err := decodePanes(layout.Panes)
		if err != nil {
			return fmt.Errorf("layouts.%s: %w", name, err)
		}
		config.Layouts[name] = Layout{Panes: panes}
	}
	type configList struct {
		name   string
		target *[]string
		value  *[]string
	}
	lists := []configList{
		{"post_create", &config.PostCreate, layer.PostCreate},
		{"pre_merge", &config.PreMerge, layer.PreMerge},
		{"pre_remove", &config.PreRemove, layer.PreRemove},
	}
	if layer.Files != nil {
		lists = append(lists,
			configList{"files.copy", &config.Files.Copy, layer.Files.Copy},
			configList{"files.symlink", &config.Files.Symlink, layer.Files.Symlink},
		)
	}
	for _, list := range lists {
		if list.value == nil {
			continue
		}
		merged := make([]string, 0, len(*list.value))
		for _, value := range *list.value {
			if value == "<global>" {
				if !repo {
					return fmt.Errorf("%s: <global> is allowed only in repository hook and file lists", list.name)
				}
				merged = append(merged, (*list.target)...)
			} else {
				merged = append(merged, value)
			}
		}
		*list.target = merged
	}
	if sandbox := layer.Sandbox; sandbox != nil {
		if sandbox.Enabled != nil {
			config.Sandbox.Enabled = *sandbox.Enabled
		}
		if sandbox.Image != nil {
			config.Sandbox.Image = *sandbox.Image
		}
	}
	return nil
}

func decodePanes(raw []configPane) ([]Pane, error) {
	panes := make([]Pane, 0, len(raw))
	for i, value := range raw {
		pane := Pane{Command: value.Command, Split: value.Split, Focus: value.Focus, Zoom: value.Zoom}
		if value.Size != nil {
			if *value.Size <= 0 {
				return nil, fmt.Errorf("pane %d: explicit size must be greater than zero", i+1)
			}
			pane.Size = *value.Size
		}
		if value.Percentage != nil {
			if *value.Percentage < 1 || *value.Percentage > 100 {
				return nil, fmt.Errorf("pane %d: explicit percentage must be between 1 and 100", i+1)
			}
			pane.Percentage = *value.Percentage
		}
		panes = append(panes, pane)
	}
	return shellPanes(panes), nil
}

func (config Config) Validate() error {
	if err := validatePanes(config.Panes); err != nil {
		return fmt.Errorf("panes: %w", err)
	}
	for name, layout := range config.Layouts {
		if strings.TrimSpace(name) == "" || strings.ContainsFunc(name, unicode.IsControl) {
			return fmt.Errorf("invalid layout name %q", name)
		}
		if err := validatePanes(layout.Panes); err != nil {
			return fmt.Errorf("layouts.%s: %w", name, err)
		}
	}
	for _, list := range []struct {
		name   string
		values []string
	}{
		{"post_create", config.PostCreate},
		{"pre_merge", config.PreMerge},
		{"pre_remove", config.PreRemove},
	} {
		for _, command := range list.values {
			if strings.TrimSpace(command) == "" {
				return fmt.Errorf("%s: commands must not be empty", list.name)
			}
			if err := validateCommand(command); err != nil {
				return fmt.Errorf("%s: %w", list.name, err)
			}
		}
	}
	if err := validateFilePatterns(config.Files); err != nil {
		return err
	}
	if !configImage.MatchString(config.Sandbox.Image) {
		return fmt.Errorf("sandbox.image %q: expected an image name with an optional tag or sha256 digest, not a URL or shell command", config.Sandbox.Image)
	}
	return nil
}

var configImage = regexp.MustCompile(`^(?:[a-zA-Z0-9]+(?:[.-][a-zA-Z0-9]+)*(?::[0-9]+)?/)?[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*(?::[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127})?(?:@sha256:[a-fA-F0-9]{64})?$`)

func validatePanes(panes []Pane) error {
	focus, zoom := 0, 0
	for i, pane := range panes {
		if err := validateCommand(pane.Command); err != nil {
			return fmt.Errorf("pane %d: %w", i+1, err)
		}
		if pane.Split != "" && pane.Split != "horizontal" && pane.Split != "vertical" {
			return fmt.Errorf("pane %d: split must be horizontal or vertical", i+1)
		}
		if i == 0 && pane.Split != "" {
			return fmt.Errorf("pane 1: the first pane cannot specify a split")
		}
		if pane.Size < 0 || pane.Percentage < 0 || pane.Percentage > 100 {
			return fmt.Errorf("pane %d: size must be positive and percentage must be between 1 and 100 when set", i+1)
		}
		if pane.Size != 0 && pane.Percentage != 0 {
			return fmt.Errorf("pane %d: size and percentage cannot both be set", i+1)
		}
		if pane.Focus {
			focus++
		}
		if pane.Zoom {
			zoom++
		}
	}
	if focus > 1 || zoom > 1 {
		return fmt.Errorf("at most one pane may set focus and at most one pane may set zoom")
	}
	return nil
}

func validateCommand(command string) error {
	if strings.Contains(command, "<agent>") {
		return fmt.Errorf("<agent> expansion is not supported: set an explicit command or omit command for a shell")
	}
	if strings.Contains(command, "<global>") {
		return fmt.Errorf("<global> must be a separate repository hook or file list entry")
	}
	if strings.ContainsRune(command, '\x00') {
		return fmt.Errorf("commands must not contain NUL")
	}
	return nil
}

func (config Config) SelectPanes(layout string) ([]Pane, error) {
	panes := config.Panes
	if layout != "" {
		selected, ok := config.Layouts[layout]
		if !ok {
			return nil, fmt.Errorf("unknown layout %q", layout)
		}
		panes = selected.Panes
	}
	if err := validatePanes(panes); err != nil {
		return nil, err
	}
	return shellPanes(panes), nil
}

func shellPanes(panes []Pane) []Pane {
	if len(panes) == 0 {
		return []Pane{{}}
	}
	return slices.Clone(panes)
}
