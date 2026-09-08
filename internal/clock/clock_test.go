package clock

import (
	"testing"
	"time"
)

func TestExponentialNext(t *testing.T) {
	t.Parallel()
	b := Exponential{Base: time.Second, Max: 10 * time.Second}
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{-1, time.Second},
		{0, time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 10 * time.Second},
		{60, 10 * time.Second},
	}
	for _, tt := range tests {
		if got := b.Next(tt.attempt); got != tt.want {
			t.Errorf("Next(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

func TestExponentialUncapped(t *testing.T) {
	t.Parallel()
	b := Exponential{Base: time.Millisecond}
	if got := b.Next(3); got != 8*time.Millisecond {
		t.Errorf("Next(3) = %v, want 8ms", got)
	}
}

func TestZeroNeverWaits(t *testing.T) {
	t.Parallel()
	if got := (Zero{}).Next(5); got != 0 {
		t.Errorf("Zero.Next = %v, want 0", got)
	}
}

func TestRealTimerFires(t *testing.T) {
	t.Parallel()
	timer := Real{}.NewTimer(time.Millisecond)
	select {
	case <-timer.C():
	case <-time.After(5 * time.Second):
		t.Fatal("timer did not fire")
	}
	if timer.Stop() {
		t.Error("Stop on a fired timer reported active")
	}
}
