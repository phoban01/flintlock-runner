package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// hostWatch bounds how long an in-flight operation can outlive the Host
// that is running it. A Transport starts one for the life of an operation
// and stops it on the way out.
type hostWatch struct {
	activity chan struct{}
	done     chan struct{}
	finished chan struct{}

	// probeCancel ends an in-flight liveness probe. stop calls it so that
	// an operation which has just finished is not held for the rest of a
	// probe's budget on a Host that is slow but alive.
	probeCancel context.CancelFunc

	stopOnce sync.Once

	mu      sync.Mutex
	failure error
}

//= docs/requirements/02-executor.md#guest-transport
//# If the Host that runs the MicroVM becomes unreachable, then the
//# Guest Transport SHALL fail the in-flight operation within the configured
//# transport deadline.

// watchHost watches the Host for as long as an operation is in flight and
// cancels it when the Host stops answering. Silence on the operation's own
// stream is not evidence of that -- a Stage compiling for ten minutes says
// nothing at all, and killing it would be worse than useless -- so when
// half the deadline passes with nothing received, the watch asks the Host
// about the MicroVM instead, with the other half as its deadline. A Host
// that answers proves the operation is merely quiet and the watch arms
// again; a Host that does not answer within that call, or that no longer
// has the MicroVM, ends the operation by cancelling its context, so the
// failure lands within the configured deadline rather than whenever the
// Job's own timeout happens to fire.
//
// A deadline of zero disables the watch, which is what an operation that
// should only end with its context wants.
func watchHost(ctx context.Context, cancel context.CancelFunc, target Target, clk clock.Clock) *hostWatch {
	w := &hostWatch{
		activity: make(chan struct{}, 1),
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
	if target.Deadline <= 0 || target.Host == nil {
		w.probeCancel = func() {}
		close(w.finished)
		return w
	}
	if clk == nil {
		clk = clock.Real{}
	}

	// Probes run under a context of the watch's own, so that stopping the
	// watch ends the call that is in flight rather than waiting it out.
	probeCtx, probeCancel := context.WithCancel(ctx)
	w.probeCancel = probeCancel

	interval := target.Deadline / 2
	if interval <= 0 {
		interval = target.Deadline
	}
	probeTimeout := target.Deadline - interval
	if probeTimeout <= 0 {
		probeTimeout = interval
	}

	go func() {
		defer close(w.finished)
		defer probeCancel()
		timer := clk.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.done:
				return
			case <-w.activity:
				resetTimer(timer, interval)
			case <-timer.C():
				if err := probeHost(probeCtx, target, probeTimeout); err != nil {
					if stopped(w.done) || ctx.Err() != nil {
						// The operation ended under us, or finished while
						// the probe was in flight and stopped the watch;
						// either way the probe's error is not a failure of
						// the operation.
						return
					}
					w.fail(err)
					cancel()
					return
				}
				timer.Reset(interval)
			}
		}
	}()
	return w
}

// probeHost asks the Host about the MicroVM the operation is running in,
// bounded by timeout. GetMicroVM is the cheapest call that proves both that
// the Host is answering and that it still holds the MicroVM.
func probeHost(ctx context.Context, target Target, timeout time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if _, err := target.Host.GetMicroVM(probeCtx, target.VMUID); err != nil {
		return fmt.Errorf("host %s did not answer for microvm %s within %s: %w",
			target.Host.Name(), target.VMUID, timeout, err)
	}
	return nil
}

// sawResponse tells the watch that the Host has just been heard from. It
// never blocks: one pending notification is as good as ten.
func (w *hostWatch) sawResponse() {
	select {
	case w.activity <- struct{}{}:
	default:
	}
}

// err returns the failure that made the watch cancel the operation, or nil
// when the operation ended for another reason.
func (w *hostWatch) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failure
}

// fail records why the watch is cancelling.
func (w *hostWatch) fail(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failure = errors.Join(ErrStreamFailed, err)
}

// stop ends the watch and waits for its goroutine, so that an operation
// leaves nothing running behind it. A probe that is in flight is cancelled
// rather than waited for: the operation is over, and holding its caller for
// the rest of a probe's budget would add that much latency to every Stage
// that finishes while the watch happens to be asking after the Host.
func (w *hostWatch) stop() {
	w.stopOnce.Do(func() {
		close(w.done)
		w.probeCancel()
	})
	<-w.finished
}

// stopped reports whether the watch has been told to stop.
func stopped(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// resetTimer re-arms a timer that may already have fired, draining the
// value it left behind so that the next wait is a real wait.
func resetTimer(timer clock.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
	timer.Reset(d)
}
