package kube_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

// tracker runs the real PoolTracker over a backend, which is how the
// Scheduler reads a Pool's available count whatever the backend is.
func (f *fixture) tracker(b *kube.Backend, ref poolmgr.PoolRef) *poolmgr.PoolTracker {
	f.t.Helper()
	tracker, err := poolmgr.NewTracker(poolmgr.TrackerConfig{Events: b, Admin: b, PollInterval: time.Hour})
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(f.ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); _ = tracker.Run(ctx) }()
	f.t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	tracker.Track(ref, true)
	return tracker
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Scheduler SHALL derive each Pool's available count from a
//# watch on the Pool's ready idle pods and SHALL wake Jobs waiting on an
//# exhausted Pool when that count rises.

// TestWatchFeedsTheAvailableCountAndWakesWaiters runs the Tracker the
// Scheduler uses on top of the backend's event stream, with its periodic
// GetPool poll pushed out of reach, so that a count that rises has risen
// because of the watch. The count
// follows the pods: up as they become ready, down on a claim. A Job waiting
// on the exhausted Pool holds the Tracker's wait channel, exactly as the
// Scheduler's allocation loop does, and is woken when the replacement pod
// becomes ready.
func TestWatchFeedsTheAvailableCountAndWakesWaiters(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.profile.Pool.Size = 1
	b := f.backend()
	ref := f.declare(b)
	tracker := f.tracker(b, ref)
	f.eventually("the tracker counts the ready idle pod", func() bool { return tracker.Available(ref) == 1 })

	// The replacement is held back, so the claim empties the Pool.
	f.kubelet.Pause()
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	f.eventually("the claim takes the count to zero", func() bool { return tracker.Available(ref) == 0 })

	wake := tracker.Wait(ref)
	if _, err := b.ClaimVM(f.ctx, ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("second claim = %v, want ErrExhausted", err)
	}
	tracker.MarkExhausted(ref)
	select {
	case <-wake:
		t.Fatal("the waiting job was woken with no pod available")
	case <-time.After(consistentFor):
	}

	f.kubelet.Resume()
	select {
	case <-wake:
	case <-time.After(waitFor):
		t.Fatalf("the waiting job was not woken by the replacement becoming ready\nlogs:\n%s", f.logs.String())
	}
	if got := tracker.Available(ref); got != 1 {
		t.Errorf("available = %d after the wake-up, want 1", got)
	}
	second, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatalf("claim after the wake-up: %v", err)
	}
	if second.LeaseID == claim.LeaseID {
		t.Errorf("the same pod %s was claimed twice", claim.LeaseID)
	}

	// A released Lease is a deleted pod; it never becomes available again.
	if err := b.ReleaseVM(f.ctx, claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	f.eventually("the pool is back to one available", func() bool { return tracker.Available(ref) == 1 })
}

// TestSubscribeReplaysAvailablePods checks that a stream opened after the
// pods became ready still learns of them, and that a closed backend ends its
// streams with ErrUnavailable, as a dropped battery stream ends.
func TestSubscribeReplaysAvailablePods(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	var stream poolmgr.EventStream
	seen := map[string]bool{}
	f.eventually("both ready pods are replayed as available", func() bool {
		// The backend's watch may trail GetPool, so subscribe until the
		// replay carries both pods.
		if stream != nil {
			_ = stream.Close()
		}
		var err error
		stream, err = b.Subscribe(f.ctx, poolmgr.EventFilter{Pool: &ref})
		if err != nil {
			t.Fatal(err)
		}
		clear(seen)
		for range 2 {
			ctx, cancel := context.WithTimeout(f.ctx, 10*tick)
			event, err := stream.Recv(ctx)
			cancel()
			if err != nil {
				return false
			}
			if event.Pool != ref || event.Type.String() != "VM_AVAILABLE" {
				t.Fatalf("replayed %+v, want VM_AVAILABLE for %s", event, ref)
			}
			seen[event.VMUID] = true
		}
		return len(seen) == 2
	})

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := stream.Recv(f.ctx)
		if errors.Is(err, poolmgr.ErrUnavailable) {
			break
		}
		if err != nil {
			t.Fatalf("Recv after Close = %v, want ErrUnavailable", err)
		}
	}
	if _, err := b.Subscribe(f.ctx, poolmgr.EventFilter{}); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("Subscribe after Close = %v, want ErrUnavailable", err)
	}
}
