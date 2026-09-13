package workmux

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Name          string `yaml:"name" json:"name"`
	Command       string `yaml:"command" json:"command"`
	Split         string `yaml:"split" json:"split"`
	Size          int    `yaml:"size" json:"size"`
	Percentage    int    `yaml:"percentage" json:"percentage"`
	Focus         bool   `yaml:"focus" json:"focus"`
	Zoom          bool   `yaml:"zoom" json:"zoom"`
	Target        *int   `yaml:"target" json:"target"`
	sizeSet       bool
	nameSet       bool
	percentageSet bool
}

type FilesConfig struct {
	Copy    []string `yaml:"copy" json:"copy"`
	Symlink []string `yaml:"symlink" json:"symlink"`
}

type Layout struct {
	Panes []Pane `yaml:"panes" json:"panes"`
}

type SandboxConfig struct {
	OpenCodeConfigDir string `yaml:"opencode_config_dir,omitempty" json:"opencode_config_dir,omitempty"`
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	Image             string `yaml:"image" json:"image"`
	Target            string `yaml:"target,omitempty" json:"target,omitempty"`
}

type configFile struct {
	Panes      *configPanes            `yaml:"panes"`
	Files      *configFiles            `yaml:"files"`
	Layouts    map[string]configLayout `yaml:"layouts"`
	PostCreate *configStrings          `yaml:"post_create"`
	PreMerge   *configStrings          `yaml:"pre_merge"`
	PreRemove  *configStrings          `yaml:"pre_remove"`
	Sandbox    *configSandbox          `yaml:"sandbox"`
}

type configPane struct {
	Name       *string `yaml:"name"`
	Command    string  `yaml:"command"`
	Split      *string `yaml:"split"`
	Size       *int    `yaml:"size"`
	Percentage *int    `yaml:"percentage"`
	Focus      bool    `yaml:"focus"`
	Zoom       bool    `yaml:"zoom"`
	Target     *int    `yaml:"target"`
}

type configFiles struct {
	Copy    *configStrings `yaml:"copy"`
	Symlink *configStrings `yaml:"symlink"`
}

type configLayout struct {
	Panes *configPanes `yaml:"panes"`
}

type configPanes []configPane

type configStrings []string

type configSandbox struct {
	OpenCodeConfigDir *string `yaml:"opencode_config_dir"`
	Enabled           *bool   `yaml:"enabled"`
	Image             *string `yaml:"image"`
	Target            *string `yaml:"target"`
}

//go:embed testdata/config/cleanup_node_modules.sh
var nodeModulesCleanupScript string

func LoadConfigForRepo(configDir, root string) (Config, error) {
	config, err := LoadConfig(configDir, filepath.Base(filepath.Clean(root)))
	if err != nil {
		return Config{}, err
	}
	if config.PreRemove == nil {
		for _, lock := range []string{"pnpm-lock.yaml", "package-lock.json", "yarn.lock"} {
			if _, err := os.Stat(filepath.Join(root, lock)); err == nil {
				config.PreRemove = []string{nodeModulesCleanupScript}
				break
			}
		}
	}
	return config, nil
}

func LoadConfig(configDir, repoName string) (Config, error) {
	name, err := repoConfigName(repoName)
	if err != nil {
		return Config{}, err
	}
	return loadConfigPaths(configDir, []string{"config.yaml", name})
}

func loadGlobalConfig(configDir string) (Config, error) {
	return loadConfigPaths(configDir, []string{"config.yaml"})
}

func loadConfigPaths(configDir string, names []string) (Config, error) {
	config := Config{
		Layouts: make(map[string]Layout),
		Sandbox: SandboxConfig{Image: "localhost/cli-workmux:fedora44"},
	}
	for i, name := range names {
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
		if err != nil {
			return Config{}, fmt.Errorf("load config %s: %w", path, err)
		}
	}
	if config.Panes == nil {
		config.Panes = defaultPanes()
	}
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("load merged config from %s: %w", configDir, err)
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
			return configFile{}, nil
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
	if len(node.Content) == 1 {
		value := node.Content[0]
		if value.Kind == yaml.ScalarNode && value.Tag == "!!null" && value.Value == "" && value.Style == 0 && value.Anchor == "" {
			return configFile{}, nil
		}
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
		return configFile{}, fmt.Errorf("config must be a YAML mapping; use {} for defaults")
	}
	if err := validateConfigNode(node.Content[0], "config"); err != nil {
		return configFile{}, err
	}
	var layer configFile
	if err := node.Decode(&layer); err != nil {
		return configFile{}, err
	}
	return layer, nil
}

func validateConfigNode(node *yaml.Node, path string) error {
	// serde_yaml ignores unknown merge keys rather than applying YAML map merging.
	// Strip them before yaml.v3 decodes, but leave anchors and aliases intact.
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		content := make([]*yaml.Node, 0, len(node.Content))
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" || key.Value == "<<" {
				if err := validateConfigNode(value, path+".<<"); err != nil {
					return err
				}
				continue
			}
			if key.Tag != "!!str" || key.Kind != yaml.ScalarNode {
				return fmt.Errorf("%s at line %d: mapping keys must be strings", path, key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("%s at line %d: duplicate key %q", path, key.Line, key.Value)
			}
			seen[key.Value] = true
			if path == "config" && !slices.Contains([]string{"panes", "files", "layouts", "post_create", "pre_merge", "pre_remove", "sandbox"}, key.Value) {
				return fmt.Errorf("unsupported config field %s at line %d", key.Value, key.Line)
			}
			if path == "config" && (key.Value == "files" || key.Value == "sandbox") {
				resolved := value
				for resolved.Kind == yaml.AliasNode {
					resolved = resolved.Alias
				}
				if resolved.Tag == "!!null" {
					return fmt.Errorf("%s must be a YAML mapping, not null", key.Value)
				}
			}
			if err := validateConfigNode(value, path+"."+key.Value); err != nil {
				return err
			}
			content = append(content, key, value)
		}
		node.Content = content
		return nil
	}
	for i, child := range node.Content {
		if err := validateConfigNode(child, fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	return nil
}

func validateConfigList(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("cannot unmarshal %s as a list", node.Tag)
	}
	for _, value := range node.Content {
		for value.Kind == yaml.AliasNode {
			value = value.Alias
		}
		if value.Tag == "!!null" {
			return fmt.Errorf("null list entries are not allowed")
		}
	}
	return nil
}

func (panes *configPanes) UnmarshalYAML(node *yaml.Node) error {
	if err := validateConfigList(node); err != nil {
		return err
	}
	return node.Decode((*[]configPane)(panes))
}

func (values *configStrings) UnmarshalYAML(node *yaml.Node) error {
	if err := validateConfigList(node); err != nil {
		return err
	}
	return node.Decode((*[]string)(values))
}

func (pane *configPane) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("pane must be a YAML mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i].Value, node.Content[i+1]
		for value.Kind == yaml.AliasNode {
			value = value.Alias
		}
		if slices.Contains([]string{"size", "percentage", "target"}, key) && value.Tag != "!!null" && value.Tag != "!!int" {
			return fmt.Errorf("pane %s must be an integer", key)
		}
		if (key == "focus" || key == "zoom") && value.Tag != "!!bool" {
			return fmt.Errorf("pane %s must be a boolean", key)
		}
	}
	type plain configPane
	if err := node.Decode((*plain)(pane)); err != nil {
		return err
	}
	if pane.Size != nil && (*pane.Size < 0 || *pane.Size > 65535) {
		return fmt.Errorf("pane size must be between 0 and 65535")
	}
	if pane.Percentage != nil && (*pane.Percentage < 0 || *pane.Percentage > 255) {
		return fmt.Errorf("pane percentage must be between 0 and 255")
	}
	if pane.Target != nil && *pane.Target < 0 {
		return fmt.Errorf("pane target must be an unsigned integer")
	}
	if pane.Split != nil && !slices.Contains([]string{"horizontal", "vertical", "stacked"}, *pane.Split) {
		return fmt.Errorf("pane split must be horizontal or vertical, or stacked for Zellij")
	}
	return nil
}

func (sandbox *configSandbox) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("sandbox must be a YAML mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "selinux_type" {
			return fmt.Errorf("unsupported sandbox.selinux_type: remove this setting; sandbox bind mounts now use shared SELinux relabeling")
		}
		if key == "opencode_config_dir" {
			value := node.Content[i+1]
			for value.Kind == yaml.AliasNode {
				value = value.Alias
			}
			if value.Tag != "!!null" && value.Tag != "!!str" {
				return fmt.Errorf("sandbox.%s must be a string", key)
			}
		}
		if key == "enabled" {
			value := node.Content[i+1]
			for value.Kind == yaml.AliasNode {
				value = value.Alias
			}
			if value.Tag != "!!null" && value.Tag != "!!bool" {
				return fmt.Errorf("cannot unmarshal sandbox.enabled as a boolean")
			}
		}
		if slices.Contains([]string{"container", "engine", "backend", "runtime"}, key) {
			return fmt.Errorf("unsupported sandbox.%s: sandbox always uses Podman; keep enabled, image and target", key)
		}
	}
	type plain configSandbox
	if err := node.Decode((*plain)(sandbox)); err != nil {
		return err
	}
	if sandbox.Target != nil && *sandbox.Target != "agent" && *sandbox.Target != "all" {
		return fmt.Errorf("sandbox.target must be agent or all")
	}
	return nil
}

func mergeConfig(config *Config, layer configFile, repo bool) error {
	if layer.Panes != nil {
		config.Panes = decodePanes(*layer.Panes)
	}
	for name, layout := range layer.Layouts {
		if layout.Panes == nil {
			return fmt.Errorf("layouts.%s: missing required panes list", name)
		}
		config.Layouts[name] = Layout{Panes: decodePanes(*layout.Panes)}
	}
	type configList struct {
		target *[]string
		value  *configStrings
	}
	lists := []configList{
		{&config.PostCreate, layer.PostCreate},
		{&config.PreMerge, layer.PreMerge},
		{&config.PreRemove, layer.PreRemove},
	}
	if layer.Files != nil {
		lists = append(lists,
			configList{&config.Files.Copy, layer.Files.Copy},
			configList{&config.Files.Symlink, layer.Files.Symlink},
		)
	}
	for _, list := range lists {
		if list.value == nil {
			continue
		}
		merged := make([]string, 0, len(*list.value))
		for _, value := range *list.value {
			if value == "<global>" && repo {
				merged = append(merged, (*list.target)...)
			} else {
				merged = append(merged, value)
			}
		}
		*list.target = merged
	}
	if sandbox := layer.Sandbox; sandbox != nil {
		if sandbox.OpenCodeConfigDir != nil {
			config.Sandbox.OpenCodeConfigDir = *sandbox.OpenCodeConfigDir
		}
		if sandbox.Enabled != nil {
			config.Sandbox.Enabled = *sandbox.Enabled
		}
		if sandbox.Image != nil {
			config.Sandbox.Image = *sandbox.Image
		}
		if sandbox.Target != nil {
			config.Sandbox.Target = *sandbox.Target
		}
	}
	return nil
}

func decodePanes(raw []configPane) []Pane {
	panes := make([]Pane, 0, len(raw))
	for _, value := range raw {
		panes = append(panes, paneFromConfig(value))
	}
	return panes
}

func (config Config) Validate() error {
	// Pane geometry and file patterns are checked by SelectPanes and ApplyFiles.
	for _, list := range []struct {
		name   string
		values []string
	}{
		{"post_create", config.PostCreate},
		{"pre_merge", config.PreMerge},
		{"pre_remove", config.PreRemove},
	} {
		for _, command := range list.values {
			if err := validateCommand(command); err != nil {
				return fmt.Errorf("%s: %w", list.name, err)
			}
		}
	}
	if config.Sandbox.Target != "" && config.Sandbox.Target != "agent" && config.Sandbox.Target != "all" {
		return fmt.Errorf("sandbox.target must be agent or all")
	}
	return nil
}

func validatePanes(panes []Pane) error {
	zoom := 0
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
		if i == 0 && (pane.SizeSpecified() || pane.Percentage != 0 || pane.percentageSet) {
			return fmt.Errorf("pane 1: the first pane cannot specify size or percentage")
		}
		if i > 0 && pane.Split == "" {
			return fmt.Errorf("pane %d: split direction must be specified", i+1)
		}
		if pane.Target != nil && (*pane.Target < 0 || *pane.Target >= i) {
			return fmt.Errorf("pane %d: target must reference a previously created pane", i+1)
		}
		if (pane.Name != "" || pane.nameSet) && strings.TrimSpace(pane.Name) == "" {
			return fmt.Errorf("pane %d: name must not be empty", i+1)
		}
		if pane.Size < 0 || pane.Size > 65535 || pane.Percentage < 0 || pane.Percentage > 100 || pane.percentageSet && pane.Percentage == 0 {
			return fmt.Errorf("pane %d: size must be between 0 and 65535 and percentage between 1 and 100 when set", i+1)
		}
		if pane.SizeSpecified() && (pane.Percentage != 0 || pane.percentageSet) {
			return fmt.Errorf("pane %d: size and percentage cannot both be set", i+1)
		}
		if pane.Zoom {
			zoom++
		}
	}
	if zoom > 1 {
		return fmt.Errorf("at most one pane may set zoom")
	}
	return nil
}

func validateCommand(command string) error {
	words := strings.Fields(command)
	if len(words) > 0 && (words[0] == "<agent>" || words[0] == "exec" && len(words) > 1 && words[1] == "<agent>") {
		return fmt.Errorf("<agent> expansion is not supported: set an explicit command or omit command for a shell")
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
	panes = shellPanes(panes)
	for i := range panes {
		if panes[i].Target != nil {
			target := *panes[i].Target
			panes[i].Target = &target
		}
		panes[i].Focus = panes[i].Focus || panes[i].Zoom
	}
	return panes, nil
}

func shellPanes(panes []Pane) []Pane {
	if panes == nil {
		return defaultPanes()
	}
	return slices.Clone(panes)
}

func defaultPanes() []Pane {
	return []Pane{{Focus: true}, {Command: "clear", Split: "horizontal"}}
}

func (pane Pane) SizeSpecified() bool {
	return pane.Size != 0 || pane.sizeSet
}

func (pane Pane) MarshalYAML() (any, error) {
	value := configPane{Command: pane.Command, Focus: pane.Focus, Zoom: pane.Zoom, Target: pane.Target}
	if pane.Name != "" || pane.nameSet {
		value.Name = &pane.Name
	}
	if pane.Split != "" {
		value.Split = &pane.Split
	}
	if pane.SizeSpecified() {
		value.Size = &pane.Size
	}
	if pane.Percentage != 0 || pane.percentageSet {
		value.Percentage = &pane.Percentage
	}
	return value, nil
}

func (pane *Pane) UnmarshalYAML(node *yaml.Node) error {
	var value configPane
	if err := node.Decode(&value); err != nil {
		return err
	}
	*pane = paneFromConfig(value)
	return nil
}

func paneFromConfig(value configPane) Pane {
	pane := Pane{Command: value.Command, Focus: value.Focus || value.Zoom, Zoom: value.Zoom, Target: value.Target}
	if value.Name != nil {
		pane.Name = *value.Name
		pane.nameSet = pane.Name == ""
	}
	if value.Split != nil {
		pane.Split = *value.Split
	}
	if value.Size != nil {
		pane.Size = *value.Size
		pane.sizeSet = pane.Size == 0
	}
	if value.Percentage != nil {
		pane.Percentage = *value.Percentage
		pane.percentageSet = pane.Percentage == 0
	}
	return pane
}

func (pane Pane) MarshalJSON() ([]byte, error) {
	type plain Pane
	var size *int
	var name *string
	var percentage *int
	if pane.Name != "" || pane.nameSet {
		name = &pane.Name
	}
	if pane.Percentage != 0 || pane.percentageSet {
		percentage = &pane.Percentage
	}
	if pane.SizeSpecified() {
		size = &pane.Size
	}
	return json.Marshal(struct {
		plain
		Size       *int    `json:"size,omitempty"`
		Name       *string `json:"name,omitempty"`
		Percentage *int    `json:"percentage,omitempty"`
	}{plain: plain(pane), Size: size, Name: name, Percentage: percentage})
}

func (pane *Pane) UnmarshalJSON(data []byte) error {
	type plain Pane
	var value struct {
		plain
		Size       *int    `json:"size"`
		Name       *string `json:"name"`
		Percentage *int    `json:"percentage"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*pane = Pane(value.plain)
	if value.Size != nil {
		pane.Size = *value.Size
		pane.sizeSet = pane.Size == 0
	}
	if value.Name != nil {
		pane.Name = *value.Name
		pane.nameSet = pane.Name == ""
	}
	if value.Percentage != nil {
		pane.Percentage = *value.Percentage
		pane.percentageSet = pane.Percentage == 0
	}
	return nil
}
