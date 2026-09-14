package fake

import (
	"context"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// TestStopDeletesAMicroVMWhoseCreateWasInFlight stops the fake while a
// replenishing CreateMicroVM is still on the Host, which is what the
// harness's shutdown does straight after a claim (TD-054). The Host makes
// the MicroVM whether or not its caller is still waiting, as flintlockd
// does. When stop cancelled the create, the fake got an error and no uid,
// forgot the reservation and left the MicroVM on the Host for ever; the
// harness saw it as a leftover sandbox about one run in eight.
func TestStopDeletesAMicroVMWhoseCreateWasInFlight(t *testing.T) {
	h := newHarness(t, poolmgr.FakeConfig{}, "host-1")
	host := h.stubs["host-1"]
	gate := make(chan struct{})
	host.set(func(s *stubHost) { s.createGate = gate })

	if _, err := h.client.CreatePool(h.ctx, h.spec("p", 1, "host-1")); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	select {
	case <-host.createEntered:
	case <-h.ctx.Done():
		t.Fatal("the fake never started a create")
	}

	// The fake's context is cancelled before the Host answers, so a create
	// that shares it has been given up on by the time the gate opens.
	h.cancel()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.stop()
	}()
	close(gate)
	<-stopped

	if created, _ := host.counts(); created != 1 {
		t.Fatalf("microvms created = %d, want 1", created)
	}
	if n := host.live(); n != 0 {
		t.Fatalf("%d microvm(s) left on the host after the fake stopped, want none", n)
	}
}

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
