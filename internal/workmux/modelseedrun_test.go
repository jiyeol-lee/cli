package workmux

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSandboxRunSeedsModelWithoutMountingState(t *testing.T) {
	sandboxTestNonroot(t)
	c, w, engine := sandboxTestFixture(t)
	source := filepath.Join(c.HomeDir, ".local", "state", "opencode", "model.json")
	command := "printf '%s' 'two words'; exit 0"
	var commands []string
	c.Runner = sandboxCommandRunner(func(ctx context.Context, p Process) ([]byte, error) {
		podman := sandboxTestPodmanProcess(p)
		if podman.Name == "podman" && podman.Args[0] == "run" {
			commands = append(commands, podman.Args[len(podman.Args)-1])
			for i, arg := range podman.Args {
				if arg == "--mount" && strings.Contains(podman.Args[i+1], filepath.Dir(source)) {
					t.Fatalf("host model state mounted: %s", podman.Args[i+1])
				}
			}
			sandboxTestSession(t, engine, podman.Args)
		}
		return engine.Run(ctx, p)
	})
	for _, data := range []string{`{"model":"first"}`, `{"model":"second"}`} {
		sandboxTestWrite(t, source, data)
		var warnings bytes.Buffer
		if err := c.RunStandalone(t.Context(), w, []string{command}, nil, nil, &warnings, nil); err != nil {
			t.Fatal(err)
		}
		if warnings.Len() != 0 {
			t.Fatalf("warnings = %q", &warnings)
		}
		if len(commands) == 0 || commands[len(commands)-1] != modelSeedCommand(command, []byte(data), modelSeedDestination) {
			t.Fatal("standalone run did not seed the current host model state")
		}
		assertModelSeedBytes(t, source, []byte(data))
		if len(engine.sessions) != 0 {
			t.Fatal("standalone run left a container")
		}
	}
	if len(commands) != 2 || commands[0] == commands[1] {
		t.Fatal("standalone runs did not receive independent snapshots")
	}
	app := App{Sandbox: c}
	commandsBefore := len(commands)
	argv, err := app.paneCommands(t.Context(), w, []Pane{{Command: "opencode"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) != 1 || !slices.Equal(argv[0], []string{"cli", "workmux", "sandbox", "run", "--", "opencode"}) {
		t.Fatalf("pane did not use the model-seeding public run path: %q", argv)
	}
	if len(commands) != commandsBefore {
		t.Fatal("pane preparation launched a sandbox synchronously")
	}
}
