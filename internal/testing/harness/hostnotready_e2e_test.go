//go:build e2e

package harness

import (
	"context"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# If a claim that a Job holds has the condition `HostReady`
//# false, then the Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and delete the claim.

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# When the Scheduler aborts a Job under KF-203, the Scheduler
//# SHALL name the reason and the message of the claim's `HostReady`
//# condition in the Job's failure.

// TestClaimStackHostNotReady reports the Host of the Job's claim not ready
// while a Stage runs, as battery-operator does when the Host's KVM device
// or thin pool has gone. The Host's flintlockd and its Exec Agent go on
// answering, so the Exec Agent probe passes (KF-200) and only the claim's
// HostReady condition tells the Runner. The Job fails as a system failure
// that names the condition's reason and message, within KF-205's bound,
// does not finish, and its claim is deleted.
func TestClaimStackHostNotReady(t *testing.T) {
	runOn(t, ClaimTier(), nil, func(t *testing.T, s *Stack) {
		gate := s.NewGate("not-ready")
		defer func() { _ = gate.Open() }()
		id, err := s.Enqueue(Job{
			Name: "not-ready", Timeout: jobTimeout,
			Script: []string{`echo "job started"`, gate.Wait(), `echo "job finished"`},
		})
		if err != nil {
			t.Fatal(err)
		}
		waitTrace(t, s, id, "job started")
		hosts, err := s.LeaseHosts(context.Background())
		if err != nil || len(hosts) != 1 {
			t.Fatalf("lease hosts %v (%v), want one", hosts, err)
		}
		host := hosts[0]

		const reason, message = "KVMUnavailable", "/dev/kvm is missing"
		started := time.Now()
		s.Claim.Battery.SetHostNotReady(host, reason, message)
		defer s.Claim.Battery.SetHostReady(host)
		rec := wait(t, s, id)
		took := time.Since(started)

		wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
		wantTrace(t, rec, "Job failed (system failure): scheduler: lease lost", reason+": "+message)
		// The Exec Agent still answers: the probe did not fail the Job.
		wantNotInTrace(t, rec, "job finished", "the exec agent of claim")
		limit := hostReadyBound(s.Config) + s.Config.Executor.GracefulKillTimeout + reportSlack
		if took > limit {
			t.Errorf("the job took %s to fail after its host was reported not ready, want within %s", took, limit)
		}
		t.Logf("the job failed %s after its host was reported not ready (KF-205's bound %s)", took, hostReadyBound(s.Config))
		eventually(t, "the claim to be deleted", func() bool {
			claims, err := s.BoundClaims(context.Background())
			return err == nil && len(claims) == 0
		})
	})
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# The Scheduler SHALL abort a Job under KF-203 no later than the
//# heartbeat interval of the claim's Pool plus the Pool Manager call
//# deadline after the claim's condition `HostReady` becomes false.

// hostReadyBound is KF-205's bound for the Runner's configuration: the
// heartbeat interval of the Profile's Pool plus the Pool Manager call
// deadline.
func hostReadyBound(cfg *config.Config) time.Duration {
	return cfg.Profiles[0].Pool.HeartbeatInterval + cfg.PoolManager.Deadline
}
