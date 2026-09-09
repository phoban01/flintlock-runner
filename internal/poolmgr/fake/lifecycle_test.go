package fake

import (
	"context"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// TestFirstClientDuringShutdownIsSafe takes the very first Client of a fake
// while its Run is returning, which is the one ordering the harness never
// produces because it always takes a client first. The loopback the client
// needs is built by New, so the shutdown only reads it; building it here
// instead would be a write racing that read. Run it under -race.
func TestFirstClientDuringShutdownIsSafe(t *testing.T) {
	for range 50 {
		clk := clock.NewFake(testEpoch)
		pm := New(poolmgr.FakeConfig{Clock: clk, ReconcileInterval: testInterval})

		ctx, cancel := context.WithCancel(context.Background())
		runDone := make(chan error, 1)
		go func() { runDone <- pm.Run(ctx) }()
		// The control loop is past its first tick and waiting.
		if err := clk.BlockUntil(context.Background(), 1); err != nil {
			t.Fatalf("waiting for the control loop's timer: %v", err)
		}

		clients := make(chan poolmgr.Client, 1)
		go func() { clients <- pm.Client() }()
		cancel()

		if err := (<-clients).Close(); err != nil {
			t.Fatalf("closing the client taken during the shutdown: %v", err)
		}
		if err := <-runDone; err != nil {
			t.Fatalf("Run returned %v", err)
		}
	}
}
