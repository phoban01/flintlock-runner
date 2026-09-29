package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
)

// probeInterval is the Host health interval of these tests. It differs from
// the Profile's heartbeat interval so that awaitTimer finds the probe's
// timer and not the keep-alive loop's.
const probeInterval = 7 * time.Second

// stubAgents is a scripted AgentProber. Each probe takes the next answer
// from the script, and the last answer repeats.
type stubAgents struct {
	mu     sync.Mutex
	script []func(context.Context) error
	calls  []probeCall
}

// probeCall is what one probe was asked.
type probeCall struct {
	alloc       Allocation
	deadline    time.Duration
	hasDeadline bool
}

func (s *stubAgents) ProbeAgent(ctx context.Context, a Allocation) error {
	deadline, ok := ctx.Deadline()
	s.mu.Lock()
	s.calls = append(s.calls, probeCall{alloc: a, deadline: time.Until(deadline), hasDeadline: ok})
	answer := func(context.Context) error { return nil }
	if n := len(s.script); n > 0 {
		answer = s.script[0]
		if n > 1 {
			s.script = s.script[1:]
		}
	}
	s.mu.Unlock()
	return answer(ctx)
}

func (s *stubAgents) probes() []probeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]probeCall(nil), s.calls...)
}

var (
	errAgentDown = errors.New("connection refused")
	agentDown    = func(context.Context) error { return errAgentDown }
	agentUp      = func(context.Context) error { return nil }
	// agentHangs is an agent that never answers: the probe ends only with
	// its deadline.
	agentHangs = func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
)

// newProbeEnv is a claim backend Scheduler with agents as its probe, a
// Host health interval of probeInterval and the given threshold, holding
// one Allocation whose heartbeats always pass.
func newProbeEnv(t *testing.T, ctx context.Context, agents *stubAgents, threshold int, deadline time.Duration) (*env, *stubClient, Handle) {
	t.Helper()
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("node-7")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{
		client:    client,
		inventory: []config.HostEntry{},
		agents:    agents,
		tune: func(s *Settings) {
			claimBackend(s)
			s.Scheduler.HostHealthInterval = probeInterval
			s.Scheduler.HostUnhealthyThreshold = threshold
			s.Scheduler.HostCallDeadline = deadline
		},
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)
	return e, client, e.allocate(ctx, JobInfo{ID: 21}, "default")
}

// probeOnce moves the fake clock to the next probe and waits until it has
// been made.
func (e *env) probeOnce(ctx context.Context, agents *stubAgents) {
	e.t.Helper()
	before := len(agents.probes())
	e.awaitTimer(ctx, probeInterval)
	e.clk.Advance(probeInterval)
	waitFor(e.t, ctx, func() bool { return len(agents.probes()) > before })
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# Where the claim backend is configured, the Scheduler SHALL
//# probe the Exec Agent of each claim that a Job holds at the configured
//# Host health interval, by calling `GetMicroVM` for the claim's MicroVM
//# with a claim token of that claim, bounded by the configured Host call
//# deadline.

// TestAgentProbeRunsAtTheHealthInterval holds a claim on the claim backend
// and checks that its Exec Agent is probed once per Host health interval,
// for the claim's lease and MicroVM, with the Host call deadline, and not
// before the first interval has passed. It stops once the Job releases
// the claim.
func TestAgentProbeRunsAtTheHealthInterval(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	agents := &stubAgents{}
	e, _, h := newProbeEnv(t, ctx, agents, 3, 2*time.Second)

	if n := len(agents.probes()); n != 0 {
		t.Fatalf("%d probes before the first interval, want none", n)
	}
	for i := 1; i <= 3; i++ {
		e.probeOnce(ctx, agents)
		if n := len(agents.probes()); n != i {
			t.Fatalf("after %d intervals, %d probes, want %d", i, n, i)
		}
	}
	alloc := h.Allocation()
	for _, p := range agents.probes() {
		if p.alloc.Lease.ID != alloc.Lease.ID || p.alloc.VMUID != alloc.VMUID || p.alloc.Host != alloc.Host {
			t.Errorf("probe of %+v, want the claim's %+v", p.alloc, alloc)
		}
		if !p.hasDeadline || p.deadline > 2*time.Second {
			t.Errorf("probe deadline %v (set %t), want at most the call deadline of 2s", p.deadline, p.hasDeadline)
		}
	}
	select {
	case <-h.Done():
		t.Fatalf("a Job whose agent answers was aborted: %v", h.Err())
	default:
	}

	e.sched.Release(h)
	waitFor(t, ctx, func() bool { return e.clk.Timers() == 0 })
	if n := len(agents.probes()); n != 3 {
		t.Errorf("%d probes after the release, want 3", n)
	}
}

// TestNoAgentProbeWithoutAProber checks that a Scheduler with no
// AgentProber, the battery backend's, arms no probe timer for a claim.
func TestNoAgentProbeWithoutAProber(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	client := newStubClient()
	client.script(func(c *stubClient) {
		c.claimFn = claimsFrom("host-1")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{client: client, tune: func(s *Settings) { s.Scheduler.HostHealthInterval = probeInterval }})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 1)
	h := e.allocate(ctx, JobInfo{ID: 22}, "default")
	defer e.sched.Release(h)
	waitFor(t, ctx, func() bool { return e.clk.Timers() > 0 })
	for _, d := range e.clk.Deadlines() {
		if d.Equal(e.clk.Now().Add(probeInterval)) {
			t.Fatal("a probe timer was armed with no AgentProber")
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# If the Exec Agent of a claim that a Job holds fails as many
//# consecutive probes as the configured Host unhealthy threshold, then the
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure` and delete the claim.

// TestAgentProbeAbortsAfterTheThreshold fails every probe of a claim's
// Exec Agent. One probe short of the threshold the Job still runs; at the
// threshold its Handle is failed with ErrHostUnhealthy, which the Executor
// reports as runner_system_failure, and the claim's Lease is released,
// which deletes the claim.
func TestAgentProbeAbortsAfterTheThreshold(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	agents := &stubAgents{script: []func(context.Context) error{agentDown}}
	e, client, h := newProbeEnv(t, ctx, agents, 3, time.Second)

	e.probeOnce(ctx, agents)
	e.probeOnce(ctx, agents)
	select {
	case <-h.Done():
		t.Fatalf("the Job was aborted after 2 failed probes of 3: %v", h.Err())
	default:
	}
	if n := client.releaseCount(); n != 0 {
		t.Fatalf("%d releases before the threshold, want none", n)
	}

	e.probeOnce(ctx, agents)
	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the Job was not aborted at the threshold")
	}
	if err := h.Err(); !errors.Is(err, ErrHostUnhealthy) || !strings.Contains(err.Error(), errAgentDown.Error()) {
		t.Errorf("Err = %v, want ErrHostUnhealthy naming the probe's error", err)
	}
	select {
	case lease := <-client.releasedCh:
		if lease != h.Allocation().Lease.ID {
			t.Errorf("released lease %q, want the claim's %q", lease, h.Allocation().Lease.ID)
		}
	case <-ctx.Done():
		t.Fatal("the claim was not deleted")
	}
	if n := e.sched.Snapshot().SlotsInUse; n != 0 {
		t.Errorf("%d slots in use after the abort, want none", n)
	}
}

// TestAgentProbePassResetsTheCount fails the probes of a claim's Exec
// Agent one short of the threshold, twice, with a passing probe between:
// an agent that restarts between probes does not abort the Job.
func TestAgentProbePassResetsTheCount(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	agents := &stubAgents{script: []func(context.Context) error{agentDown, agentUp, agentDown, agentUp}}
	e, client, h := newProbeEnv(t, ctx, agents, 2, time.Second)
	defer e.sched.Release(h)

	for range 4 {
		e.probeOnce(ctx, agents)
	}
	select {
	case <-h.Done():
		t.Fatalf("the Job was aborted though no two probes failed in a row: %v", h.Err())
	default:
	}
	if n := client.releaseCount(); n != 0 {
		t.Errorf("%d releases, want none", n)
	}
}

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# The Scheduler SHALL abort a Job under KF-201 no later than the
//# Host unhealthy threshold times the sum of the Host health interval and
//# the Host call deadline after the claim's Exec Agent stops answering.

//= docs/requirements/12-cluster-fleet.md#claim-host-faults
//= type=test
//# Scheduler SHALL abort the Job with the failure reason
//# `runner_system_failure`

// TestAgentProbeAbortsWithinTheBound has an Exec Agent that stops answering
// at once, so each probe waits out the Host call deadline. The next probe
// is due one Host health interval after the last one returned, so the Job
// is aborted after the threshold's worth of intervals on the clock, each
// with at most one call deadline of waiting: within the threshold times
// their sum. A probe that waited without a deadline would hang this test.
// The abort names the probes' deadline but does not wrap it, so that the
// Job is still reported as runner_system_failure.
func TestAgentProbeAbortsWithinTheBound(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	const threshold, deadline = 3, 50 * time.Millisecond
	agents := &stubAgents{script: []func(context.Context) error{agentHangs}}
	e, _, h := newProbeEnv(t, ctx, agents, threshold, deadline)

	start, began := e.clk.Now(), time.Now()
	for range threshold {
		e.probeOnce(ctx, agents)
	}
	select {
	case <-h.Done():
	case <-ctx.Done():
		t.Fatal("the Job was not aborted")
	}
	onClock, waited := e.clk.Now().Sub(start), time.Since(began)
	if bound := threshold * probeInterval; onClock > bound {
		t.Errorf("aborted %v after the agent went quiet on the clock, want within %v", onClock, bound)
	}
	// Each probe waited out its deadline in real time, and nothing else
	// did; allow for a slow test machine, but not for a probe with no
	// deadline or with the default one.
	if lo, hi := threshold*deadline, threshold*deadline+5*time.Second; waited < lo || waited > hi {
		t.Errorf("the probes waited %v in all, want between %v and %v", waited, lo, hi)
	}
	err := h.Err()
	if !errors.Is(err, ErrHostUnhealthy) || !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("Err = %v, want ErrHostUnhealthy naming probes that ran out of time", err)
	}
	// gitlab-runner reports a Job error that matches either context error
	// as job_execution_timeout or job_canceled, whatever failure reason it
	// carries, so the probe's deadline must not show through.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Errorf("Err = %v matches a context error, which gitlab-runner would not report as runner_system_failure", err)
	}
}
