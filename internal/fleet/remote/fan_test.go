package remote

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

func instances(n int) []fleet.Instance {
	out := make([]fleet.Instance, n)
	for i := range out {
		out[i] = fleet.Instance{ID: fmt.Sprintf("i-%d", i)}
	}
	return out
}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# The Fleet Controller SHALL provision instances in parallel with
//# the configured parallelism limit.

// TestFanRunsUpToTheParallelismLimit holds every started instance until the
// test releases it and, inside a synctest bubble, checks that exactly the
// limit is running whenever the goroutines are blocked: not fewer (they run
// in parallel) and not more (the limit holds).
func TestFanRunsUpToTheParallelismLimit(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{1, 3} {
		synctest.Test(t, func(t *testing.T) {
			const n = 7
			var mu sync.Mutex
			running := 0
			release := make(chan struct{})
			done := make(chan []string, 1)
			go func() {
				res, err := Fan(context.Background(), instances(n), limit, func(_ context.Context, inst fleet.Instance) (string, error) {
					mu.Lock()
					running++
					mu.Unlock()
					<-release
					mu.Lock()
					running--
					mu.Unlock()
					return inst.ID, nil
				})
				if err != nil {
					t.Errorf("Fan: %v", err)
				}
				done <- res
			}()
			for finished := 0; finished < n; finished++ {
				// Every goroutine is blocked: the started ones on release,
				// Fan on its limit.
				synctest.Wait()
				mu.Lock()
				got, want := running, min(limit, n-finished)
				mu.Unlock()
				if got != want {
					t.Fatalf("limit %d: %d running with %d finished, want %d", limit, got, finished, want)
				}
				release <- struct{}{}
			}
			res := <-done
			want := make([]string, n)
			for i := range want {
				want[i] = fmt.Sprintf("i-%d", i)
			}
			if !reflect.DeepEqual(res, want) {
				t.Errorf("results %v, want instance order %v", res, want)
			}
		})
	}
}

//= docs/requirements/06-fleet.md#remote-execution
//= type=test
//# If provisioning fails on one instance, then the Fleet Controller
//# SHALL continue provisioning the others and SHALL exit with a non-zero
//# status that summarises every failure.

// TestFanContinuesPastFailuresAndSummarisesEach fails two of five instances,
// with parallelism one so the first failure comes before the rest start, and
// checks every instance ran and the error names each failure.
func TestFanContinuesPastFailuresAndSummarisesEach(t *testing.T) {
	t.Parallel()
	errDevice := errors.New("thin pool device /dev/nvme1n1 not found")
	errTimeout := errors.New("flintlockd not active")
	failing := map[string]error{"i-1": errDevice, "i-3": errTimeout}
	var mu sync.Mutex
	var ran []string
	res, err := Fan(context.Background(), instances(5), 1, func(_ context.Context, inst fleet.Instance) (fleet.HostResult, error) {
		mu.Lock()
		ran = append(ran, inst.ID)
		mu.Unlock()
		r := fleet.HostResult{Instance: inst, Err: failing[inst.ID]}
		return r, r.Err
	})
	if want := []string{"i-0", "i-1", "i-2", "i-3", "i-4"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want every instance %v", ran, want)
	}
	if len(res) != 5 || res[1].Err != errDevice || res[4].Instance.ID != "i-4" {
		t.Errorf("results %+v", res)
	}
	var failures Failures
	if !errors.As(err, &failures) || len(failures) != 2 {
		t.Fatalf("err = %v, want Failures for two instances", err)
	}
	if !errors.Is(err, errDevice) || !errors.Is(err, errTimeout) {
		t.Errorf("err %v does not wrap each failure", err)
	}
	msg := err.Error()
	for _, want := range []string{"2 instance(s) failed", "i-1: " + errDevice.Error(), "i-3: " + errTimeout.Error()} {
		if !strings.Contains(msg, want) {
			t.Errorf("summary %q lacks %q", msg, want)
		}
	}

	if _, err := Fan(context.Background(), instances(3), 2, func(context.Context, fleet.Instance) (int, error) { return 1, nil }); err != nil {
		t.Errorf("all succeeded: err = %v (%T), want nil", err, err)
	}
}

func TestFanStopsStartingWhenContextEnds(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var started []string
	_, err := Fan(ctx, instances(3), 1, func(_ context.Context, inst fleet.Instance) (int, error) {
		started = append(started, inst.ID)
		cancel()
		return 0, nil
	})
	if !reflect.DeepEqual(started, []string{"i-0"}) {
		t.Errorf("started %v after cancel", started)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the unstarted instances to fail with context.Canceled", err)
	}
}
