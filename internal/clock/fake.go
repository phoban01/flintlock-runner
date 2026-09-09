package clock

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Fake is a Clock that only moves when a test tells it to. Now returns the
// time last set; After and NewTimer schedule against that time, and Advance
// or Set fires every timer that falls due, in deadline order, setting Now to
// each deadline as it goes so a callback that reads Now sees a time that is
// consistent with the timer it is handling.
//
// Timers are armed from other goroutines, so a test that wants to fire one
// has to wait until it exists; BlockUntil does that. Fake is safe for
// concurrent use.
type Fake struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers []*fakeTimer
	seq    uint64
}

// NewFake returns a Fake whose Now is start.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start}
	f.cond = sync.NewCond(&f.mu)
	return f
}

// Now implements Clock.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// After implements Clock.
func (f *Fake) After(d time.Duration) <-chan time.Time { return f.NewTimer(d).C() }

// NewTimer implements Clock. A non-positive duration fires at once, as
// time.NewTimer does.
func (f *Fake) NewTimer(d time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{clk: f, ch: make(chan time.Time, 1)}
	f.armLocked(t, d)
	return t
}

// armLocked schedules t for now+d, or fires it immediately when d <= 0.
func (f *Fake) armLocked(t *fakeTimer, d time.Duration) {
	f.seq++
	t.seq = f.seq
	t.when = f.now.Add(d)
	if d <= 0 {
		t.active = false
		t.fire(f.now)
		return
	}
	t.active = true
	f.timers = append(f.timers, t)
	f.cond.Broadcast()
}

// removeLocked takes t out of the pending list. It reports whether t was
// pending.
func (f *Fake) removeLocked(t *fakeTimer) bool {
	if !t.active {
		return false
	}
	t.active = false
	for i, o := range f.timers {
		if o == t {
			f.timers = append(f.timers[:i], f.timers[i+1:]...)
			break
		}
	}
	return true
}

// Advance moves Now forward by d, firing every timer due on the way.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	target := f.now.Add(d)
	f.mu.Unlock()
	f.Set(target)
}

// Set moves Now to t, firing every timer due on the way in deadline order.
// A t earlier than Now only fires nothing and leaves Now unchanged.
func (f *Fake) Set(t time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		next := f.nextDueLocked(t)
		if next == nil {
			break
		}
		if next.when.After(f.now) {
			f.now = next.when
		}
		f.removeLocked(next)
		next.fire(f.now)
	}
	if t.After(f.now) {
		f.now = t
	}
}

// nextDueLocked returns the earliest pending timer with a deadline at or
// before t, ties broken by creation order, or nil.
func (f *Fake) nextDueLocked(t time.Time) *fakeTimer {
	var best *fakeTimer
	for _, tm := range f.timers {
		if tm.when.After(t) {
			continue
		}
		if best == nil || tm.when.Before(best.when) || (tm.when.Equal(best.when) && tm.seq < best.seq) {
			best = tm
		}
	}
	return best
}

// Timers returns how many timers are pending.
func (f *Fake) Timers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

// Deadlines returns the pending deadlines in ascending order, for tests that
// want to check what a component scheduled.
func (f *Fake) Deadlines() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Time, 0, len(f.timers))
	for _, t := range f.timers {
		out = append(out, t.when)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// BlockUntil blocks until at least n timers are pending or ctx is done. It
// is how a test waits for the code under test to arm the timer it is about
// to fire with Advance.
func (f *Fake) BlockUntil(ctx context.Context, n int) error {
	stop := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.cond.Broadcast()
		f.mu.Unlock()
	})
	defer stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.timers) < n {
		if err := ctx.Err(); err != nil {
			return err
		}
		f.cond.Wait()
	}
	return nil
}

// fakeTimer is a Timer owned by a Fake.
type fakeTimer struct {
	clk    *Fake
	ch     chan time.Time
	when   time.Time
	seq    uint64
	active bool
}

// fire delivers now on the channel without blocking; a value already waiting
// is left in place, matching time.Timer's one-slot channel.
func (t *fakeTimer) fire(now time.Time) {
	select {
	case t.ch <- now:
	default:
	}
}

// C implements Timer.
func (t *fakeTimer) C() <-chan time.Time { return t.ch }

// Stop implements Timer.
func (t *fakeTimer) Stop() bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	return t.clk.removeLocked(t)
}

// Reset implements Timer. It drains a value left by an earlier firing so
// the next receive is the new deadline.
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	wasActive := t.clk.removeLocked(t)
	select {
	case <-t.ch:
	default:
	}
	t.clk.armLocked(t, d)
	return wasActive
}

// Compile-time interface check.
var _ Clock = (*Fake)(nil)
