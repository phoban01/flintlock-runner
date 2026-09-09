package fake

import (
	"sync"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
)

// fakeClock is a manually advanced clock.Clock. Timers fire when Advance
// moves Now past their deadline, so tests drive boot delays and exec
// timeouts without sleeping.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)}
}

// Now implements clock.Clock.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After implements clock.Clock.
func (c *fakeClock) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C() }

// NewTimer implements clock.Clock.
func (c *fakeClock) NewTimer(d time.Duration) clock.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{c: c, ch: make(chan time.Time, 1)}
	t.armLocked(d)
	return t
}

// Advance moves the clock forward and fires every timer due by then.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
}

// fireLocked delivers every due timer.
func (c *fakeClock) fireLocked() {
	remaining := c.timers[:0]
	for _, t := range c.timers {
		if !t.when.After(c.now) {
			t.active = false
			select {
			case t.ch <- c.now:
			default:
			}
			continue
		}
		remaining = append(remaining, t)
	}
	c.timers = remaining
}

// fakeTimer is a clock.Timer owned by a fakeClock.
type fakeTimer struct {
	c      *fakeClock
	ch     chan time.Time
	when   time.Time
	active bool
}

// armLocked schedules the timer; a non-positive duration fires at once.
func (t *fakeTimer) armLocked(d time.Duration) {
	t.when = t.c.now.Add(d)
	t.active = true
	t.c.timers = append(t.c.timers, t)
	t.c.fireLocked()
}

// C implements clock.Timer.
func (t *fakeTimer) C() <-chan time.Time { return t.ch }

// Stop implements clock.Timer.
func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.active = false
	t.c.removeLocked(t)
	return was
}

// Reset implements clock.Timer.
func (t *fakeTimer) Reset(d time.Duration) bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	was := t.active
	t.c.removeLocked(t)
	t.armLocked(d)
	return was
}

func (c *fakeClock) removeLocked(t *fakeTimer) {
	for i, x := range c.timers {
		if x == t {
			c.timers = append(c.timers[:i], c.timers[i+1:]...)
			return
		}
	}
}

var _ clock.Clock = (*fakeClock)(nil)
