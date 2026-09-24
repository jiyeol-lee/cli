package workmux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

type standaloneRun struct {
	Workspace Workspace
	Session   string
	Endpoint  string
}

func standaloneDirectory(state string, w Workspace) string {
	return filepath.Join(state, "standalone", identity(w.CommonDir, w.Path))
}

func (c *Containers) registerStandalone(w Workspace, engine sandboxEngine) (func(bool) error, error) {
	dir := standaloneDirectory(c.StateDir, w)
	if err := sandboxDirectory(dir, true, true); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, w.SandboxRun+".json")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, err
	}
	if err := json.NewEncoder(file).Encode(standaloneRun{w, w.SandboxRun, engine.Endpoint}); err != nil {
		file.Close()
		return nil, err
	}
	return func(clean bool) error {
		var err error
		if clean {
			err = os.RemoveAll(filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(w), "runs", w.SandboxRun))
			if err == nil {
				err = os.Remove(path)
			}
		}
		return errors.Join(err, file.Close())
	}, nil
}

// Called under the repository lock, before any destructive operation.
func (c *Containers) checkStandalone(ctx context.Context, w Workspace) error {
	dir := standaloneDirectory(c.StateDir, w)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if _, err := sandboxRegular(path); err != nil {
			return err
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = c.reconcileStandalone(ctx, w, file)
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Containers) reconcileStandalone(ctx context.Context, w Workspace, file *os.File) error {
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("checkout has an active standalone sandbox; exit it before removing or merging: %w", err)
	}
	var run standaloneRun
	if err := json.NewDecoder(file).Decode(&run); err != nil {
		return fmt.Errorf("read standalone registration: %w", err)
	}
	if run.Workspace.Path != w.Path || run.Workspace.CommonDir != w.CommonDir || !sandboxContainerName.MatchString(run.Session) || filepath.Base(file.Name()) != run.Session+".json" {
		return fmt.Errorf("standalone registration identity does not match checkout")
	}
	engine, err := c.sessionEngine(ctx)
	if err != nil {
		return err
	}
	if engine.Endpoint != run.Endpoint {
		return fmt.Errorf("standalone sandbox endpoint changed; select its original Podman connection")
	}
	name := "cli-workmux-" + sandboxWorkspaceKey(run.Workspace)[:16] + "-" + run.Session
	found, err := c.inspect(ctx, engine, name)
	if err != nil {
		return err
	}
	if found != nil {
		return fmt.Errorf("checkout has a registered standalone sandbox %s; remove it at its original Podman endpoint before removing or merging", name)
	}
	if err := os.RemoveAll(filepath.Join(c.StateDir, "containers", sandboxWorkspaceKey(run.Workspace), "runs", run.Session)); err != nil {
		return err
	}
	return os.Remove(file.Name())
}

func (app *App) checkStandalone(ctx context.Context, w Workspace) error {
	if c, ok := app.Sandbox.(*Containers); ok {
		return c.checkStandalone(ctx, w)
	}
	return nil
}
