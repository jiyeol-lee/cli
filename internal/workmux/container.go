package workmux

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Containers struct {
	Runner   Runner
	HomeDir  string
	StateDir string
	Getenv   func(string) string
}

const sandboxLabel = "io.github.jiyeol-lee.cli.workmux."
const sandboxPolicy = "2"

var sandboxContainerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
var sandboxDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

var _ Sandbox = (*Containers)(nil)

var sandboxEngineEnvKeys = []string{
	"PATH", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_RUNTIME_DIR",
	"CONTAINER_HOST", "CONTAINER_CONNECTION", "CONTAINER_SSHKEY", "PODMAN_CONNECTIONS_CONF",
	"CONTAINERS_CONF", "CONTAINERS_CONF_OVERRIDE", "CONTAINERS_STORAGE_CONF", "STORAGE_DRIVER", "STORAGE_OPTS",
}

type sandboxEngine struct {
	Endpoint string
	Env      []string
}

func (engine sandboxEngine) process(args ...string) Process {
	return Process{Name: "podman", Args: append([]string{"--remote=false"}, args...), Dir: "/", Env: engine.Env, CleanGitEnv: true}
}

func (engine sandboxEngine) panePrefix() []string {
	args := []string{"/usr/bin/env", "--chdir=/"}
	for _, key := range sandboxEngineEnvKeys {
		args = append(args, "--unset="+key)
	}
	for _, entry := range engine.Env {
		if !strings.HasSuffix(entry, "=") {
			args = append(args, entry)
		}
	}
	p := engine.process()
	return append(append(args, p.Name), p.Args...)
}

type sandboxInspection struct {
	ID     string `json:"Id"`
	Name   string
	Image  string
	Config struct {
		Image  string
		Labels map[string]string
	}
	State struct {
		Running bool
	}
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
	if err := c.checkOpenCodeConfig(ctx); err != nil {
		return "", err
	}
	engine, err := c.localEngine(ctx)
	if err != nil {
		return "", err
	}
	if config.Image == "" || strings.HasPrefix(config.Image, "-") || strings.ContainsAny(config.Image, " \t\r\n\x00") {
		return "", fmt.Errorf("sandbox image must be an explicit local image name")
	}
	output, err := c.run(ctx, engine, "image", "inspect", config.Image)
	if err != nil {
		return "", fmt.Errorf("inspect local sandbox image %q (build or pull it explicitly): %w", config.Image, err)
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

func (c *Containers) localEngine(ctx context.Context) (sandboxEngine, error) {
	var engine sandboxEngine
	if err := ctx.Err(); err != nil {
		return engine, err
	}
	if runtime.GOOS != "linux" || os.Getuid() == 0 || os.Geteuid() != os.Getuid() || os.Getgid() == 0 {
		return engine, fmt.Errorf("sandbox requires a nonroot Linux user and local rootless Podman")
	}
	for _, key := range sandboxEngineEnvKeys[5:] {
		_, present := os.LookupEnv(key)
		if c.getenv(key) != "" || present {
			return engine, fmt.Errorf("sandbox requires local Podman without routing or storage overrides; unset %s", key)
		}
	}
	for _, key := range sandboxEngineEnvKeys {
		// HomeDir/Getenv may select fake OpenCode directories, not a different engine home.
		if value := os.Getenv(key); value != "" {
			engine.Env = append(engine.Env, key+"="+value)
		}
	}
	output, err := c.run(ctx, engine, "info", "--format", "{{json .}}")
	if err != nil {
		return engine, fmt.Errorf("check Podman engine: %w", err)
	}
	var info struct {
		Host struct {
			ServiceIsRemote bool
			DatabaseBackend string
			Security        struct {
				Rootless bool
			}
		}
		Store struct {
			GraphRoot, RunRoot, GraphDriverName, ConfigFile string
			TransientStore                                  bool
		}
	}
	if err := json.Unmarshal(output, &info); err != nil {
		return engine, fmt.Errorf("decode Podman engine info: %w", err)
	}
	if info.Host.ServiceIsRemote {
		return engine, fmt.Errorf("sandbox requires a local engine, not a remote Podman connection")
	}
	if !info.Host.Security.Rootless || info.Store.GraphDriverName == "" {
		return engine, fmt.Errorf("sandbox requires Podman to identify its local rootless storage")
	}
	for _, path := range []string{info.Store.GraphRoot, info.Store.RunRoot, info.Store.ConfigFile} {
		if err := sandboxMountPath(path); err != nil {
			return engine, fmt.Errorf("invalid Podman storage identity: %w", err)
		}
	}
	scope := []string{"podman", strconv.Itoa(os.Getuid()), info.Store.GraphRoot, info.Store.RunRoot, info.Store.GraphDriverName, info.Store.ConfigFile, info.Host.DatabaseBackend, strconv.FormatBool(info.Store.TransientStore)}
	engine.Endpoint = fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(scope, "\x00"))))
	return engine, nil
}

func (c *Containers) inspect(ctx context.Context, engine sandboxEngine, name string) (*sandboxInspection, error) {
	output, err := c.run(ctx, engine, "container", "inspect", name)
	if err != nil {
		// An inspect failure alone is not evidence that the container is absent.
		listed, listErr := c.run(ctx, engine, "container", "ls", "--all", "--filter", "name=^/?"+regexp.QuoteMeta(name)+"$", "--format", "{{.Names}}")
		if listErr != nil {
			return nil, fmt.Errorf("inspect sandbox %s: %w; confirm absence: %v", name, err, listErr)
		}
		if strings.TrimSpace(string(listed)) == "" {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect sandbox %s: %w", name, err)
	}
	var containers []sandboxInspection
	if err := json.Unmarshal(output, &containers); err != nil {
		return nil, fmt.Errorf("decode sandbox inspection: %w", err)
	}
	if len(containers) != 1 || containers[0].ID == "" || strings.TrimPrefix(containers[0].Name, "/") != name {
		return nil, fmt.Errorf("sandbox inspection did not identify %s", name)
	}
	if !sandboxContainerName.MatchString(containers[0].ID) {
		return nil, fmt.Errorf("sandbox inspection returned an invalid container ID")
	}
	return &containers[0], nil
}

func sandboxLabels(w Workspace, engine sandboxEngine) map[string]string {
	return map[string]string{
		"owner": "cli-workmux", "policy": sandboxPolicy, "workspace": w.ID, "repository": w.RepoID,
		"root": w.Root, "common": w.CommonDir, "path": w.Path, "runtime": "podman", "image": w.Config.Sandbox.Image,
		"endpoint": engine.Endpoint,
	}
}

func sandboxOwned(found *sandboxInspection, w Workspace, engine sandboxEngine) error {
	for key, value := range sandboxLabels(w, engine) {
		if found.Config.Labels[sandboxLabel+key] != value {
			return fmt.Errorf("refusing sandbox %s: ownership label %s does not match", found.Name, key)
		}
	}
	if !sandboxDigest.MatchString(found.Config.Labels[sandboxLabel+"mounts"]) || !sandboxDigest.MatchString(strings.TrimPrefix(found.Image, "sha256:")) || found.Config.Labels[sandboxLabel+"image-id"] != found.Image {
		return fmt.Errorf("refusing sandbox %s: missing protection or image identity labels", found.Name)
	}
	return nil
}

func (c *Containers) lookup(ctx context.Context, w Workspace) (sandboxEngine, string, *sandboxInspection, error) {
	name, err := sandboxName(w)
	if err != nil {
		return sandboxEngine{}, "", nil, err
	}
	engine, err := c.localEngine(ctx)
	if err != nil {
		return engine, "", nil, err
	}
	if err := c.endpoint(w, engine, false); err != nil {
		return engine, "", nil, err
	}
	found, err := c.inspect(ctx, engine, name)
	if err == nil && found != nil {
		err = sandboxOwned(found, w, engine)
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
		return fmt.Errorf("sandbox engine identity changed; restore the original local engine and storage before acting on this workspace")
	}
	return nil
}

func (c *Containers) Ensure(ctx context.Context, w Workspace) error {
	if !w.Config.Sandbox.Enabled {
		return fmt.Errorf("sandbox is disabled")
	}
	image, err := c.checkImage(ctx, w.Config.Sandbox)
	if err != nil {
		return err
	}
	engine, name, found, err := c.lookup(ctx, w)
	if err != nil {
		return err
	}
	plan, err := c.mountPlan(ctx, w, image, found == nil)
	if err != nil {
		return fmt.Errorf("protect sandbox mounts: %w", err)
	}
	if found != nil {
		if err := sandboxCurrent(found, plan, image); err != nil {
			return err
		}
	} else {
		if err := plan.snapshots(true); err != nil {
			return err
		}
		if err := c.endpoint(w, engine, true); err != nil {
			return err
		}
		if _, err := c.run(ctx, engine, c.createArgs(w, name, engine, image, plan)...); err != nil {
			return fmt.Errorf("create sandbox: %w", err)
		}
		found, err = c.inspect(ctx, engine, name)
		if err != nil {
			return err
		}
		if found == nil {
			return fmt.Errorf("created sandbox %s is missing", name)
		}
		if err := sandboxOwned(found, w, engine); err != nil {
			return err
		}
		if err := sandboxCurrent(found, plan, image); err != nil {
			return err
		}
	}
	if !found.State.Running {
		if _, err := c.run(ctx, engine, "start", found.ID); err != nil {
			return fmt.Errorf("start sandbox: %w", err)
		}
	}
	return nil
}

func sandboxCurrent(found *sandboxInspection, plan sandboxMountPlan, image string) error {
	if found.Image != image || found.Config.Labels[sandboxLabel+"mounts"] != plan.Fingerprint {
		return fmt.Errorf("sandbox %s has outdated image or mount protection; explicitly remove and recreate it", found.Name)
	}
	return plan.snapshots(false)
}

func (c *Containers) createArgs(w Workspace, name string, engine sandboxEngine, image string, plan sandboxMountPlan) []string {
	args := []string{"create", "--name", name, "--pull=never", "--init", "--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()), "--cap-drop=ALL", "--security-opt=no-new-privileges", "--userns=keep-id"}
	labels := sandboxLabels(w, engine)
	labels["mounts"] = plan.Fingerprint
	labels["image-id"] = image
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		args = append(args, "--label", sandboxLabel+key+"="+labels[key])
	}
	for _, mount := range plan.Mounts {
		value := "type=bind,source=" + mount.Source + ",target=" + mount.Target
		if mount.ReadOnly {
			value += ",readonly"
		}
		args = append(args, "--mount", value)
	}
	args = append(args, "--workdir", w.Path)
	for _, entry := range plan.Env {
		args = append(args, "--env", entry)
	}
	return append(args, "--entrypoint", "/usr/bin/sleep", image, "infinity")
}

func (c *Containers) Exec(ctx context.Context, w Workspace, command string, env []string, stdin io.Reader, stdout, stderr io.Writer) error {
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !sandboxEnvKey(key) || strings.ContainsRune(entry, 0) {
			return fmt.Errorf("invalid sandbox environment assignment %q", key)
		}
	}
	if err := c.Ensure(ctx, w); err != nil {
		return err
	}
	engine, _, found, err := c.lookup(ctx, w)
	if err != nil {
		return err
	}
	if found == nil || !found.State.Running {
		return fmt.Errorf("sandbox is not running")
	}
	args := []string{"exec"}
	if stdin != nil {
		args = append(args, "-i")
	}
	args = append(args, "--workdir", w.Path)
	for _, entry := range env {
		args = append(args, "--env", entry)
	}
	args = append(args, found.ID, "bash", "-c", command)
	p := engine.process(args...)
	p.Stdin, p.Stdout, p.Stderr = stdin, stdout, stderr
	_, err = c.Runner.Run(ctx, p)
	if err != nil {
		return fmt.Errorf("exec sandbox: %w", err)
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

func (c *Containers) PaneCommand(w Workspace, command string) []string {
	if !w.Config.Sandbox.Enabled {
		return []string{"/usr/bin/false"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	engine, _, found, err := c.lookup(ctx, w)
	if err != nil || found == nil || !found.State.Running {
		// Empty argv would let tmux start a host shell instead of a sandbox.
		return []string{"/usr/bin/false"}
	}
	plan, err := c.mountPlan(ctx, w, found.Image, false)
	if err != nil || sandboxCurrent(found, plan, found.Image) != nil {
		return []string{"/usr/bin/false"}
	}
	args := append(engine.panePrefix(), "exec", "-it", "--workdir", w.Path, found.ID, "bash")
	if command == "" {
		return append(args, "-i")
	}
	return append(args, "-c", command)
}

func (c *Containers) Stop(ctx context.Context, w Workspace) error {
	engine, _, found, err := c.lookup(ctx, w)
	if err != nil || found == nil || !found.State.Running {
		return err
	}
	if _, err := c.run(ctx, engine, "stop", found.ID); err != nil {
		return fmt.Errorf("stop sandbox: %w", err)
	}
	return nil
}

func (c *Containers) Remove(ctx context.Context, w Workspace) error {
	engine, _, found, err := c.lookup(ctx, w)
	if err != nil || found == nil {
		return err
	}
	if _, err := c.run(ctx, engine, "rm", "--force", found.ID); err != nil {
		return fmt.Errorf("remove sandbox: %w", err)
	}
	return nil
}
