package clock

import (
	"context"
	"testing"
	"time"
)

var epoch = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)

func TestFakeAdvanceFiresTimersInOrder(t *testing.T) {
	t.Parallel()
	f := NewFake(epoch)
	late := f.NewTimer(3 * time.Second)
	early := f.NewTimer(time.Second)
	never := f.NewTimer(time.Hour)
	if f.Timers() != 3 {
		t.Fatalf("Timers = %d, want 3", f.Timers())
	}

	f.Advance(5 * time.Second)

	if got := <-early.C(); !got.Equal(epoch.Add(time.Second)) {
		t.Errorf("early fired at %v, want %v", got, epoch.Add(time.Second))
	}
	if got := <-late.C(); !got.Equal(epoch.Add(3 * time.Second)) {
		t.Errorf("late fired at %v, want %v", got, epoch.Add(3*time.Second))
	}
	select {
	case <-never.C():
		t.Error("hour timer fired after 5s")
	default:
	}
	if !f.Now().Equal(epoch.Add(5 * time.Second)) {
		t.Errorf("Now = %v, want %v", f.Now(), epoch.Add(5*time.Second))
	}
	if f.Timers() != 1 {
		t.Errorf("Timers = %d, want 1", f.Timers())
	}
}

func TestFakeStopAndReset(t *testing.T) {
	t.Parallel()
	f := NewFake(epoch)
	tm := f.NewTimer(time.Second)
	if !tm.Stop() {
		t.Error("Stop on a pending timer reported inactive")
	}
	if tm.Stop() {
		t.Error("second Stop reported active")
	}
	f.Advance(2 * time.Second)
	select {
	case <-tm.C():
		t.Error("stopped timer fired")
	default:
	}
	if tm.Reset(time.Second) {
		t.Error("Reset on a stopped timer reported active")
	}
	f.Advance(time.Second)
	select {
	case got := <-tm.C():
		if !got.Equal(epoch.Add(3 * time.Second)) {
			t.Errorf("fired at %v", got)
		}
	default:
		t.Error("reset timer did not fire")
	}
	if tm.Reset(time.Second) {
		t.Error("Reset on a fired timer reported active")
	}
}

func TestFakeZeroDurationFiresImmediately(t *testing.T) {
	t.Parallel()
	f := NewFake(epoch)
	select {
	case <-f.After(0):
	default:
		t.Error("After(0) did not fire immediately")
	}
	if f.Timers() != 0 {
		t.Errorf("Timers = %d, want 0", f.Timers())
	}
}

func TestFakeBlockUntil(t *testing.T) {
	t.Parallel()
	f := NewFake(epoch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { f.NewTimer(time.Minute) }()
	if err := f.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("BlockUntil: %v", err)
	}
	if got := f.Deadlines(); len(got) != 1 || !got[0].Equal(epoch.Add(time.Minute)) {
		t.Errorf("Deadlines = %v", got)
	}
	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	if err := f.BlockUntil(short, 2); err == nil {
		t.Error("BlockUntil(2) returned without a second timer")
	}
}

func TestFakeSetBackwardsIsNoop(t *testing.T) {
	t.Parallel()
	f := NewFake(epoch)
	f.Set(epoch.Add(-time.Hour))
	if !f.Now().Equal(epoch) {
		t.Errorf("Now moved backwards to %v", f.Now())
	}
}
