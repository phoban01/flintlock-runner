package verify

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/opstest"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// failingLaterListings answers the first ListPools from the real Pool
// Manager and fails every later one.
type failingLaterListings struct {
	PoolManager
	calls atomic.Int32
}

func (f *failingLaterListings) ListPools(ctx context.Context, ns string) ([]*poolmgr.Pool, error) {
	if f.calls.Add(1) == 1 {
		return f.PoolManager.ListPools(ctx, ns)
	}
	return nil, fmt.Errorf("list pools: %w", poolmgr.ErrUnavailable)
}

// TestAFailedLaterListingKeepsThePreviousAnswer pins what verification
// reports when the Pool Manager answers once and then stops answering while
// the Pool is still filling. The first answer, a Pool below its target size,
// has to stand. The failure used to replace it whenever the context had not
// yet reported its own expiry, which happens when gRPC's deadline timer fires
// a moment before the context's: TestVerifyReportsPoolBelowTargetSize then
// intermittently read "ListPools: context deadline exceeded" instead of the
// Pool's size.
func TestAFailedLaterListingKeepsThePreviousAnswer(t *testing.T) {
	t.Parallel()
	s := opstest.Start(t, opstest.Options{Hosts: 1, PoolSize: 1})
	s.Declare(t)
	s.WaitAvailable(t, 1)
	s.Profiles[0].Pool.Size = 5

	pm := &failingLaterListings{PoolManager: s.Client}
	v, err := New(Config{
		PoolManager:       pm,
		Hosts:             opstest.Dialer(),
		Transports:        transport.NewFactory(),
		Profiles:          s.Profiles,
		Timeout:           500 * time.Millisecond,
		TransportDeadline: 5 * time.Second,
		PollInterval:      20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := v.Verify(context.Background(), &fleet.Inventory{Hosts: s.Inventory})
	if err != nil {
		t.Fatal(err)
	}
	if pm.calls.Load() < 2 {
		t.Fatalf("the Pool Manager was listed %d times; the test needs a later listing to fail", pm.calls.Load())
	}
	f := failuresFor(report, s.Ref().String())
	if len(f) != 1 || f[0].Step != StepPool || !strings.Contains(f[0].Err.Error(), "of 5") {
		t.Fatalf("failures = %v, want the Pool below its target size from the first answer", report.Failures)
	}
	noLeasesLeft(t, s)
}
