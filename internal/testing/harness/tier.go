package harness

import (
	"os"
	"testing"
)

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//# Where the environment variable naming a hardware Inventory is
//# set, the harness SHALL run the same scenarios against the real Hosts it
//# lists instead of fake Hosts, and SHALL be skipped otherwise.

// HardwareTier returns the Options for running scenarios against the real
// Hosts named by EnvHardwareInventory (TD-052), with the Pool Manager
// switch and images read from the environment too, and skips t when the
// variable is unset. A test that runs the scenario table under it is the
// hardware tier; the same table under FakeTier is the fake one.
func HardwareTier(t testing.TB) Options {
	t.Helper()
	return hardwareTier(t, os.LookupEnv)
}

func hardwareTier(t testing.TB, lookup func(string) (string, bool)) Options {
	t.Helper()
	opts := optionsFromLookup(Options{}, lookup)
	if !opts.Hardware() {
		t.Skipf("hardware tier skipped: %s names no hardware Inventory", EnvHardwareInventory)
	}
	return opts
}

// FakeTier returns the Options for running scenarios against fake Hosts,
// which always runs. It honours EnvRunnerBinary, and EnvPoolManager
// (TD-053) unless a hardware Inventory is set as well: a real Pool Manager
// set up next to a hardware Inventory reaches those Hosts, not the fake
// ones on this machine's ephemeral ports, so the fake tier then keeps its
// fake Pool Manager and the hardware tier gets the real one.
func FakeTier() Options {
	return fakeTier(os.LookupEnv)
}

func fakeTier(lookup func(string) (string, bool)) Options {
	opts := optionsFromLookup(Options{}, lookup)
	if opts.Hardware() {
		opts.PoolManagerEndpoint = ""
	}
	opts.HardwareInventory = ""
	opts.KernelImage, opts.RootFSImage = "", ""
	return opts
}
