package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/urfave/cli"

	"gitlab.com/gitlab-org/gitlab-runner/common"
)

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
	cli.OsExiter = func(int) {}
	err := app.Run([]string{"flintlock-runner", "fleet", "verify"})
	if !errors.Is(err, ErrNotImplemented) && !strings.Contains(err.Error(), ErrNotImplemented.Error()) {
		t.Fatalf("error = %v, want not implemented", err)
	}
	var exitErr cli.ExitCoder
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitNotImplemented {
		t.Errorf("exit code = %v, want %d", err, exitNotImplemented)
	}
	_ = os.Stderr
}
