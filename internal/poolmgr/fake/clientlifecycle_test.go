package fake

import (
	"context"
	"errors"
	"runtime"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// TestClientLifecycleAcrossTheFakesLife pins what a Client is worth at each
// point of the fake's life: before Run the RPCs are served and only
// provisioning waits for the control loop; after the shutdown a client
// taken earlier reports the Pool Manager is gone, as a client of a real one
// that went away does, and a client taken afterwards says the fake is shut
// down rather than pointing at a server that is no longer listening.
func TestClientLifecycleAcrossTheFakesLife(t *testing.T) {
	clk := clock.NewFake(testEpoch)
	pm := New(poolmgr.FakeConfig{Clock: clk, ReconcileInterval: testInterval})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	early := pm.Client()
	defer func() { _ = early.Close() }()
	if _, err := early.ListPools(ctx, testNamespace); err != nil {
		t.Fatalf("ListPools before Run: %v", err)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- pm.Run(ctx) }()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("waiting for the control loop's timer: %v", err)
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("Run returned %v", err)
	}

	if _, err := early.ListPools(context.Background(), testNamespace); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Fatalf("ListPools on a client of a stopped fake = %v, want ErrUnavailable", err)
	}
	late := pm.Client()
	defer func() { _ = late.Close() }()
	if _, err := late.ListPools(context.Background(), testNamespace); !errors.Is(err, ErrStopped) {
		t.Fatalf("ListPools on a client taken after the shutdown = %v, want ErrStopped", err)
	}
}

// TestServeRunsOnce checks that a second Serve is refused rather than
// binding a second listener and closing the ready channel again, which
// panicked on the caller's goroutine: in cmd/fake-poolmgr that goroutine is
// the process's own.
func TestServeRunsOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := New(poolmgr.FakeConfig{
		Listen:            "127.0.0.1:0",
		Clock:             clock.NewFake(testEpoch),
		ReconcileInterval: testInterval,
	})
	served := make(chan error, 1)
	go func() { served <- pm.Serve(ctx) }()
	select {
	case <-pm.Ready():
	case err := <-served:
		t.Fatalf("Serve returned before listening: %v", err)
	}

	if err := pm.Serve(ctx); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second Serve = %v, want ErrAlreadyRunning", err)
	}

	cancel()
	if err := <-served; err != nil {
		t.Fatalf("Serve returned %v", err)
	}
}

// TestServeShutdownIsNotReportedAsAnEarlyStop drives the shutdown race in
// Serve's three-way select.
//
// Cancelling the caller's context cancels the control loop's context too,
// so at a normal shutdown ctx.Done() and the loop's result become ready a
// moment apart. If the serving goroutine is descheduled in between, the
// select polls with both ready and picks at random; taking the loop's
// branch turned an ordinary shutdown into "control loop stopped while
// serving". It surfaced about once in a full parallel run of the
// repository's suite and never in isolation, so this drives many shutdowns
// rather than one.
func TestServeShutdownIsNotReportedAsAnEarlyStop(t *testing.T) {
	t.Parallel()

	for i := range 200 {
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		pm := New(poolmgr.FakeConfig{
			Listen:            "127.0.0.1:0",
			Clock:             clock.NewFake(testEpoch),
			ReconcileInterval: testInterval,
		})
		served := make(chan error, 1)
		go func() { served <- pm.Serve(ctx) }()
		select {
		case <-pm.Ready():
		case err := <-served:
			cancel()
			t.Fatalf("iteration %d: Serve returned before listening: %v", i, err)
		}

		// Give the serving goroutine every chance to be somewhere
		// inconvenient when the cancellation lands.
		runtime.Gosched()
		cancel()
		if err := <-served; err != nil {
			t.Fatalf("iteration %d: Serve reported %v; a shutdown the caller"+
				" asked for is not an early stop", i, err)
		}
	}
}
