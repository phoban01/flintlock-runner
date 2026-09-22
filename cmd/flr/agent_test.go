package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli"
)

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL reach `flintlockd` only through the local
//# endpoint of HI-042, and the Fleet Manifests SHALL run it as the user id
//# that HI-063 admits there and run no other container as that user id.

// TestAgentRefusesARemoteFlintlockd checks that `flr agent` exits with the
// invalid-configuration status, naming the field, before it connects to
// anything when flintlockd is configured at an address off the Host; and
// that the agent refuses to start as any user but the one flintlockd
// admits.
func TestAgentRefusesARemoteFlintlockd(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	config := "host_node: host-a\nflintlockd: 10.0.0.5:9090\ntls: {cert_file: c, key_file: k}\nguard: {namespace: flintlock-system}\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	var stderr bytes.Buffer
	app.ErrWriter = &stderr
	err := app.Run([]string{"flr", "agent", "--agent-config", path})
	var exit cli.ExitCoder
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != exitInvalidConfig {
		t.Fatalf("flr agent = %v, want exit status %d", err, exitInvalidConfig)
	}
	if !strings.Contains(err.Error(), "flintlockd:") || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not name the flintlockd field and why", err)
	}

	if err := checkAgentUser(10250, 10250); err != nil {
		t.Errorf("the admitted user: %v", err)
	}
	if err := checkAgentUser(10250, 0); err == nil {
		t.Error("the agent may start as root")
	}
	if err := checkAgentUser(-1, 1000); err != nil {
		t.Errorf("with the check switched off: %v", err)
	}
}
