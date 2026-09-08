package executor

import (
	"context"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

type fakeHandle struct {
	alloc scheduler.Allocation
	done  chan struct{}
	err   error
}

func (h *fakeHandle) Allocation() scheduler.Allocation { return h.alloc }
func (h *fakeHandle) Done() <-chan struct{}            { return h.done }
func (h *fakeHandle) Err() error                       { return h.err }

func TestDataWithContextCancelsWhenHandleDone(t *testing.T) {
	t.Parallel()
	h := &fakeHandle{done: make(chan struct{}), alloc: scheduler.Allocation{Profile: "p", VMUID: "vm1", Placement: scheduler.Placement{Host: "h1"}}}
	d := &Data{Handle: h}
	ctx, cancel := d.WithContext(context.Background())
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("context cancelled before handle done")
	default:
	}
	h.err = scheduler.ErrLeaseLost
	close(h.done)
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("context not cancelled after handle done")
	}
	if got := d.LogFields(); got["vm_uid"] != "vm1" || got["host"] != "h1" || got["profile"] != "p" {
		t.Errorf("LogFields = %v", got)
	}
}

func TestDataWithContextWithoutHandle(t *testing.T) {
	t.Parallel()
	d := &Data{}
	ctx, cancel := d.WithContext(context.Background())
	if ctx.Err() != nil {
		t.Fatal("context cancelled with no handle")
	}
	cancel()
	if ctx.Err() == nil {
		t.Fatal("cancel did not cancel")
	}
	if (*Data)(nil).LogFields() != nil {
		t.Error("nil Data should have no log fields")
	}
}
