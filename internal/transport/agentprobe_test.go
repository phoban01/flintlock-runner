package transport_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# by calling `GetMicroVM` for the claim's MicroVM
//# with a claim token of that claim

// TestAgentProbe probes the Exec Agent's test double in front of a fake
// Host through battery-operator's Client Library. A probe of a held claim
// passes, and reaches the agent as GetMicroVM for the claim's MicroVM with
// a claim token of that claim. It fails while the Host does not answer,
// passes again once it does, fails for a MicroVM the claim does not name,
// and fails once the claim is released.
func TestAgentProbe(t *testing.T) {
	t.Parallel()
	f := newClaimFixture(t)
	vmA := f.claim(f.claims, "job-a")
	vmB := f.claim(f.claims, "job-b")
	claimA := transport.AgentClaim{LeaseID: "job-a", VMUID: vmA, Host: agentNode, Address: f.agent.Addr()}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	if err := f.agents.Probe(ctx, claimA); err != nil {
		t.Fatalf("Probe of a held claim = %v, want nil", err)
	}
	var probes []string
	for _, c := range f.agent.Calls() {
		if c.Method == "GetMicroVM" && c.Admitted {
			if c.VMUID != vmA {
				t.Errorf("the probe asked about microvm %q, want the claim's %q", c.VMUID, vmA)
			}
			probes = append(probes, c.Token)
		}
	}
	if len(probes) != 1 {
		t.Fatalf("the agent admitted %d GetMicroVM calls, want the probe's one", len(probes))
	}
	if !slices.Contains(f.world.Tokens("job-a"), probes[0]) {
		t.Errorf("the probe carried %q, which is no claim token of job-a", probes[0])
	}

	// A Host that stops answering fails the probe within the probe's own
	// deadline.
	f.host.SetFaults(flintlock.HostFaults{Unresponsive: true})
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	err := f.agents.Probe(short, claimA)
	stop()
	if err == nil {
		t.Error("Probe of an unresponsive Host = nil, want an error")
	}
	f.host.SetFaults(flintlock.HostFaults{})
	if err := f.agents.Probe(ctx, claimA); err != nil {
		t.Errorf("Probe once the Host answers again = %v, want nil", err)
	}

	// The claim's token opens no other MicroVM.
	across := claimA
	across.VMUID = vmB
	if err := f.agents.Probe(ctx, across); err == nil {
		t.Error("Probe of another claim's MicroVM with job-a's claim = nil, want an error")
	}

	// A released claim answers no probe.
	f.mu.Lock()
	held := f.held["job-a"]
	f.mu.Unlock()
	if err := held.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.agents.Probe(ctx, claimA); err == nil {
		t.Error("Probe after the claim's release = nil, want an error")
	}
}
