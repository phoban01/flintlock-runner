package poolmgr_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"

	"github.com/phoban01/flintlock-runner/internal/config"
	flintlockfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# The Scheduler SHALL communicate with the Pool Manager through the
//# `poolmgr.v1alpha1` gRPC services using the generated Go client from the
//# battery module.

// TestClientSpeaksEveryService drives every RPC of the three services
// against the fake Pool Manager over a real gRPC connection, and checks
// each one against what the fake actually did rather than against the
// answer it gave back.
func TestClientSpeaksEveryService(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 1), "host-a")

	// PoolAdmin: create, get, list, update.
	pm.fillPool(c, spec)
	got, err := c.GetPool(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if got.Spec.Ref != spec.Ref || got.Status.Available != 1 {
		t.Errorf("GetPool = %+v, want %s with one available", got, spec.Ref)
	}
	pools, err := c.ListPools(ctx, testNamespace)
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 1 || pools[0].Spec.Ref != spec.Ref {
		t.Errorf("ListPools = %+v, want just %s", pools, spec.Ref)
	}
	grown := spec
	grown.Size = 2
	if _, err := c.UpdatePool(ctx, grown); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	if got, err = c.GetPool(ctx, spec.Ref); err != nil || got.Spec.Size != 2 {
		t.Errorf("after UpdatePool GetPool = %+v, %v, want size 2", got, err)
	}

	// Lease: claim, heartbeat, release.
	claim, err := c.ClaimVM(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	if len(pm.leases()) != 1 || pm.leases()[0].LeaseID != claim.LeaseID {
		t.Errorf("fake holds leases %+v, want the one just claimed (%s)", pm.leases(), claim.LeaseID)
	}
	expires, err := c.Heartbeat(ctx, claim.LeaseID)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if expires.IsZero() {
		t.Error("Heartbeat returned a zero expiry")
	}
	if err := c.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}

	// Events: the claim and the release are on the stream.
	stream := pm.events(c)
	claimed := awaitEvent(t, ctx, stream, poolmgrv1.EventType_VM_CLAIMED)
	if claimed.Pool != spec.Ref || claimed.VMUID != claim.VMUID {
		t.Errorf("VM_CLAIMED = %+v, want pool %s and uid %s", claimed, spec.Ref, claim.VMUID)
	}
	if released := awaitEvent(t, ctx, stream, poolmgrv1.EventType_VM_RELEASED); released.VMUID != claim.VMUID {
		t.Errorf("VM_RELEASED uid = %q, want %q", released.VMUID, claim.VMUID)
	}
}

//= docs/requirements/04-pool-manager.md#claiming
//= type=test
//# When `ClaimVM` succeeds, the Scheduler SHALL record the returned
//# `lease_id`, `vm_uid`, `network_interfaces` and the `host` name and
//# address on the Allocation.

// TestClaimCarriesEveryFieldOfTheResponse checks the four things an
// Allocation is built from against the fake Pool Manager's own records: the
// lease it granted, the MicroVM it handed over, that MicroVM's network
// interfaces and the Host it was placed on.
func TestClaimCarriesEveryFieldOfTheResponse(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	pm.decorateHosts()
	c := pm.client()
	spec := specFor(t, testProfile("small", 1), "host-a")
	pm.fillPool(c, spec)

	claim, err := c.ClaimVM(ctx, spec.Ref)
	if err != nil {
		t.Fatalf("ClaimVM: %v", err)
	}
	leases := pm.leases()
	if len(leases) != 1 {
		t.Fatalf("fake holds %d leases, want one", len(leases))
	}
	if claim.LeaseID != leases[0].LeaseID {
		t.Errorf("LeaseID = %q, want %q", claim.LeaseID, leases[0].LeaseID)
	}
	if claim.VMUID != leases[0].VMUID {
		t.Errorf("VMUID = %q, want %q", claim.VMUID, leases[0].VMUID)
	}
	var placed poolmgr.VMRecord
	for _, vm := range pm.vms() {
		if vm.UID == claim.VMUID {
			placed = vm
		}
	}
	if claim.Host.Name != placed.Host {
		t.Errorf("Host.Name = %q, want %q", claim.Host.Name, placed.Host)
	}
	if want := placed.Host + ":9090"; claim.Host.Address != want {
		t.Errorf("Host.Address = %q, want %q", claim.Host.Address, want)
	}
	iface, ok := claim.NetworkInterfaces[poolmgr.GuestDeviceID]
	if !ok {
		t.Fatalf("NetworkInterfaces = %v, want the %s interface of the leased MicroVM",
			claim.NetworkInterfaces, poolmgr.GuestDeviceID)
	}
	if want := "tap-" + claim.VMUID; iface.GetHostDeviceName() != want {
		t.Errorf("interface host device = %q, want %q", iface.GetHostDeviceName(), want)
	}
}

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# The Scheduler SHALL apply the configured deadline to every unary call to
//# the Pool Manager.

// TestEveryUnaryCallCarriesTheConfiguredDeadline drives every unary RPC
// through a recording proxy in front of the fake Pool Manager and checks
// the deadline each one arrived with. A deadline set by the client crosses
// the wire as the grpc-timeout header, so the server seeing one at all is
// proof the client set it; its value is checked as well, because a client
// that set the wrong one would still pass the first test.
func TestEveryUnaryCallCarriesTheConfiguredDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	px := startProxy(t, pm.addr, "", "")

	const deadline = 4 * time.Second
	c, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint: px.addr,
		TLS:      config.ClientTLS{Insecure: true},
		Deadline: deadline,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = c.Close() }()

	spec := specFor(t, testProfile("small", 0), "host-a")
	if _, err := c.CreatePool(ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	if _, err := c.UpdatePool(ctx, spec); err != nil {
		t.Fatalf("UpdatePool: %v", err)
	}
	if _, err := c.GetPool(ctx, spec.Ref); err != nil {
		t.Fatalf("GetPool: %v", err)
	}
	if _, err := c.ListPools(ctx, testNamespace); err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	// The Pool is empty, so this claim is refused; the call still
	// reaches the server, which is what is being recorded.
	if _, err := c.ClaimVM(ctx, spec.Ref); !errors.Is(err, poolmgr.ErrExhausted) {
		t.Fatalf("ClaimVM = %v, want ErrExhausted", err)
	}
	if _, err := c.Heartbeat(ctx, "no-such-lease"); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("Heartbeat = %v, want ErrNotFound", err)
	}
	if err := c.ReleaseVM(ctx, "no-such-lease"); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("ReleaseVM = %v, want ErrNotFound", err)
	}
	if err := c.DeletePool(ctx, spec.Ref); err != nil {
		t.Fatalf("DeletePool: %v", err)
	}

	seen, without := px.seen()
	if len(without) > 0 {
		t.Errorf("calls arrived with no deadline: %v", without)
	}
	want := []string{
		"/poolmgr.v1alpha1.PoolAdmin/CreatePool",
		"/poolmgr.v1alpha1.PoolAdmin/UpdatePool",
		"/poolmgr.v1alpha1.PoolAdmin/GetPool",
		"/poolmgr.v1alpha1.PoolAdmin/ListPools",
		"/poolmgr.v1alpha1.PoolAdmin/DeletePool",
		"/poolmgr.v1alpha1.Lease/ClaimVM",
		"/poolmgr.v1alpha1.Lease/Heartbeat",
		"/poolmgr.v1alpha1.Lease/ReleaseVM",
	}
	for _, method := range want {
		remaining, ok := seen[method]
		if !ok {
			t.Errorf("%s did not reach the server", method)
			continue
		}
		for _, d := range remaining {
			if d <= 0 || d > deadline {
				t.Errorf("%s arrived with %s left of a %s deadline", method, d, deadline)
			}
		}
	}
}

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# The Scheduler SHALL connect to the Pool Manager with TLS unless the
//# configuration explicitly marks the endpoint insecure.

// TestTLSUnlessExplicitlyInsecure checks the three cases that matter: a TLS
// endpoint reached with the configured certificate authority works, the
// same endpoint reached in plaintext does not, and a plaintext endpoint is
// only reached when the configuration says the endpoint is insecure. The
// server is the recording proxy, serving the fake Pool Manager over TLS.
func TestTLSUnlessExplicitlyInsecure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	certs, err := flintlockfake.WriteTestCerts(t.TempDir())
	if err != nil {
		t.Fatalf("WriteTestCerts: %v", err)
	}
	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	secure := startProxy(t, pm.addr, certs.ServerCertFile, certs.ServerKeyFile)
	plaintext := startProxy(t, pm.addr, "", "")

	tests := []struct {
		name     string
		endpoint string
		tls      config.ClientTLS
		wantErr  bool
	}{
		{
			name:     "tls endpoint with the configured ca",
			endpoint: secure.addr,
			tls:      config.ClientTLS{CAFile: certs.CAFile},
		},
		{
			name:     "tls endpoint dialled in plaintext",
			endpoint: secure.addr,
			tls:      config.ClientTLS{Insecure: true},
			wantErr:  true,
		},
		{
			name:     "plaintext endpoint dialled with tls",
			endpoint: plaintext.addr,
			tls:      config.ClientTLS{CAFile: certs.CAFile},
			wantErr:  true,
		},
		{
			name:     "plaintext endpoint explicitly marked insecure",
			endpoint: plaintext.addr,
			tls:      config.ClientTLS{Insecure: true},
		},
		{
			name:     "tls endpoint whose certificate is not trusted",
			endpoint: secure.addr,
			tls:      config.ClientTLS{},
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, err := poolmgr.NewClient(poolmgr.ClientConfig{
				Endpoint: tc.endpoint,
				TLS:      tc.tls,
				Deadline: 2 * time.Second,
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			defer func() { _ = c.Close() }()

			_, err = c.ListPools(ctx, testNamespace)
			switch {
			case tc.wantErr && err == nil:
				t.Error("ListPools succeeded, want the connection to be refused")
			case !tc.wantErr && err != nil:
				t.Errorf("ListPools: %v", err)
			}
		})
	}
}

// TestVerificationCannotBeSkipped is the SE-022 guard on this connection:
// the configuration has no field that would turn certificate verification
// off, so a client of a server whose certificate is signed by an unknown
// authority cannot be made to connect.
func TestVerificationCannotBeSkipped(t *testing.T) {
	t.Parallel()
	if _, ok := any(config.ClientTLS{}).(interface{ SkipVerify() bool }); ok {
		t.Error("config.ClientTLS grew a way to skip verification")
	}
	cfg := config.ClientTLS{CAFile: "/nonexistent/ca.pem"}
	if _, err := poolmgr.NewClient(poolmgr.ClientConfig{Endpoint: "127.0.0.1:1", TLS: cfg}); err == nil {
		t.Error("NewClient accepted a certificate authority file that does not exist")
	}
}

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# The Scheduler SHALL maintain one long-lived connection to the Pool
//# Manager with keepalive enabled and SHALL reconnect with exponential
//# backoff when it is lost.

// TestOneConnectionThatReconnects checks both halves against a real server:
// a run of calls goes over one accepted TCP connection rather than one per
// call, and a client whose server has gone away reconnects by itself and
// works again once a server is back.
func TestOneConnectionThatReconnects(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("one connection carries every call", func(t *testing.T) {
		pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
		px := startProxy(t, pm.addr, "", "")
		c, err := poolmgr.NewClient(poolmgr.ClientConfig{
			Endpoint: px.addr,
			TLS:      config.ClientTLS{Insecure: true},
			Deadline: 5 * time.Second,
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		defer func() { _ = c.Close() }()

		for i := 0; i < 10; i++ {
			if _, err := c.ListPools(ctx, testNamespace); err != nil {
				t.Fatalf("ListPools: %v", err)
			}
		}
		if got := px.connections(); got != 1 {
			t.Errorf("the server accepted %d connections for 10 calls, want 1", got)
		}
	})

	t.Run("the client reconnects to a pool manager that comes back", func(t *testing.T) {
		firstCtx, stopFirst := context.WithCancel(ctx)
		defer stopFirst()
		first := startPoolManager(t, firstCtx, poolmgr.FakeConfig{}, "host-a")

		dialer := &switchDialer{addr: first.addr}
		c, err := poolmgr.NewClient(poolmgr.ClientConfig{
			Endpoint:      "passthrough:///pool-manager",
			TLS:           config.ClientTLS{Insecure: true},
			Deadline:      2 * time.Second,
			ReconnectBase: 10 * time.Millisecond,
			ReconnectMax:  50 * time.Millisecond,
			DialOptions:   []grpc.DialOption{grpc.WithContextDialer(dialer.dial)},
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		defer func() { _ = c.Close() }()

		if _, err := c.ListPools(ctx, testNamespace); err != nil {
			t.Fatalf("ListPools against the first pool manager: %v", err)
		}

		// Take the Pool Manager away, then bring a different one back at a
		// different address. The Pool declared only on the second one is
		// what proves the client reconnected rather than answering from
		// anything it had cached.
		stopFirst()
		second := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
		marker := specFor(t, testProfile("second-pool-manager", 0), "host-a")
		direct := second.client()
		if _, err := direct.CreatePool(ctx, marker); err != nil {
			t.Fatalf("declaring the marker pool: %v", err)
		}
		dialer.set(second.addr)

		eventually(t, ctx, "the client to reconnect", func() bool {
			pools, err := c.ListPools(ctx, testNamespace)
			return err == nil && len(pools) == 1 && pools[0].Spec.Ref == marker.Ref
		})
	})
}

// TestErrorsMapToSentinels checks the status codes the Scheduler branches
// on, so that no caller has to import gRPC codes.
func TestErrorsMapToSentinels(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{Namespace: testNamespace}, "host-a")
	c := pm.client()
	spec := specFor(t, testProfile("small", 1), "host-a")

	tests := []struct {
		name string
		call func() error
		want error
	}{
		{
			name: "claiming an unknown pool is not found",
			call: func() error { _, err := c.ClaimVM(ctx, ref("nothing-here")); return err },
			want: poolmgr.ErrNotFound,
		},
		{
			name: "claiming an empty pool is exhausted",
			call: func() error {
				empty := specFor(t, testProfile("empty", 0), "host-a")
				if _, err := c.CreatePool(ctx, empty); err != nil {
					return err
				}
				_, err := c.ClaimVM(ctx, empty.Ref)
				return err
			},
			want: poolmgr.ErrExhausted,
		},
		{
			name: "creating a pool twice already exists",
			call: func() error {
				if _, err := c.CreatePool(ctx, spec); err != nil {
					return err
				}
				_, err := c.CreatePool(ctx, spec)
				return err
			},
			want: poolmgr.ErrAlreadyExists,
		},
		{
			name: "a pool in another namespace is invalid",
			call: func() error {
				other := spec
				other.Ref.Namespace = "someone-else"
				_, err := c.CreatePool(ctx, other)
				return err
			},
			want: poolmgr.ErrInvalid,
		},
		{
			name: "an unknown lease is not found",
			call: func() error { return c.ReleaseVM(ctx, "no-such-lease") },
			want: poolmgr.ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestUnavailableIsReportedAsUnavailable checks that the fault-injected
// UNAVAILABLE period reaches the caller as ErrUnavailable, which is what
// sends the Scheduler to Health.MarkUnavailable (PL-034).
func TestUnavailableIsReportedAsUnavailable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	pm.setFaults(poolmgr.Faults{UnavailableFor: time.Hour})
	if _, err := c.ListPools(ctx, testNamespace); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("ListPools during the unavailable period = %v, want ErrUnavailable", err)
	}
	pm.setFaults(poolmgr.Faults{})
	if _, err := c.ListPools(ctx, testNamespace); err != nil {
		t.Errorf("ListPools after the unavailable period: %v", err)
	}
}

// TestSubscribeReportsADroppedStream checks that a stream the Runner did
// not close comes back as ErrUnavailable, which is what makes the Tracker
// poll and re-subscribe (PL-051).
func TestSubscribeReportsADroppedStream(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()
	stream, err := c.Subscribe(ctx, poolmgr.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// A gRPC stream is established lazily, so wait for an event before
	// injecting the fault: until the server's handler is running there is no
	// subscription for the fault to drop.
	spec := specFor(t, testProfile("small", 1), "host-a")
	if _, err := c.CreatePool(ctx, spec); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	awaitEvent(t, ctx, stream, poolmgrv1.EventType_VM_AVAILABLE)

	pm.setFaults(poolmgr.Faults{DropEventsStream: true})
	if _, err := stream.Recv(ctx); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("Recv on a dropped stream = %v, want ErrUnavailable", err)
	}
	if _, err := stream.Recv(ctx); !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Error("Recv after the stream ended did not repeat the terminal error")
	}
}

// TestSubscribeThatCannotStartIsUnavailable checks that a Subscribe the
// connection refuses comes back as ErrUnavailable rather than as a
// cancellation. Subscribe cancels the stream's own context on the failure
// path, so mapping the error against that context would answer with its
// cancellation for a CANCELLED status, and a Tracker that saw
// context.Canceled would take it for its own shutdown instead of polling
// and re-subscribing (PL-051).
func TestSubscribeThatCannotStartIsUnavailable(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint: pm.addr,
		TLS:      config.ClientTLS{Insecure: true},
		Deadline: testTimeout,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// A connection that is closing answers every new call with CANCELLED,
	// which is a cancellation the caller did not ask for.
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	stream, err := c.Subscribe(ctx, poolmgr.EventFilter{})
	if err == nil {
		_ = stream.Close()
		t.Fatal("Subscribe succeeded on a closed connection")
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe on a closed connection = %v, want it not reported as the caller's cancellation", err)
	}
	if !errors.Is(err, poolmgr.ErrUnavailable) {
		t.Errorf("Subscribe on a closed connection = %v, want ErrUnavailable", err)
	}
	if ctx.Err() != nil {
		t.Errorf("the caller's context ended during the test: %v", ctx.Err())
	}
}

// TestClientRejectsAnIncompleteConfiguration covers the configuration
// mistakes NewClient can catch before anything is dialled.
func TestClientRejectsAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  poolmgr.ClientConfig
		want string
	}{
		{
			name: "no endpoint",
			cfg:  poolmgr.ClientConfig{TLS: config.ClientTLS{Insecure: true}},
			want: "endpoint is required",
		},
		{
			name: "a client key without its certificate",
			cfg:  poolmgr.ClientConfig{Endpoint: "127.0.0.1:1", TLS: config.ClientTLS{KeyFile: "key.pem"}},
			want: "cert_file and key_file",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := poolmgr.NewClient(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("NewClient error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// switchDialer dials whichever address it currently points at.
type switchDialer struct {
	mu   sync.Mutex
	addr string
}

func (d *switchDialer) set(addr string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.addr = addr
}

func (d *switchDialer) dial(ctx context.Context, _ string) (net.Conn, error) {
	d.mu.Lock()
	addr := d.addr
	d.mu.Unlock()
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

// eventually retries fn until it is true. It is used only where the thing
// being waited for is gRPC's own connection management, which runs on the
// wall clock and cannot be driven by the fake clock the units use.
func eventually(t *testing.T, ctx context.Context, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if fn() {
			return
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
