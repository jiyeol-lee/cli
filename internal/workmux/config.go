package workmux

import (
	"bytes"
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
	Audio             bool   `yaml:"audio" json:"audio"`
	Image             string `yaml:"image" json:"image"`
}

type configFile struct {
	Panes      *configPanes            `yaml:"panes"`
	Files      *configFiles            `yaml:"files"`
	Layouts    map[string]configLayout `yaml:"layouts"`
	PostCreate *configStrings          `yaml:"post_create"`
	PreMerge   *configStrings          `yaml:"pre_merge"`
	PreRemove  *configStrings          `yaml:"pre_remove"`
	Sandbox    *configSandbox          `yaml:"sandbox"`
	lines      map[string]int
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
	Audio             *bool   `yaml:"audio"`
	Image             *string `yaml:"image"`
}

func LoadConfigForRepo(configDir, root string) (Config, error) {
	name, err := repoConfigName(filepath.Base(filepath.Clean(root)))
	if err != nil {
		return Config{}, err
	}
	return loadConfigPathsForRepo(configDir, []string{"config.yaml", name}, root)
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
	return loadConfigPathsForRepo(configDir, names, "")
}

func loadConfigPathsForRepo(configDir string, names []string, root string) (Config, error) {
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
		if err == nil {
			err = config.Validate()
		}
		if err == nil && root != "" {
			err = validateFilePatternsForRepo(root, config.Files)
		}
		if err != nil {
			field, line := "", 0
			for name, number := range layer.lines {
				if strings.HasPrefix(err.Error(), name) && len(name) > len(field) {
					field, line = name, number
				}
			}
			var pane int
			if _, scanErr := fmt.Sscanf(strings.TrimPrefix(err.Error(), field+": "), "pane %d:", &pane); scanErr == nil {
				panePath := fmt.Sprintf("%s[%d]", field, pane-1)
				if number := layer.lines[panePath]; number != 0 {
					field, line = panePath, number
				}
			}
			if line != 0 {
				err = fmt.Errorf("%s at line %d: %w", field, line, err)
			}
			return Config{}, fmt.Errorf("load config %s: %w", path, err)
		}
	}
	if config.Panes == nil {
		config.Panes = config.defaultPanes()
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
	if len(node.Content[0].Content) > 0 {
		layer.lines = make(map[string]int)
		configNodeLines(node.Content[0], "", layer.lines)
	}
	return layer, nil
}

func configNodeLines(node *yaml.Node, path string, lines map[string]int) {
	if node.Kind == yaml.AliasNode {
		configNodeLines(node.Alias, path, lines)
		return
	}
	if node.Kind == yaml.SequenceNode {
		for i, child := range node.Content {
			name := fmt.Sprintf("%s[%d]", path, i)
			lines[name] = child.Line
			configNodeLines(child, name, lines)
		}
		return
	}
	if node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		name := key.Value
		if path != "" {
			name = path + "." + name
		}
		lines[name] = key.Line
		configNodeLines(value, name, lines)
	}
}

func validateConfigNode(node *yaml.Node, path string) error {
	return validateConfigSchema(node, path, "config", make(map[*yaml.Node]bool))
}

func validateConfigSchema(node *yaml.Node, path, schema string, active map[*yaml.Node]bool) error {
	if active[node] {
		return fmt.Errorf("%s at line %d: recursive YAML aliases are not allowed", path, node.Line)
	}
	active[node] = true
	defer delete(active, node)
	if node.Kind == yaml.AliasNode {
		return validateConfigSchema(node.Alias, path, schema, active)
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" || key.Value == "<<" {
				return fmt.Errorf("%s.<< at line %d: YAML merge keys are unsupported; specify fields explicitly", path, key.Line)
			}
			if key.Tag != "!!str" || key.Kind != yaml.ScalarNode {
				return fmt.Errorf("%s at line %d: mapping keys must be strings", path, key.Line)
			}
			if seen[key.Value] {
				return fmt.Errorf("%s at line %d: duplicate key %q", path, key.Line, key.Value)
			}
			seen[key.Value] = true
			fields := map[string][]string{
				"config":  {"panes", "files", "layouts", "post_create", "pre_merge", "pre_remove", "sandbox"},
				"files":   {"copy", "symlink"},
				"sandbox": {"opencode_config_dir", "enabled", "audio", "image"},
				"layout":  {"panes"},
				"pane":    {"name", "command", "split", "size", "percentage", "focus", "zoom", "target"},
			}
			if allowed, ok := fields[schema]; ok && !slices.Contains(allowed, key.Value) {
				if schema == "sandbox" && key.Value == "target" {
					return fmt.Errorf("%s.%s at line %d: unsupported sandbox.target: remove this setting; only OpenCode panes are sandboxed", path, key.Value, key.Line)
				}
				if schema == "sandbox" && key.Value == "selinux_type" {
					return fmt.Errorf("%s.%s at line %d: unsupported sandbox.selinux_type: remove this setting; sandbox bind mounts now use shared SELinux relabeling", path, key.Value, key.Line)
				}
				if schema == "sandbox" && slices.Contains([]string{"container", "engine", "backend", "runtime"}, key.Value) {
					return fmt.Errorf("%s.%s at line %d: sandbox always uses Podman; keep enabled, audio, image and opencode_config_dir", path, key.Value, key.Line)
				}
				if schema == "config" {
					return fmt.Errorf("unsupported config field %s at line %d", key.Value, key.Line)
				}
				return fmt.Errorf("unsupported config field %s.%s at line %d", path, key.Value, key.Line)
			}
			if path == "config" && (key.Value == "files" || key.Value == "sandbox") {
				resolved := value
				for resolved.Kind == yaml.AliasNode {
					resolved = resolved.Alias
				}
				if resolved.Tag == "!!null" {
					return fmt.Errorf("%s.%s at line %d: %s must be a YAML mapping, not null", path, key.Value, key.Line, key.Value)
				}
			}
			childSchema := ""
			switch schema {
			case "config":
				childSchema = key.Value
			case "layouts":
				childSchema = "layout"
			case "layout":
				childSchema = "panes"
			case "files":
				childSchema = "strings"
			}
			if err := validateConfigSchema(value, path+"."+key.Value, childSchema, active); err != nil {
				return err
			}
		}
		return validateConfigValue(node, path, schema)
	}
	for i, child := range node.Content {
		childSchema := ""
		if schema == "panes" {
			childSchema = "pane"
		}
		if err := validateConfigSchema(child, fmt.Sprintf("%s[%d]", path, i), childSchema, active); err != nil {
			return err
		}
	}
	return validateConfigValue(node, path, schema)
}

func validateConfigValue(node *yaml.Node, path, schema string) error {
	var value any
	switch schema {
	case "pane":
		value = new(configPane)
	case "sandbox":
		value = new(configSandbox)
	case "panes":
		value = new(*configPanes)
	case "strings", "post_create", "pre_merge", "pre_remove":
		value = new(*configStrings)
	case "layout":
		value = new(configLayout)
	case "files":
		value = new(configFiles)
	default:
		return nil
	}
	if err := node.Decode(value); err != nil {
		return fmt.Errorf("%s at line %d: %w", path, node.Line, err)
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
	if pane.Target != nil && *pane.Target < 0 {
		return fmt.Errorf("pane target must be an unsigned integer")
	}
	// Pane does not retain whether an empty split was explicitly set.
	if pane.Split != nil && *pane.Split == "" {
		return fmt.Errorf("pane split must be horizontal or vertical")
	}
	return nil
}

func (sandbox *configSandbox) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("sandbox must be a YAML mapping")
	}
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "target" {
			return fmt.Errorf("unsupported sandbox.target: remove this setting; only OpenCode panes are sandboxed")
		}
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
		if key == "enabled" || key == "audio" {
			value := node.Content[i+1]
			for value.Kind == yaml.AliasNode {
				value = value.Alias
			}
			if value.Tag != "!!null" && value.Tag != "!!bool" {
				return fmt.Errorf("cannot unmarshal sandbox.%s as a boolean", key)
			}
		}
		if slices.Contains([]string{"container", "engine", "backend", "runtime"}, key) {
			return fmt.Errorf("unsupported sandbox.%s: sandbox always uses Podman; keep enabled, audio, image and opencode_config_dir", key)
		}
	}
	type plain configSandbox
	if err := node.Decode((*plain)(sandbox)); err != nil {
		return err
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
		if sandbox.Audio != nil {
			config.Sandbox.Audio = *sandbox.Audio
		}
		if sandbox.Image != nil {
			config.Sandbox.Image = *sandbox.Image
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
	if err := validatePanes(config.Panes); err != nil {
		return fmt.Errorf("panes: %w", err)
	}
	names := make([]string, 0, len(config.Layouts))
	for name := range config.Layouts {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := validatePanes(config.Layouts[name].Panes); err != nil {
			return fmt.Errorf("layouts.%s.panes: %w", name, err)
		}
	}
	if err := validateFilePatterns(config.Files); err != nil {
		return err
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
			if err := validateCommand(command); err != nil {
				return fmt.Errorf("%s: %w", list.name, err)
			}
		}
	}
	if err := validateSandboxImage(config.Sandbox.Image); err != nil {
		return fmt.Errorf("sandbox.image: %w", err)
	}
	return nil
}

func validateSandboxImage(image string) error {
	if image == "" || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00") {
		return fmt.Errorf("sandbox image must be an explicit image name")
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
	if panes == nil {
		panes = config.defaultPanes()
	} else {
		panes = slices.Clone(panes)
	}
	for i := range panes {
		if panes[i].Target != nil {
			target := *panes[i].Target
			panes[i].Target = &target
		}
		panes[i].Focus = panes[i].Focus || panes[i].Zoom
	}
	return panes, nil
}

func (config Config) defaultPanes() []Pane {
	if config.Sandbox.Enabled {
		return []Pane{{Command: "opencode", Focus: true}, {Split: "horizontal"}}
	}
	return defaultPanes()
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
