package claim_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

// The Scheduler's scenarios over the claim backend and the fake battery:
// allocate, exhausted then retry, lease lost, and release. The Scheduler,
// the Declarer, the Tracker and Health are the real ones, as cmd/flr wires
// them, with no Inventory: the claim names the Host.

//= docs/requirements/12-cluster-fleet.md#battery-claims
//= type=test
//# The claim backend SHALL implement the `poolmgr.Client`
//# interface, so that the Scheduler's requirements in `03-scheduler.md` hold
//# unchanged over it.

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//= type=test
//# and binds claims from warm
//# MicroVMs on the fake Hosts.

// TestSchedulerAllocatesAndReleases allocates a MicroVM for a Job through
// the Scheduler: the Pool is declared from the Profile, the claim binds on
// a fake Host, the Placement is that Host, and releasing the Allocation
// deletes the claim.
func TestSchedulerAllocatesAndReleases(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.scheduler()

	h := s.allocate(1)
	alloc := h.Allocation()
	obj, ok := f.claim(alloc.Lease.ID)
	if !ok {
		t.Fatalf("no claim named %s, the allocation's lease", alloc.Lease.ID)
	}
	if alloc.VMUID != obj.Status.MicroVM.UID || alloc.Placement.Host != obj.Status.Host.NodeName {
		t.Errorf("allocation = vm %s on %s, want the claim's %s on %s",
			alloc.VMUID, alloc.Placement.Host, obj.Status.MicroVM.UID, obj.Status.Host.NodeName)
	}
	if !isFakeHost(alloc.Placement.Host) {
		t.Errorf("placement %s is not a fake host", alloc.Placement.Host)
	}

	s.sched.Release(h)
	f.eventually("the claim is deleted", func() bool {
		_, ok := f.claim(alloc.Lease.ID)
		return !ok
	})
}

// TestSchedulerWaitsOutAnExhaustedPool allocates the only MicroVM of a Pool
// of one, then a second Job. The second claim finds the Pool exhausted and
// is deleted, and the Job waits (SC-021) until the first Job's release puts
// a warm MicroVM back, which the Pool's watch reports, and then it gets it.
func TestSchedulerWaitsOutAnExhaustedPool(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.scheduler()

	r1 := s.reserve()
	r2 := s.reserve()
	first, err := s.sched.Allocate(f.ctx, r1, scheduler.JobInfo{ID: 1}, s.profile)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		h   scheduler.Handle
		err error
	}
	second := make(chan result, 1)
	go func() {
		h, err := s.sched.Allocate(f.ctx, r2, scheduler.JobInfo{ID: 2}, s.profile)
		second <- result{h, err}
	}()

	f.eventually("the second claim finds the pool exhausted", func() bool {
		for _, reason := range f.battery.Pending() {
			if reason == batteryv1alpha1.ReasonPoolExhausted {
				return true
			}
		}
		return false
	})
	f.eventually("the exhausted claim is deleted", func() bool { return len(f.claims()) == 1 })
	select {
	case res := <-second:
		t.Fatalf("second allocation returned %v, %v while the pool was exhausted", res.h, res.err)
	default:
	}

	s.sched.Release(first)
	select {
	case res := <-second:
		if res.err != nil {
			t.Fatalf("second allocation = %v, want it granted after the release", res.err)
		}
		if _, ok := f.claim(res.h.Allocation().Lease.ID); !ok {
			t.Error("the second allocation holds no claim")
		}
		s.sched.Release(res.h)
	case <-time.After(waitFor):
		t.Fatal("the release did not wake the waiting allocation")
	}
}

// TestSchedulerAbortsOnALostLease allocates two MicroVMs and loses both
// Leases, one because the claim becomes Expired and one because it is
// deleted. The Scheduler aborts each Job with ErrLeaseLost (SC-061).
func TestSchedulerAbortsOnALostLease(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 2
	s := f.scheduler()

	expired := s.allocate(1)
	deleted := s.allocate(2)

	f.battery.Expire(expired.Allocation().Lease.ID)
	obj, _ := f.claim(deleted.Allocation().Lease.ID)
	if err := f.kube.Delete(f.ctx, obj); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]scheduler.Handle{"expired": expired, "deleted": deleted} {
		select {
		case <-h.Done():
			if !errors.Is(h.Err(), scheduler.ErrLeaseLost) {
				t.Errorf("%s: job ended with %v, want ErrLeaseLost", name, h.Err())
			}
		case <-time.After(waitFor):
			t.Fatalf("%s: the job was not aborted", name)
		}
	}
}

// schedulerEnv is a running Scheduler over the fixture's backend.
type schedulerEnv struct {
	f       *fixture
	backend *claim.Backend
	sched   scheduler.Scheduler
	profile *scheduler.Profile
}

// scheduler builds and runs the Scheduler over a new backend, as cmd/flr
// does for the claim backend, and waits until it can grant a Reservation.
func (f *fixture) scheduler() *schedulerEnv {
	f.t.Helper()
	f.profile.Default = true
	b := f.backend()
	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	health, err := poolmgr.NewHealth(poolmgr.HealthConfig{Admin: b, Namespace: runnerNamespace, Log: log})
	if err != nil {
		f.t.Fatal(err)
	}
	tracker, err := poolmgr.NewTracker(poolmgr.TrackerConfig{Events: b, Admin: b, Health: health, Log: log})
	if err != nil {
		f.t.Fatal(err)
	}
	hosts, err := flintlock.NewRegistry(f.ctx, noHostDialer{}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = hosts.Close() })
	settings := scheduler.Settings{
		RunnerName:      runnerName,
		Namespace:       runnerNamespace,
		Slots:           2,
		ShutdownTimeout: 5 * time.Second,
		Scheduler:       config.Scheduler{AllocationTimeout: waitFor},
		PoolManager:     config.PoolManager{Backend: config.PoolBackendClaim, Deadline: 5 * time.Second},
		Profiles:        []config.Profile{f.profile},
	}
	sched, err := scheduler.New(scheduler.Deps{
		PoolManager:  b,
		Specs:        poolmgr.NewSpecBuilder(),
		HostSelector: poolmgr.NewHostSelector(),
		Declarer:     poolmgr.NewDeclarer(b),
		Tracker:      tracker,
		Health:       health,
		Hosts:        hosts,
		Logger:       log,
	}, settings)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = sched.Run(ctx) }()
	f.t.Cleanup(func() { cancel(); wg.Wait() })

	profile, err := sched.ResolveProfile(scheduler.JobInfo{ID: 0})
	if err != nil {
		f.t.Fatal(err)
	}
	s := &schedulerEnv{f: f, backend: b, sched: sched, profile: profile}
	f.eventually("the pool is declared and counted", func() bool {
		snap := sched.Snapshot()
		if !snap.PoolManagerContacted {
			return false
		}
		for _, p := range snap.Pools {
			if p.Pool == profile.PoolRef && int(p.Status.Available) == f.profile.Pool.Size {
				return true
			}
		}
		return false
	})
	return s
}

// reserve takes a Reservation.
func (s *schedulerEnv) reserve() *scheduler.Reservation {
	s.f.t.Helper()
	r, err := s.sched.Reserve(s.f.ctx)
	if err != nil {
		s.f.t.Fatal(err)
	}
	return r
}

// allocate reserves and allocates for a Job.
func (s *schedulerEnv) allocate(job int64) scheduler.Handle {
	s.f.t.Helper()
	h, err := s.sched.Allocate(s.f.ctx, s.reserve(), scheduler.JobInfo{ID: job}, s.profile)
	if err != nil {
		s.f.t.Fatalf("job %d: %v", job, err)
	}
	return h
}

func isFakeHost(name string) bool {
	for _, h := range hosts {
		if h.NodeName == name {
			return true
		}
	}
	return false
}

// noHostDialer is the Host dialer of a Runner on the claim backend, which
// has no Inventory and dials no Host.
type noHostDialer struct{}

func (noHostDialer) Dial(_ context.Context, ep flintlock.Endpoint) (flintlock.HostClient, error) {
	return nil, fmt.Errorf("host %s: the claim backend dials no host", ep.Name)
}
