package scripts

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// ensureServiceCall is a unit a provisioning script enables and starts.
var ensureServiceCall = regexp.MustCompile(`(?m)^\s*ensure_service ([A-Za-z0-9_.@-]+)`)

// verifyActiveUnits is the list of units verify_active checks.
var verifyActiveUnits = regexp.MustCompile(`(?m)^units=\(([^)]*)\)`)

// teardownLoop is the teardown script's loop over the units it disables.
var teardownLoop = regexp.MustCompile(`(?ms)^for u in ([^;\n]+); do\n(.*?)^done$`)

// installedUnits is every systemd unit the Host provisioning steps enable,
// rendered with every Host Service on, together with the units
// verify_active requires to be active (FL-053), which include the ones the
// flintlock host provisioner installs.
func installedUnits(t *testing.T) []string {
	t.Helper()
	var units []string
	add := func(u string) {
		if !slices.Contains(units, u) {
			units = append(units, u)
		}
	}
	for _, step := range userDataSteps {
		s := render(t, step, fullInput())
		for _, m := range ensureServiceCall.FindAllStringSubmatch(s, -1) {
			add(m[1])
		}
		if step == fleet.StepVerifyActive {
			m := verifyActiveUnits.FindStringSubmatch(s)
			if m == nil {
				t.Fatal("verify_active has no units list")
			}
			for _, u := range strings.Fields(m[1]) {
				add(u)
			}
		}
	}
	// The pool agent is added to verify_active's list at run time, when
	// the pinned release ships it.
	if len(units) < 8 || !slices.Contains(units, "poolmgr-hostagent") || !slices.Contains(units, "flintlockd") {
		t.Fatalf("installed units = %v; the provisioning steps changed shape and this test has to follow", units)
	}
	return units
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The Fleet Controller SHALL provide a teardown command that
//# deletes the declared Pools through the Pool Manager, waits for their
//# MicroVMs to be removed, stops and disables the installed services on every
//# Host and removes the Inventory.

// TestTeardownDisablesEveryInstalledUnit renders the real teardown template
// and checks that it stops and disables every unit the provisioning steps
// install, and starts nothing.
func TestTeardownDisablesEveryInstalledUnit(t *testing.T) {
	t.Parallel()
	for name, in := range map[string]fleet.RenderInput{"full": fullInput(), "sparse": sparseInput()} {
		td := render(t, fleet.StepTeardown, in)
		m := teardownLoop.FindStringSubmatch(td)
		if m == nil {
			t.Fatalf("%s: the teardown script has no loop over units:\n%s", name, td)
		}
		listed := strings.Fields(m[1])
		if !strings.Contains(m[2], `systemctl disable --now --quiet "$u"`) {
			t.Errorf("%s: the teardown loop does not stop and disable its units:\n%s", name, m[2])
		}
		for _, u := range installedUnits(t) {
			if !slices.Contains(listed, u) {
				t.Errorf("%s: teardown leaves %s enabled; it disables only %v", name, u, listed)
			}
		}
		// Past the shared prelude, nothing enables or starts a unit.
		_, body, ok := strings.Cut(td, "# Teardown stops")
		if !ok {
			t.Fatalf("%s: the teardown script lost its header comment", name)
		}
		mustNotContain(t, body, "ensure_service", "systemctl enable", "systemctl start", "systemctl restart", "reload-or-restart")
	}
}

//= docs/requirements/06-fleet.md#drain-and-teardown
//= type=test
//# The teardown command SHALL NOT remove the thin pool or its
//# backing device unless the purge flag is passed explicitly.

// TestTeardownRemovesThinPoolOnlyWithPurge renders the real teardown
// template with every purge option but "true" and checks that nothing in it
// removes a volume group, physical volume, logical volume or wipes a device;
// with "true" it removes the thin pool's volume group and its physical
// volumes.
func TestTeardownRemovesThinPoolOnlyWithPurge(t *testing.T) {
	t.Parallel()
	destructive := []string{"vgremove", "pvremove", "lvremove", "wipefs", "blkdiscard", "mkfs", "/dev/nvme1n1"}
	for _, opts := range []map[string]string{nil, {}, {OptionPurge: ""}, {OptionPurge: "false"}, {OptionPurge: "yes"}, {OptionPurge: "TRUE"}} {
		in := fullInput()
		in.Options = opts
		td := render(t, fleet.StepTeardown, in)
		mustNotContain(t, td, destructive...)
		mustContain(t, td, "thin pool "+ThinPool+" kept; pass the purge flag to remove it")
	}

	in := fullInput()
	in.Options = map[string]string{OptionPurge: "true"}
	td := render(t, fleet.StepTeardown, in)
	mustContain(t, td, "vg='"+ThinPool+"'", `vgremove -f -y "$vg"`, `pvremove -y "$pv"`, `pvs --noheadings -o pv_name -S vg_name="$vg"`)
	// The pool is removed after the units are down, so containerd no
	// longer holds it.
	before(t, td, "done\n", `vgremove -f -y "$vg"`)
}
