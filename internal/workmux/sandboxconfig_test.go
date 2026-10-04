package workmux

import (
	"os"
	"strings"
	"testing"
)

func TestSandboxCommandsRejectMisconfiguration(t *testing.T) {
	for _, name := range []string{"build", "run"} {
		t.Run(name, func(t *testing.T) {
			c, w, engine := sandboxTestFixture(t)
			app := sandboxCommandApp(c, w)
			if err := os.MkdirAll(app.ConfigDir, 0700); err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, app.ConfigDir, "config.yaml", "sandbox: {enabeld: true}\n")
			before := len(engine.calls)
			command := Command{Kind: "sandbox", Name: name}
			if name == "run" {
				command.ShellCommand = []string{"pwd"}
			}
			if err := app.Run(t.Context(), command); err == nil || !strings.Contains(err.Error(), "config.sandbox.enabeld") {
				t.Fatalf("invalid config was not rejected: %v", err)
			}
			for _, process := range engine.calls[before:] {
				if sandboxTestPodmanProcess(process).Name == "podman" {
					t.Fatalf("invalid config invoked Podman: %+v", process)
				}
			}
		})
	}
}
