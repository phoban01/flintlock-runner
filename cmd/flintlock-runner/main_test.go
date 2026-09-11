package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli"

	"gitlab.com/gitlab-org/gitlab-runner/common"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// TestMain disarms the cli package's process exit once for the whole
// package, so that a command returning a cli.ExitCoder does not end the test
// binary and so that no test writes the global concurrently with another.
func TestMain(m *testing.M) {
	cli.OsExiter = func(int) {}
	os.Exit(m.Run())
}

//= docs/requirements/01-gitlab-protocol.md#library-basis
//= type=test
//# The Runner SHALL be a single Go program that imports the
//# gitlab-runner `common`, `network`, `shells` and `executors` packages and
//# SHALL NOT execute a `gitlab-runner` binary.

// TestRunLoopIsBuiltFromTheLibrary checks that the run loop is the
// gitlab-runner library's own command, that the bash shell implementation is
// linked in through the shells package, and that no gitlab-runner binary is
// on the PATH the test runs with, so nothing here could have exec'd one.
func TestRunLoopIsBuiltFromTheLibrary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := exec.LookPath("gitlab-runner"); err == nil {
		t.Fatal("a gitlab-runner binary is reachable; the test cannot show the library path is used")
	}

	cmd, client := newRunLoopCommand(map[string]common.ExecutorProvider{})
	if cmd.Name != "run" {
		t.Errorf("run loop command name = %q, want run", cmd.Name)
	}
	if client == nil {
		t.Error("network client is nil")
	}
	if common.GetShell("bash") == nil {
		t.Error("bash shell not registered; the shells package is not linked in")
	}
}

func TestSubcommandsAreRegistered(t *testing.T) {
	t.Parallel()
	app := newApp()
	want := map[string][]string{
		"run":    nil,
		"config": {"show"},
		"fleet":  {"provision", "verify", "drain", "teardown", "emit-userdata"},
	}
	for _, c := range app.Commands {
		subs, ok := want[c.Name]
		if !ok {
			t.Errorf("unexpected command %q", c.Name)
			continue
		}
		delete(want, c.Name)
		var got []string
		for _, s := range c.Subcommands {
			got = append(got, s.Name)
		}
		if strings.Join(got, ",") != strings.Join(subs, ",") {
			t.Errorf("%s subcommands = %v, want %v", c.Name, got, subs)
		}
	}
	for name := range want {
		t.Errorf("command %q not registered", name)
	}
}

func TestNotImplementedExitCode(t *testing.T) {
	t.Parallel()
	app := newApp()
	app.Writer = &bytes.Buffer{}
	app.ErrWriter = &bytes.Buffer{}
	err := app.Run([]string{"flintlock-runner", "fleet", "verify"})
	if !errors.Is(err, ErrNotImplemented) && !strings.Contains(err.Error(), ErrNotImplemented.Error()) {
		t.Fatalf("error = %v, want not implemented", err)
	}
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitNotImplemented {
		t.Errorf("exit code = %v, want %d", err, exitNotImplemented)
	}
}

// writeConfig writes a configuration file into a new temporary directory and
// returns its path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimalConfigFile is the smallest valid configuration, with a secret in it.
const minimalConfigFile = `
gitlab:
  url: https://gitlab.example.com
  token: glrt-super-secret
  name: metal-runner
pool_manager:
  endpoint: 10.0.0.5:9091
inventory:
  hosts:
    - name: host-a
      endpoint: 10.0.1.10:9090
      arch: arm64
      vcpu: 8
      memory_mb: 16384
      token: host-a-super-secret
profiles:
  - name: default
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    pool:
      size: 2
`

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# The Runner SHALL provide a `config show` command that prints the
//# effective configuration with every secret value redacted.

// TestConfigShow runs the command the way an operator does, through the
// command-line application, and checks that it prints the effective
// configuration (the defaults included) with no secret in it.
func TestConfigShow(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, minimalConfigFile)

	var out, errOut bytes.Buffer
	app := newApp()
	app.Writer = &out
	app.ErrWriter = &errOut
	if err := app.Run([]string{"flintlock-runner", "--config", path, "config", "show"}); err != nil {
		t.Fatalf("config show: %v", err)
	}

	shown := out.String()
	for _, secret := range []string{"glrt-super-secret", "host-a-super-secret"} {
		if strings.Contains(shown, secret) {
			t.Errorf("config show leaked %q:\n%s", secret, shown)
		}
	}
	if strings.Count(shown, config.Redacted) != 2 {
		t.Errorf("want one redaction marker per secret:\n%s", shown)
	}
	// The effective configuration, so the defaults the file leaves out are
	// printed too.
	for _, want := range []string{"shell: /bin/bash", "namespace: metal-runner", "log_format: json"} {
		if !strings.Contains(shown, want) {
			t.Errorf("output lacks the defaulted %q:\n%s", want, shown)
		}
	}
}

// TestConfigShowWithTheDefaultErrorWriter runs config show on an app whose
// ErrWriter is left unset, as main leaves it, over a file without a
// distributed_cache section, whose startup notice (CF-083) is logged. It
// used to panic on a nil writer; the end-to-end harness found it.
func TestConfigShowWithTheDefaultErrorWriter(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, minimalConfigFile)

	var out bytes.Buffer
	app := newApp()
	app.Writer = &out
	if err := app.Run([]string{"flintlock-runner", "--config", path, "config", "show"}); err != nil {
		t.Fatalf("config show: %v", err)
	}
	if !strings.Contains(out.String(), "namespace: metal-runner") {
		t.Errorf("config show printed no configuration:\n%s", out.String())
	}
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# When starting, the Runner SHALL validate the configuration and,
//# if it is invalid, SHALL exit with a non-zero status and a message naming
//# the first invalid field and the reason.

// TestConfigShowRejectsAnInvalidFile checks the start path: an invalid file
// makes the command exit non-zero with a message naming the first invalid
// field and the reason, and print nothing.
func TestConfigShowRejectsAnInvalidFile(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, strings.Replace(minimalConfigFile, "url: https://gitlab.example.com", `url: ""`, 1))

	var out, errOut bytes.Buffer
	app := newApp()
	app.Writer = &out
	app.ErrWriter = &errOut
	err := app.Run([]string{"flintlock-runner", "--config", path, "config", "show"})
	if err == nil {
		t.Fatalf("config show accepted an invalid configuration and printed:\n%s", out.String())
	}
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) || exitErr.ExitCode() == 0 {
		t.Fatalf("error = %v, want a non-zero exit status", err)
	}
	if exitErr.ExitCode() != exitInvalidConfig {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode(), exitInvalidConfig)
	}
	if !strings.Contains(err.Error(), "gitlab.url: is required") {
		t.Errorf("message does not name the first invalid field and the reason: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("an invalid configuration was printed anyway:\n%s", out.String())
	}
}
