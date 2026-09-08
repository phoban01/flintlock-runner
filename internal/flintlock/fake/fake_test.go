package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

func TestSkeleton(t *testing.T) {
	t.Parallel()
	h := New(flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	h.SetFaults(flintlock.HostFaults{Unresponsive: true})
	if !h.Faults().Unresponsive {
		t.Error("faults not stored")
	}
	d := NewDialer(h)
	c, err := d.Dial(context.Background(), flintlock.Endpoint{Name: "h1"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c.Name() != "h1" {
		t.Errorf("Name = %q", c.Name())
	}
	if _, err := c.ServerInfo(context.Background()); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ServerInfo error = %v, want ErrUnsupported", err)
	}
	if _, err := d.Dial(context.Background(), flintlock.Endpoint{Name: "nope"}); !errors.Is(err, flintlock.ErrUnknownHost) {
		t.Errorf("unknown host error = %v", err)
	}
}
