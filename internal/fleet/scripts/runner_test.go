package scripts

import (
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// runnerInput is fullInput with the runner step's options as fleet up
// --install-runner sets them.
func runnerInput() fleet.RenderInput {
	in := fullInput()
	in.Options = map[string]string{
		OptionRunnerConfig:      "/etc/flintlock-runner/config.yaml",
		OptionRunnerBinary:      "/home/ops/flintlock-runner",
		OptionRunnerStateDir:    "/var/lib/flintlock-runner",
		OptionRunnerReads:       "/etc/flintlock-runner/config.yaml\n/etc/flintlock-runner/inventory.yaml\n/etc/flintlock-runner/tls/ca.pem",
		OptionRunnerStopTimeout: "1m30s",
	}
	return in
}

//= docs/requirements/06-fleet.md#one-shot-operation
//= type=test
//# Where the install-runner flag is passed, the up command SHALL
//# install the Runner as a systemd service on the Control Node that uses the
//# generated Runner configuration, and SHALL enable and start it and verify
//# that it is active.

// TestRunnerStepInstallsEnablesAndChecksTheService renders the runner step
// and checks that it installs the binary, writes a unit that runs it as the
// unprivileged Runner user with the generated configuration, enables and
// starts it, and fails unless it stays active; and that its remove mode
// only stops and disables the service.
func TestRunnerStepInstallsEnablesAndChecksTheService(t *testing.T) {
	t.Parallel()
	sc := render(t, fleet.StepRunner, runnerInput())
	mustContain(t, sc,
		"install -D -m 0755 '/home/ops/flintlock-runner' \"$bin\"",
		"bin='/usr/local/bin/flintlock-runner'",
		"config='/etc/flintlock-runner/config.yaml'",
		`ensure_user "$user" "$state"`,
		"User=$user\nGroup=$user\n",
		"ExecStart=$bin --config $config run\n",
		"ExecReload=/bin/kill -HUP \\$MAINPID\n",
		"TimeoutStopSec=90\n",
		"WantedBy=multi-user.target",
		`ensure_service "$unit" "$restart"`,
		`restarts=$(systemctl show -p NRestarts --value "$unit")`+"\nsleep 10\n"+`status=$(systemctl is-active "$unit" || true)`+"\n"+
			`if [ "$status" != active ] || [ "$(systemctl show -p NRestarts --value "$unit")" != "$restarts" ]; then`+"\n",
		`die "$unit did not stay active (it is $status)"`+"\nfi\nprintf '::active:: %s\\n' \"$unit\"\n",
		"reads=('/etc/flintlock-runner/config.yaml' '/etc/flintlock-runner/inventory.yaml' '/etc/flintlock-runner/tls/ca.pem')",
		`runuser -u "$user" -- test -r "$f" || die "$user cannot read $f"`,
	)
	mustNotContain(t, sc, "User=root", "systemctl disable")

	in := runnerInput()
	in.Options[OptionRunnerRemove] = "true"
	rm := render(t, fleet.StepRunner, in)
	mustContain(t, rm, `systemctl disable --now --quiet "$unit"`)
	mustNotContain(t, rm, `ensure_service "$unit"`, "ExecStart=", "setfacl", "install -D")

	// Every path lands on the unit's ExecStart line or in a shell word.
	for _, bad := range []string{"relative/config.yaml", "/etc/flintlock runner/config.yaml", "/etc/%n.yaml", "/etc/$HOME.yaml"} {
		in := runnerInput()
		in.Options[OptionRunnerConfig] = bad
		s, err := New()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Render(fleet.StepRunner, in); err == nil {
			t.Errorf("the runner step accepted the configuration path %q", bad)
		}
	}
	if !strings.Contains(render(t, fleet.StepRunner, fleet.RenderInput{}), "config='/etc/flintlock-runner/config.yaml'") {
		t.Error("without options the runner step does not start the Runner with the default configuration path")
	}
}
