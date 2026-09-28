//go:build e2e

package harness

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/testing/fakegitlab"
)

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The harness SHALL include a scenario in which the Exec Agent
//# restarts while a Stage runs, and SHALL assert that the Job fails with a
//# stream failure and is never reported as having succeeded.

// TestClaimStackExecAgentRestart restarts the Exec Agent of the Job's Host
// while a Stage runs, as a restart of battery-operator's Exec Agent pod
// does. The Stage's exec stream ends without an exit status, which the
// agent-exec Guest Transport takes as a stream failure (KF-187). The Job
// fails as a system failure, the Stage is not run again (EX-023), and the
// Job is never reported as having succeeded.
func TestClaimStackExecAgentRestart(t *testing.T) {
	runOn(t, ClaimTier(), nil, func(t *testing.T, s *Stack) {
		gate := s.NewGate("restart")
		defer func() { _ = gate.Open() }()
		id, err := s.Enqueue(Job{
			Name:        "restarted",
			Script:      []string{`echo "stage started"`, gate.Wait(), `echo "stage finished"`},
			AfterScript: []string{`echo "after_script ran"`},
		})
		if err != nil {
			t.Fatal(err)
		}
		waitTrace(t, s, id, "stage started")
		hosts, err := s.LeaseHosts(context.Background())
		if err != nil || len(hosts) != 1 {
			t.Fatalf("lease hosts %v (%v), want one", hosts, err)
		}
		agent := s.Claim.Agent(hosts[0])
		if err := agent.Restart(); err != nil {
			t.Fatal(err)
		}
		rec := wait(t, s, id)

		wantStatus(t, rec, fakegitlab.StatusFailed, "runner_system_failure")
		if slices.Contains(rec.States, fakegitlab.StatusSuccess) {
			t.Errorf("the job was reported as having succeeded: states %v", rec.States)
		}
		wantTrace(t, rec, "stream failed before exit status")
		wantNotInTrace(t, rec, "stage finished", "Job succeeded")
		// The trace shows each command before it runs.
		if n := strings.Count(rec.Trace, `$ echo "stage started"`); n != 1 {
			t.Errorf("the stage started %d times, want once (EX-023):\n%s", n, rec.Trace)
		}
	})
}
