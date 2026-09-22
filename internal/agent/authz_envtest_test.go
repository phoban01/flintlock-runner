package agent_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/phoban01/flintlock-runner/internal/agent"
	"github.com/phoban01/flintlock-runner/internal/agent/agenttest"
)

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL authenticate every request with a
//# TokenReview of the bearer token it carries, and SHALL refuse a request
//# that does not authenticate.

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL run a command in a MicroVM only for an
//# identity that created a `MicroVMClaim` which is `Bound`, has not expired,
//# and names that MicroVM's uid and this Host, and SHALL refuse every other
//# request.

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# The Exec Agent and the `agent-exec` Guest Transport SHALL be
//# tested against a Kubernetes API server test environment serving the
//# claim resources from a test definition, and the fake Host, with no KVM
//# and no battery.

// TestExecOnlyForTheHolderOfABoundClaim tries to run a command that leaves
// a marker in the guest, once per way of not holding the claim, and once
// with it. Every caller is a real ServiceAccount token the agent reviews
// with a real TokenReview, and every claim is a real object of the claim
// resource's test definition. Each refusal is checked to be the right
// status and to have run nothing on the fake Host; the one caller that
// holds a Bound, unexpired claim on this MicroVM and this Host runs its
// command.
func TestExecOnlyForTheHolderOfABoundClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t, agenttest.HostOptions{})
	other := newFixture(t, agenttest.HostOptions{})
	stranger := env.ServiceAccountToken(t, f.ns, "stranger")
	hour := time.Now().Add(time.Hour)
	bound := func() agenttest.ClaimStatus {
		return agenttest.ClaimStatus{Phase: agent.ClaimBound, VMUID: f.host.VMUID, HostNode: f.host.Node, ExpiresAt: hour}
	}

	cases := []struct {
		name    string
		token   string
		claim   func(name string)
		want    codes.Code
		allowed bool
	}{
		{name: "no token", token: "", want: codes.Unauthenticated},
		{name: "a bad token", token: "not-a-token", want: codes.Unauthenticated},
		{name: "a valid token with no claim", token: f.runner.Token, want: codes.PermissionDenied},
		{name: "a pending claim", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.runner.User, agenttest.ClaimStatus{Phase: agent.ClaimPending, ExpiresAt: hour})
		}},
		{name: "an expired claim", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.Phase = agent.ClaimExpired
			env.PutClaim(t, f.ns, name, f.runner.User, st)
		}},
		{name: "a released claim", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.Phase = agent.ClaimReleased
			env.PutClaim(t, f.ns, name, f.runner.User, st)
		}},
		{name: "a bound claim whose lease has run out", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.ExpiresAt = time.Now().Add(-time.Minute)
			env.PutClaim(t, f.ns, name, f.runner.User, st)
		}},
		{name: "a claim for another microvm", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.VMUID = other.host.VMUID
			env.PutClaim(t, f.ns, name, f.runner.User, st)
		}},
		{name: "a claim for another host", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			st := bound()
			st.HostNode = other.host.Node
			env.PutClaim(t, f.ns, name, f.runner.User, st)
		}},
		{name: "a claim created by someone else", token: stranger.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.runner.User, bound())
		}},
		{name: "a claim that records no creator", token: f.runner.Token, want: codes.PermissionDenied, claim: func(name string) {
			env.PutClaim(t, f.ns, name, "", bound())
		}},
		{name: "a bound claim of the caller", token: f.runner.Token, allowed: true, claim: func(name string) {
			env.PutClaim(t, f.ns, name, f.runner.User, bound())
		}},
	}
	for i, tc := range cases {
		name := "claim-" + string(rune('a'+i))
		if tc.claim != nil {
			tc.claim(name)
		}
		marker := "ran-" + name
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		// The agent reads claims afresh for every request, but it learns
		// which claims to read from a cache; a refusal must not be the
		// cache being behind, so each case gives it a moment first.
		time.Sleep(200 * time.Millisecond)
		code, gotExit, _, err := execRaw(ctx, f.rawExec(tc.token), f.host.VMUID, "touch "+marker)
		cancel()
		if tc.allowed {
			if err != nil || !gotExit || code != 0 {
				t.Errorf("%s: exec = (exit %d, %v, %v), want the command run", tc.name, code, gotExit, err)
			}
			if !f.ran(marker) {
				t.Errorf("%s: the command did not run on the host", tc.name)
			}
		} else {
			if got := statusCode(err); got != tc.want {
				t.Errorf("%s: exec ended with %v (%v), want %v", tc.name, got, err, tc.want)
			}
			if gotExit {
				t.Errorf("%s: a refused exec carried an exit code", tc.name)
			}
			if f.ran(marker) {
				t.Errorf("%s: the refused command ran on the host", tc.name)
			}
		}
		env.DeleteClaim(t, f.ns, name)
	}

	// The claim is checked on every request, not once per connection: a
	// claim released while the caller stays connected stops working.
	f.bind("released-later")
	time.Sleep(200 * time.Millisecond)
	client := f.rawExec(f.runner.Token)
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	if _, gotExit, _, err := execRaw(ctx, client, f.host.VMUID, "true"); err != nil || !gotExit {
		t.Fatalf("exec with a bound claim = (%v, %v)", gotExit, err)
	}
	env.DeleteClaim(t, f.ns, "released-later")
	if _, _, _, err := execRaw(ctx, client, f.host.VMUID, "touch ran-after-release"); statusCode(err) != codes.PermissionDenied {
		t.Errorf("exec after the claim was deleted = %v, want permission denied", err)
	}
	if f.ran("ran-after-release") {
		t.Error("a command ran after its claim was deleted")
	}
}
