package workmux

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type Containers struct {
	Runner   Runner
	HomeDir  string
	StateDir string
	Getenv   func(string) string
}

const sandboxLabel = "io.github.jiyeol-lee.cli.workmux."
const sandboxPolicy = "3"

var sandboxContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var sandboxDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

var _ Sandbox = (*Containers)(nil)

type sandboxEngine struct {
	Endpoint string
	Prefix   []string
	Host     string
}

var sandboxEngineEnvKeys = []string{
	"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_RUNTIME_DIR",
	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "PODMAN_CONNECTIONS_CONF",
	"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_STORAGE_CONF", "STORAGE_DRIVER", "STORAGE_OPTS",
	"CONTAINERS_REGISTRIES_CONF", "CONTAINERS_REGISTRIES_CONF_DIR", "REGISTRY_AUTH_FILE", "CONTAINERS_POLICY",
	"CONTAINERS_MACHINE_PROVIDER", "CONTAINERS_HELPER_BINARY_DIR", "TMPDIR", "SSH_AUTH_SOCK",
}

func sandboxCurrentEngine() sandboxEngine {
	engine := sandboxEngine{Prefix: []string{"/usr/bin/env"}}
	var values []string
	// OpenCode's injected HomeDir/Getenv must not select the native Podman store.
	for _, key := range sandboxEngineEnvKeys {
		if value, present := os.LookupEnv(key); present {
			values = append(values, key+"="+value)
		} else {
			engine.Prefix = append(engine.Prefix, "-u", key)
		}
	}
	engine.Prefix = append(engine.Prefix, values...)
	engine.Prefix = append(engine.Prefix, "podman")
	return engine
}

func (engine sandboxEngine) process(args ...string) Process {
	if len(engine.Prefix) != 0 {
		return Process{Name: engine.Prefix[0], Args: append(slices.Clone(engine.Prefix[1:]), args...), Dir: "/", CleanGitEnv: true}
	}
	return Process{Name: "podman", Args: args, Dir: "/", CleanGitEnv: true}
}

type sandboxInspection struct {
	engine sandboxEngine
	ID     string `json:"Id"`
	Name   string
	Image  string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct{ Running bool }
}

func sandboxName(w Workspace) (string, error) {
	name := w.Container
	if name == "" {
		name = "cli-workmux-" + w.ID
	}
	if w.ID == "" || w.RepoID == "" || !sandboxContainerName.MatchString(name) {
		return "", fmt.Errorf("invalid sandbox identity or container name %q", name)
	}
	for _, path := range []string{w.Root, w.CommonDir, w.Path} {
		if err := sandboxMountPath(path); err != nil {
			return "", err
		}
	}
	return name, nil
}

func (c *Containers) getenv(key string) string {
	if c.Getenv != nil {
		return c.Getenv(key)
	}
	return os.Getenv(key)
}

func (c *Containers) run(ctx context.Context, engine sandboxEngine, args ...string) ([]byte, error) {
	if c.Runner == nil {
		return nil, fmt.Errorf("sandbox runner is not configured")
	}
	return c.Runner.Run(ctx, engine.process(args...))
}

func (c *Containers) Check(ctx context.Context, config SandboxConfig) error {
	if !config.Enabled {
		return nil
	}
	_, err := c.checkImage(ctx, config)
	return err
}

func (c *Containers) checkImage(ctx context.Context, config SandboxConfig) (string, error) {
	return c.checkEngineImage(ctx, sandboxCurrentEngine(), config)
}

func (c *Containers) checkEngineImage(ctx context.Context, engine sandboxEngine, config SandboxConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if config.Image == "" || strings.HasPrefix(config.Image, "-") || strings.ContainsAny(config.Image, " \t\r\n\x00") {
		return "", fmt.Errorf("sandbox image must be an explicit image name")
	}
	output, err := c.run(ctx, engine, "image", "inspect", config.Image)
	if err != nil {
		return "", fmt.Errorf("inspect sandbox image %q in the configured Podman image store; run cli workmux sandbox build, then open the existing ready workspace: %w", config.Image, err)
	}
	var images []struct {
		ID string `json:"Id"`
	}
	if err := json.Unmarshal(output, &images); err != nil {
		return "", fmt.Errorf("decode sandbox image: %w", err)
	}
	if len(images) != 1 || !sandboxDigest.MatchString(strings.TrimPrefix(images[0].ID, "sha256:")) {
		return "", fmt.Errorf("inspect sandbox image returned no unique image ID")
	}
	return images[0].ID, nil
}

func (c *Containers) Ensure(ctx context.Context, w Workspace) error {
	if !w.Config.Sandbox.Enabled {
		return fmt.Errorf("sandbox is disabled")
	}
	if _, err := sandboxName(w); err != nil {
		return err
	}
	if w.Container != "" {
		return fmt.Errorf("legacy persistent sandbox %s is preserved; recover any container-local files and explicitly remove this workspace before creating a new ephemeral session", w.Container)
	}
	return c.Check(ctx, w.Config.Sandbox)
}

func (c *Containers) ownerLabels(w Workspace) (map[string]string, error) {
	if _, err := sandboxName(w); err != nil {
		return nil, err
	}
	if err := sandboxMountPath(c.StateDir); err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("identify sandbox host: %w", err)
	}
	source := fmt.Sprintf("%x", sha256.Sum256([]byte(host+"\x00"+strconv.Itoa(os.Getuid())+"\x00"+c.StateDir)))
	labels := map[string]string{
		"owner": "cli-workmux", "policy": sandboxPolicy, "workspace": w.ID, "repository": w.RepoID,
		"root": w.Root, "common": w.CommonDir, "path": w.Path, "runtime": "podman", "source": source,
	}
	if w.Standalone {
		labels["standalone"] = "true"
	}
	return labels, nil
}

func (c *Containers) runArgs(ctx context.Context, engine sandboxEngine, w Workspace, command string, env []string, interactive, input bool) ([]string, error) {
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !sandboxEnvKey(key) || strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("invalid sandbox environment assignment %q", key)
		}
	}
	if strings.ContainsRune(command, 0) {
		return nil, fmt.Errorf("sandbox command contains NUL")
	}
	if !w.Config.Sandbox.Enabled {
		return nil, fmt.Errorf("sandbox is disabled")
	}
	if w.Container != "" {
		return nil, c.Ensure(ctx, w)
	}
	labels, err := c.ownerLabels(w)
	if err != nil {
		return nil, err
	}
	image, err := c.checkEngineImage(ctx, engine, w.Config.Sandbox)
	if err != nil {
		return nil, err
	}
	plan, err := c.mountPlan(ctx, w, image, true)
	if err != nil {
		return nil, fmt.Errorf("protect sandbox mounts: %w", err)
	}
	if err := plan.snapshots(true); err != nil {
		return nil, err
	}
	if engine.Endpoint == "" {
		engine, err = c.sessionEngineFor(ctx, engine)
		if err != nil {
			return nil, err
		}
	}
	labels["endpoint"] = engine.Endpoint
	session := rand.Text()
	if w.SandboxRun != "" {
		session = w.SandboxRun
	}
	name := "cli-workmux-" + sandboxWorkspaceKey(w)[:16] + "-" + session
	args := []string{"run", "--rm"}
	if interactive {
		args = append(args, "-it")
	} else if input {
		args = append(args, "-i")
	}
	args = append(args, "--name", name, "--pull=never", "--userns=keep-id", "--user", strconv.Itoa(os.Getuid())+":"+strconv.Itoa(os.Getgid()))
	labels["session"], labels["image"], labels["image-id"] = session, w.Config.Sandbox.Image, image
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		args = append(args, "--label", sandboxLabel+key+"="+labels[key])
	}
	for _, mount := range plan.Mounts {
		for _, path := range []string{mount.Source, mount.Target} {
			if err := sandboxMountPath(path); err != nil {
				return nil, err
			}
		}
		value := "type=bind,source=" + mount.Source + ",target=" + mount.Target + ",relabel=shared"
		if mount.ReadOnly {
			value += ",readonly"
		}
		args = append(args, "--mount", value)
	}
	args = append(args, "--workdir", w.Path)
	for _, entry := range append(plan.Env, env...) {
		args = append(args, "--env", entry)
	}
	for _, key := range []string{"TERM", "COLORTERM"} {
		if value := c.getenv(key); value != "" {
			args = append(args, "--env", key+"="+value)
		}
	}
	return append(args, image, "bash", "-c", command), nil
}

func (c *Containers) PaneCommand(ctx context.Context, w Workspace, command string) ([]string, error) {
	engine := sandboxCurrentEngine()
	args, err := c.runArgs(ctx, engine, w, command, nil, true, true)
	if err != nil {
		return nil, err
	}
	return append(engine.Prefix, args...), nil
}

func (c *Containers) Exec(ctx context.Context, w Workspace, command string, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	engine := sandboxCurrentEngine()
	args, err := c.runArgs(ctx, engine, w, command, env, false, stdin != nil)
	if err != nil {
		return err
	}
	p := engine.process(args...)
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	_, err = c.Runner.Run(ctx, p)
	if err != nil {
		return fmt.Errorf("run sandbox: %w", err)
	}
	return nil
}

func sandboxEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for i, ch := range key {
		if ch != '_' && (ch < 'A' || ch > 'Z') && (ch < 'a' || ch > 'z') && (i == 0 || ch < '0' || ch > '9') {
			return false
		}
	}
	return true
}

func (c *Containers) inspect(ctx context.Context, engine sandboxEngine, name string) (*sandboxInspection, error) {
	if !sandboxContainerName.MatchString(name) {
		return nil, fmt.Errorf("invalid sandbox container identity %q", name)
	}
	var output []byte
	for attempt := range 2 {
		var err error
		output, err = c.run(ctx, engine, "container", "inspect", name)
		if err == nil {
			break
		}
		listed, listErr := c.run(ctx, engine, "container", "ls", "--all", "--filter", "name=^/?"+regexp.QuoteMeta(name)+"$", "--format", "{{.Names}}")
		if listErr != nil {
			return nil, fmt.Errorf("inspect sandbox %s: %w; confirm absence: %v", name, err, listErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return nil, nil
		}
		if attempt == 1 {
			return nil, fmt.Errorf("inspect sandbox %s: %w", name, err)
		}
		// The container can appear after inspect failed but before the absence
		// query. Inspect it once more; listing alone never proves ownership.
	}
	var containers []sandboxInspection
	if err := json.Unmarshal(output, &containers); err != nil {
		return nil, fmt.Errorf("decode sandbox inspection: %w", err)
	}
	if len(containers) != 1 || !sandboxContainerName.MatchString(containers[0].ID) || strings.TrimPrefix(containers[0].Name, "/") != name {
		return nil, fmt.Errorf("sandbox inspection did not identify %s", name)
	}
	containers[0].engine = engine
	return &containers[0], nil
}

func sandboxMatchLabels(found *sandboxInspection, labels map[string]string) error {
	for key, value := range labels {
		if found.Config.Labels[sandboxLabel+key] != value {
			return fmt.Errorf("refusing sandbox %s: ownership label %s does not match", found.Name, key)
		}
	}
	if !sandboxDigest.MatchString(strings.TrimPrefix(found.Image, "sha256:")) || found.Config.Labels[sandboxLabel+"image-id"] != found.Image || found.Config.Labels[sandboxLabel+"image"] == "" {
		return fmt.Errorf("refusing sandbox %s: missing image identity labels", found.Name)
	}
	return nil
}

func (c *Containers) owned(ctx context.Context, w Workspace) ([]*sandboxInspection, error) {
	return c.ownedSessions(ctx, w, false)
}

func (c *Containers) ownedSessions(ctx context.Context, w Workspace, strict bool) ([]*sandboxInspection, error) {
	engine := sandboxCurrentEngine()
	labels, err := c.ownerLabels(w)
	if err != nil {
		return nil, err
	}
	var owned []*sandboxInspection
	if w.Container != "" {
		_, _, legacy, err := c.lookup(ctx, w)
		if err != nil {
			return nil, err
		}
		if legacy != nil {
			owned = append(owned, legacy)
		}
	}
	args := []string{"container", "ls", "--filter", "label=" + sandboxLabel + "owner=cli-workmux", "--filter", "label=" + sandboxLabel + "source=" + labels["source"], "--filter", "label=" + sandboxLabel + "workspace=" + w.ID, "--filter", "label=" + sandboxLabel + "repository=" + w.RepoID, "--filter", "label=" + sandboxLabel + "path=" + w.Path, "--format", "{{.Names}}"}
	if strict {
		args = append(args, "--all")
	}
	out, err := c.run(ctx, engine, args...)
	if err != nil {
		return nil, fmt.Errorf("list sandbox sessions: %w", err)
	}
	for name := range strings.FieldsSeq(string(out)) {
		found, err := c.inspect(ctx, engine, name)
		if err != nil {
			return nil, err
		}
		if found == nil {
			if strict {
				return nil, fmt.Errorf("registered sandbox %s disappeared; retry after checking its original Podman endpoint", name)
			}
			continue
		}
		if !strict && found.Config.Labels[sandboxLabel+"standalone"] == "true" {
			continue
		}
		if err := sandboxMatchLabels(found, labels); err != nil {
			return nil, err
		}
		if endpoint := found.Config.Labels[sandboxLabel+"endpoint"]; endpoint != "" {
			current, err := c.sessionEngineFor(ctx, engine)
			if err != nil {
				return nil, err
			}
			if endpoint != current.Endpoint {
				return nil, fmt.Errorf("refusing sandbox %s: recorded endpoint does not match", name)
			}
		}
		session := found.Config.Labels[sandboxLabel+"session"]
		if session == "" || name != "cli-workmux-"+sandboxWorkspaceKey(w)[:16]+"-"+session {
			return nil, fmt.Errorf("refusing sandbox %s: session identity does not match", name)
		}
		owned = append(owned, found)
	}
	slices.SortFunc(owned, func(a, b *sandboxInspection) int { return strings.Compare(a.Name, b.Name) })
	return owned, nil
}

func (c *Containers) Exists(ctx context.Context, w Workspace) (bool, error) {
	found, err := c.owned(ctx, w)
	return len(found) != 0, err
}

func (c *Containers) Stop(ctx context.Context, w Workspace) error {
	found, err := c.owned(ctx, w)
	if err != nil {
		return err
	}
	var failures []error
	for _, container := range found {
		if !container.State.Running {
			continue
		}
		if err := c.act(ctx, container, "stop", "-t", "0"); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (c *Containers) Remove(ctx context.Context, w Workspace) error {
	found, err := c.owned(ctx, w)
	if err != nil {
		return err
	}
	var failures []error
	for _, container := range found {
		if err := c.act(ctx, container, "rm", "--force"); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (c *Containers) act(ctx context.Context, found *sandboxInspection, args ...string) error {
	_, err := c.run(ctx, found.engine, append(args, found.ID)...)
	if err == nil {
		return nil
	}
	if found.Config.Labels[sandboxLabel+"policy"] == sandboxPolicy {
		// A --rm session can exit between inspection and the stop/remove request.
		current, inspectErr := c.inspect(ctx, found.engine, strings.TrimPrefix(found.Name, "/"))
		if inspectErr == nil && (current == nil || current.ID != found.ID) {
			return nil
		}
	}
	return fmt.Errorf("%s sandbox %s: %w", args[0], found.Name, err)
}

// Legacy endpoint records are only consulted for persistent containers created by policy 2.
func (c *Containers) localEngine(ctx context.Context) (sandboxEngine, error) {
	return c.engineInfo(ctx, sandboxCurrentEngine())
}

func (c *Containers) engineInfo(ctx context.Context, engine sandboxEngine) (sandboxEngine, error) {
	out, err := c.run(ctx, engine, "info", "--format", "{{json .}}")
	if err != nil {
		return engine, fmt.Errorf("identify Podman store: %w", err)
	}
	var info struct {
		Host  struct{ DatabaseBackend, Hostname string }
		Store struct {
			GraphRoot, RunRoot, GraphDriverName, ConfigFile string
			TransientStore                                  bool
		}
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return engine, fmt.Errorf("decode Podman store: %w", err)
	}
	if info.Store.GraphRoot == "" || info.Store.RunRoot == "" {
		return engine, fmt.Errorf("podman did not identify its storage endpoint")
	}
	engine.Host = info.Host.Hostname
	scope := []string{"podman", strconv.Itoa(os.Getuid()), info.Store.GraphRoot, info.Store.RunRoot, info.Store.GraphDriverName, info.Store.ConfigFile, info.Host.DatabaseBackend, strconv.FormatBool(info.Store.TransientStore)}
	engine.Endpoint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(scope, "\x00"))))
	return engine, nil
}

func (c *Containers) sessionEngine(ctx context.Context) (sandboxEngine, error) {
	return c.sessionEngineFor(ctx, sandboxCurrentEngine())
}

func (c *Containers) sessionEngineFor(ctx context.Context, engine sandboxEngine) (sandboxEngine, error) {
	engine, err := c.engineInfo(ctx, engine)
	if err != nil {
		return engine, err
	}
	// Store paths alone do not distinguish two remote hosts with identical layouts.
	var connection []string
	for _, key := range []string{"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "PODMAN_CONNECTIONS_CONF", "CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE"} {
		value := key + "="
		for _, entry := range engine.Prefix {
			if strings.HasPrefix(entry, key+"=") {
				value = entry
			}
		}
		connection = append(connection, value)
	}
	engine.Endpoint = fmt.Sprintf("%x", sha256.Sum256([]byte(engine.Endpoint+"\x00"+engine.Host+"\x00"+strings.Join(connection, "\x00"))))
	return engine, nil
}

func (c *Containers) lookup(ctx context.Context, w Workspace) (sandboxEngine, string, *sandboxInspection, error) {
	name, err := sandboxName(w)
	if err != nil {
		return sandboxEngine{}, "", nil, err
	}
	engine, err := c.localEngine(ctx)
	if err != nil {
		return engine, name, nil, err
	}
	if err := c.endpoint(w, engine, false); err != nil {
		return engine, name, nil, err
	}
	found, err := c.inspect(ctx, engine, name)
	if err == nil && found != nil {
		labels := map[string]string{"owner": "cli-workmux", "policy": "2", "workspace": w.ID, "repository": w.RepoID, "root": w.Root, "common": w.CommonDir, "path": w.Path, "runtime": "podman", "endpoint": engine.Endpoint}
		err = sandboxMatchLabels(found, labels)
		if err == nil && !sandboxDigest.MatchString(found.Config.Labels[sandboxLabel+"mounts"]) {
			err = fmt.Errorf("refusing legacy sandbox %s: missing protection identity", name)
		}
	}
	return engine, name, found, err
}

func (c *Containers) endpoint(w Workspace, engine sandboxEngine, create bool) error {
	if err := sandboxMountPath(c.StateDir); err != nil {
		return err
	}
	path := filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(w), "endpoint")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) && !create {
		return nil
	}
	if create {
		plan := sandboxMountPlan{Mounts: []sandboxMount{{Source: path, Snapshot: true, Content: []byte(engine.Endpoint + "\n")}}}
		if err := plan.snapshots(true); err != nil {
			return fmt.Errorf("record sandbox engine identity: %w", err)
		}
	}
	if _, err := sandboxRegular(path); err != nil {
		return fmt.Errorf("read sandbox engine identity: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read sandbox engine identity: %w", err)
	}
	if string(data) != engine.Endpoint+"\n" {
		return fmt.Errorf("legacy sandbox engine identity changed; select the original Podman store before removing this workspace so container-local data is not orphaned")
	}
	return nil
}
