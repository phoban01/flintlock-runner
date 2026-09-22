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

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL reach `flintlockd` only through the
//# local endpoint of HI-042.

// TestKubeletRefusesARemoteFlintlockd checks that `flr kubelet` exits with
// the invalid-configuration status, naming the field, before it connects to
// anything when flintlockd is configured at an address off the Host.
func TestKubeletRefusesARemoteFlintlockd(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kubelet.yaml")
	config := "host_node: host-a\nflintlockd: 10.0.0.5:9090\nmax_microvms: 4\n" +
		"tls: {cert_file: c, key_file: k, client_ca_file: ca}\nguard: {namespace: flintlock-system}\n"
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	app := newApp()
	var stderr bytes.Buffer
	app.ErrWriter = &stderr
	err := app.Run([]string{"flr", "kubelet", "--kubelet-config", path})
	var exit cli.ExitCoder
	if err == nil || !errors.As(err, &exit) || exit.ExitCode() != exitInvalidConfig {
		t.Fatalf("flr kubelet = %v, want exit status %d", err, exitInvalidConfig)
	}
	if !strings.Contains(err.Error(), "flintlockd:") || !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error %q does not name the flintlockd field and why", err)
	}
}
