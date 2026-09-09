package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// TestDialerHidesAdminMethods proves the HO-007 split survives the fake: a
// Runner-side Dial cannot be asserted up to create and delete, while
// DialAdmin can.
func TestDialerHidesAdminMethods(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	d := NewDialer(h)
	ctx := testCtx(t)

	c, err := d.Dial(ctx, flintlock.Endpoint{Name: "h1"})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if c.Name() != "h1" {
		t.Errorf("Name = %q, want h1", c.Name())
	}
	if _, ok := c.(flintlock.HostAdminClient); ok {
		t.Error("Dial returned a client with admin methods; HO-007 needs them hidden")
	}
	if _, err := c.ServerInfo(ctx); err != nil {
		t.Errorf("ServerInfo through Dial: %v", err)
	}

	admin, err := d.DialAdmin(ctx, flintlock.Endpoint{Name: "h1"})
	if err != nil {
		t.Fatalf("DialAdmin: %v", err)
	}
	if _, err := admin.CreateMicroVM(ctx, nil); err == nil {
		t.Error("CreateMicroVM(nil spec) succeeded, want an error")
	}

	if _, err := d.Dial(ctx, flintlock.Endpoint{Name: "nope"}); !errors.Is(err, flintlock.ErrUnknownHost) {
		t.Errorf("Dial(unknown) error = %v, want ErrUnknownHost", err)
	}

	h2 := newTestHost(t, flintlock.FakeHostConfig{Name: "h2"})
	d.Add(h2)
	if _, err := d.Dial(ctx, flintlock.Endpoint{Name: "h2"}); err != nil {
		t.Errorf("Dial after Add: %v", err)
	}
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=test
//# The fake Host SHALL enforce basic auth and TLS when configured so that
//# the Runner's authentication code is exercised.

// TestDialerChecksToken: the in-memory client enforces the Host's token so
// that an Inventory with the wrong token fails in unit tests exactly as it
// would over gRPC.
func TestDialerChecksToken(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{Name: "h1", Token: "s3cret"})
	d := NewDialer(h)
	ctx := testCtx(t)

	tests := []struct {
		name    string
		token   string
		wantErr error
	}{
		{name: "right token", token: "s3cret"},
		{name: "wrong token", token: "nope", wantErr: flintlock.ErrUnauthenticated},
		{name: "no token", token: "", wantErr: flintlock.ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, err := d.Dial(ctx, flintlock.Endpoint{Name: "h1", Token: tt.token})
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			_, err = c.ServerInfo(ctx)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("ServerInfo error = %v, want %v", err, tt.wantErr)
			}
			_, err = c.GetMicroVM(ctx, "x")
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("GetMicroVM error = %v, want %v", err, tt.wantErr)
			}
		})
	}

	// Host.Client carries the Host's own token and is always authorised.
	if _, err := h.Client().ServerInfo(ctx); err != nil {
		t.Errorf("Host.Client().ServerInfo: %v", err)
	}
}

// TestClientCloseFailsStreams: HostClient.Close fails in-flight streams and
// later calls.
func TestClientCloseFailsStreams(t *testing.T) {
	t.Parallel()
	h := newTestHost(t, flintlock.FakeHostConfig{})
	vm := createVM(t, h, nil)
	c := h.Client()

	stream, err := c.Exec(testCtx(t))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	sendStart(stream, shell(vm.GetSpec().GetUid(), "echo started; sleep 60"))
	res := &execResult{}
	waitForStdout(t, stream, res, "started")

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drain(stream, res)
	if res.err == nil {
		t.Error("stream ended cleanly after Close, want an error")
	}
	if res.exit != nil {
		t.Errorf("exit code %d received after Close, want none", *res.exit)
	}
	if _, err := c.ServerInfo(context.Background()); !errors.Is(err, flintlock.ErrUnavailable) {
		t.Errorf("ServerInfo after Close = %v, want ErrUnavailable", err)
	}
}
