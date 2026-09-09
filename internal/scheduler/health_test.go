package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// hostHealth is the health of one Host in the Snapshot.
func hostHealth(t *testing.T, e *env, name string) HostHealth {
	t.Helper()
	for _, h := range e.sched.Snapshot().Hosts {
		if h.Name == name {
			return h
		}
	}
	t.Fatalf("no host %q in the snapshot", name)
	return HostHealth{}
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# The Scheduler SHALL probe every Host in the Inventory at the
//# configured health interval by calling `ServerInfo`, falling back to
//# `ListMicroVMs` on the Runner's namespace when `ServerInfo` is not
//# implemented by the Host.

func TestProbeCallsServerInfoOnEveryHostAtTheConfiguredInterval(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	first, second := newCountingHost("host-1"), newCountingHost("host-2")
	e := newEnv(t, envConfig{
		inventory: []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")},
		hosts:     map[string]flintlock.HostClient{"host-1": first, "host-2": second},
		tune:      func(s *Settings) { s.Scheduler.HostHealthInterval = 15 * time.Second },
	})
	e.startBare(ctx)

	probeCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.sched.probeLoop(probeCtx)
	}()
	defer func() {
		stop()
		<-done
	}()

	// The first round runs at once, so a Host that is down is known before
	// the first Job is placed on it.
	waitFor(t, ctx, func() bool {
		_, _, a := first.counts()
		_, _, b := second.counts()
		return a == 1 && b == 1
	})

	// The next round is one health interval later.
	e.advance(ctx, 1, 15*time.Second)
	waitFor(t, ctx, func() bool {
		_, _, a := first.counts()
		_, _, b := second.counts()
		return a == 2 && b == 2
	})
	if get, list, _ := first.counts(); get != 0 || list != 0 {
		t.Fatalf("host-1 saw %d GetMicroVM and %d ListMicroVMs calls, want none while ServerInfo works", get, list)
	}
	if got := hostHealth(t, e, "host-1"); !got.Healthy || got.Info == nil || got.Info.Version != "test" {
		t.Fatalf("host-1 health = %+v, want healthy with the reported version", got)
	}
}

func TestProbeFallsBackToListMicroVMsWhenServerInfoIsUnimplemented(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	// A Host that predates the ServerInfo RPC, as TD-026 produces.
	old := newFakeHost(t, "host-old")
	unimplemented := &unimplementedInfoHost{HostClient: runnerClient(t, old)}

	e := newEnv(t, envConfig{
		inventory: []config.HostEntry{testHostEntry("host-old")},
		hosts:     map[string]flintlock.HostClient{"host-old": unimplemented},
		tune:      func(s *Settings) { s.Namespace = testNamespace },
	})
	e.startBare(ctx)
	e.sched.probeAll(ctx)

	if got := hostHealth(t, e, "host-old"); !got.Healthy {
		t.Fatalf("host health = %+v, want healthy through the ListMicroVMs fallback", got)
	}
	if got := unimplemented.namespaces(); len(got) != 1 || got[0] != testNamespace {
		t.Fatalf("ListMicroVMs namespaces = %v, want only the runner namespace %q", got, testNamespace)
	}
	if got := hostHealth(t, e, "host-old"); got.Info == nil || got.Info.VersionKnown {
		t.Fatalf("host info = %+v, want an unknown version", got.Info)
	}
}

// unimplementedInfoHost is a Host whose ServerInfo is not implemented, and
// which records the namespaces ListMicroVMs was called with.
type unimplementedInfoHost struct {
	flintlock.HostClient
	mu   sync.Mutex
	seen []string
}

func (h *unimplementedInfoHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	return nil, fmt.Errorf("server info: %w", flintlock.ErrUnimplemented)
}

func (h *unimplementedInfoHost) ListMicroVMs(ctx context.Context, namespace string) ([]*types.MicroVM, error) {
	h.mu.Lock()
	h.seen = append(h.seen, namespace)
	h.mu.Unlock()
	return h.HostClient.ListMicroVMs(ctx, namespace)
}

func (h *unimplementedInfoHost) namespaces() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# When a Host fails the configured number of consecutive probes,
//# the Scheduler SHALL mark the Host unhealthy.

func TestHostBecomesUnhealthyAfterTheConfiguredNumberOfFailures(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newCountingHost("host-1")
	e := newEnv(t, envConfig{
		hosts: map[string]flintlock.HostClient{"host-1": host},
		tune:  func(s *Settings) { s.Scheduler.HostUnhealthyThreshold = 3 },
	})
	e.startBare(ctx)

	host.set(func(h *countingHost) { h.infoErr = flintlock.ErrUnavailable })
	for probe := 1; probe < 3; probe++ {
		e.sched.probeAll(ctx)
		got := hostHealth(t, e, "host-1")
		if !got.Healthy {
			t.Fatalf("host marked unhealthy after %d of 3 failed probes", probe)
		}
		if got.ConsecutiveFailures != probe {
			t.Fatalf("consecutive failures = %d, want %d", got.ConsecutiveFailures, probe)
		}
	}

	e.sched.probeAll(ctx)
	if got := hostHealth(t, e, "host-1"); got.Healthy {
		t.Fatal("host still healthy after three consecutive failed probes")
	}
	if len(e.logs.find("marked unhealthy")) != 1 {
		t.Fatal("the transition to unhealthy was not logged")
	}
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# When an unhealthy Host passes a probe, the Scheduler SHALL mark
//# the Host healthy.

func TestUnhealthyHostBecomesHealthyOnTheNextGoodProbe(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newCountingHost("host-1")
	e := newEnv(t, envConfig{
		hosts: map[string]flintlock.HostClient{"host-1": host},
		tune:  func(s *Settings) { s.Scheduler.HostUnhealthyThreshold = 1 },
	})
	e.startBare(ctx)

	host.set(func(h *countingHost) { h.infoErr = flintlock.ErrUnavailable })
	e.sched.probeAll(ctx)
	if hostHealth(t, e, "host-1").Healthy {
		t.Fatal("host is healthy after a failed probe with a threshold of one")
	}

	host.set(func(h *countingHost) { h.infoErr = nil })
	e.sched.probeAll(ctx)
	got := hostHealth(t, e, "host-1")
	if !got.Healthy {
		t.Fatal("host is not healthy again after a successful probe")
	}
	if got.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures after a success = %d, want 0", got.ConsecutiveFailures)
	}
	if len(e.logs.find("healthy again")) != 1 {
		t.Fatal("the recovery was not logged")
	}
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# When a Host becomes unhealthy, the Scheduler SHALL abort every
//# Job whose MicroVM is placed on that Host with the failure reason
//# `runner_system_failure` and release their Leases.

func TestUnhealthyHostAbortsItsJobsAndReleasesTheirLeases(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	failing, healthy := newCountingHost("host-1"), newCountingHost("host-2")
	client := newStubClient()
	client.script(func(c *stubClient) {
		// The first Job lands on the Host that is about to fail, the second
		// on the one that stays healthy.
		c.claimFn = claimsFrom("host-1", "host-2")
		c.beatFn = beatsFor(clock.NewFake(testEpoch), time.Hour)
	})
	e := newEnv(t, envConfig{
		client:    client,
		inventory: []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")},
		hosts:     map[string]flintlock.HostClient{"host-1": failing, "host-2": healthy},
		tune:      func(s *Settings) { s.Scheduler.HostUnhealthyThreshold = 1 },
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 5)

	doomed := e.allocate(ctx, JobInfo{ID: 1}, "default")
	survivor := e.allocate(ctx, JobInfo{ID: 2}, "default")
	if doomed.Allocation().Placement.Host != "host-1" || survivor.Allocation().Placement.Host != "host-2" {
		t.Fatalf("placements = %q and %q, want host-1 and host-2",
			doomed.Allocation().Placement.Host, survivor.Allocation().Placement.Host)
	}

	failing.set(func(h *countingHost) { h.infoErr = flintlock.ErrUnavailable })
	e.sched.probeAll(ctx)

	select {
	case <-doomed.Done():
	case <-ctx.Done():
		t.Fatal("the job on the unhealthy host was not aborted")
	}
	if !errors.Is(doomed.Err(), ErrHostUnhealthy) {
		t.Fatalf("aborted job error = %v, want ErrHostUnhealthy", doomed.Err())
	}
	if !contains(doomed.Err().Error(), "host-1") {
		t.Fatalf("abort error %q does not name the host", doomed.Err())
	}
	if got := waitForRelease(t, ctx, client); got != doomed.Allocation().Lease.ID {
		t.Fatalf("released lease = %v, want the aborted job's %s", got, doomed.Allocation().Lease.ID)
	}

	// The Job on the healthy Host is untouched, and only one Slot came back.
	select {
	case <-survivor.Done():
		t.Fatal("the job on the healthy host was aborted too")
	default:
	}
	waitFor(t, ctx, func() bool { return e.sched.Snapshot().SlotsInUse == 1 })

	// The Executor's Cleanup arrives afterwards and must not release twice.
	e.sched.Release(doomed)
	if got := client.releaseCount(); got != 1 {
		t.Fatalf("release calls = %d, want 1", got)
	}
	e.sched.Release(survivor)
}

//= docs/requirements/03-scheduler.md#host-health
//= type=test
//# The Scheduler SHALL NOT refuse Reservations because a Host is
//# unhealthy, because the Pool Manager decides where warm MicroVMs live.

func TestReservationsAreGrantedWhileAHostIsUnhealthy(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	first, second := newCountingHost("host-1"), newCountingHost("host-2")
	e := newEnv(t, envConfig{
		inventory: []config.HostEntry{testHostEntry("host-1"), testHostEntry("host-2")},
		hosts:     map[string]flintlock.HostClient{"host-1": first, "host-2": second},
		tune:      func(s *Settings) { s.Scheduler.HostUnhealthyThreshold = 1 },
	})
	e.startBare(ctx)
	e.tracker.setAvailable(e.poolOf("default"), 3)

	// Every Host in the Inventory is unhealthy; the Pool Manager still
	// reports warm MicroVMs, so Reservations are still granted.
	first.set(func(h *countingHost) { h.infoErr = flintlock.ErrUnavailable })
	second.set(func(h *countingHost) { h.infoErr = flintlock.ErrUnavailable })
	e.sched.probeAll(ctx)
	for _, name := range []string{"host-1", "host-2"} {
		if hostHealth(t, e, name).Healthy {
			t.Fatalf("%s is still healthy", name)
		}
	}

	if _, err := e.sched.Reserve(ctx); err != nil {
		t.Fatalf("Reserve with every host unhealthy: %v", err)
	}
	if got := e.metrics.refusal(RefusalNoWarmMicroVM); got != 0 {
		t.Fatalf("refusals counted = %d, want none", got)
	}
	// Readiness, unlike capacity, does depend on Host health (OB-031).
	if e.sched.Snapshot().Ready() {
		t.Fatal("the scheduler reports ready with no healthy host")
	}
}
