package fake

import (
	"context"
	"errors"
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
