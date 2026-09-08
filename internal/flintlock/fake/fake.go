// Package fake is the fake Host (docs/requirements/10-test-doubles.md#fake-host):
// a flintlock MicroVM and MicroVMExec server (TD-020) that represents each
// MicroVM as a sandbox directory and runs ExecCommand as a local process
// (TD-021). It also provides an in-memory flintlock.PoolHostClient bound to a
// Host directly, plus a Dialer/AdminDialer over a set of fake Hosts, so that
// Scheduler and transport tests run without gRPC.
//
// Phase 0 provides the compiling skeleton: every method returns
// errors.ErrUnsupported or a zero value. The `fakes` work package of
// docs/PLAN.md fills it in; each todo annotation below names the requirement
// it will satisfy.
package fake

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The project SHALL provide a fake Host that serves the flintlock
//# `MicroVM` service, including `ServerInfo`, and the `MicroVMExec` service
//# over gRPC using the generated server stubs from the flintlock `api`
//# module.

// Host is one fake Host.
type Host struct {
	cfg flintlock.FakeHostConfig

	mu     sync.Mutex
	faults flintlock.HostFaults
}

// New builds a fake Host from cfg.
func New(cfg flintlock.FakeHostConfig) *Host { return &Host{cfg: cfg} }

// Config returns the configuration the Host was built with.
func (h *Host) Config() flintlock.FakeHostConfig { return h.cfg }

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The fake Host SHALL represent each MicroVM as a sandbox directory on the
//# local filesystem and SHALL run each `ExecCommand` as a local process
//# rooted in that directory, streaming standard input, standard output,
//# standard error and the exit code as `flintlockd` does.

// Serve listens on cfg.Listen and serves the gRPC services until ctx is
// cancelled (TD-020, TD-024).
func (h *Host) Serve(ctx context.Context) error {
	_ = ctx
	return errors.ErrUnsupported
}

// Addr is the bound listen address once Serve is running, empty before.
func (h *Host) Addr() string { return "" }

// Client returns an in-memory PoolHostClient bound to this Host, for tests
// that need no gRPC.
func (h *Host) Client() flintlock.PoolHostClient { return &Client{host: h} }

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The fake Host SHALL support fault injection for a create that ends in
//# `FAILED`, an exec stream dropped before the exit code, and a Host that
//# stops answering.

// SetFaults implements flintlock.HostFaultInjector.
func (h *Host) SetFaults(f flintlock.HostFaults) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults = f
}

// Faults implements flintlock.HostFaultInjector.
func (h *Host) Faults() flintlock.HostFaults {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.faults
}

// Client is the in-memory flintlock.PoolHostClient over a Host.
type Client struct {
	host *Host
}

// Name implements flintlock.HostClient.
func (c *Client) Name() string { return c.host.cfg.Name }

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The fake Host SHALL report a configurable flintlock version and service
//# flags from `ServerInfo` and SHALL provide a mode in which `ServerInfo`
//# returns `UNIMPLEMENTED`.

// ServerInfo implements flintlock.HostClient.
func (c *Client) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	return nil, errors.ErrUnsupported
}

// GetMicroVM implements flintlock.HostClient.
func (c *Client) GetMicroVM(context.Context, string) (*types.MicroVM, error) {
	return nil, errors.ErrUnsupported
}

// ListMicroVMs implements flintlock.HostClient.
func (c *Client) ListMicroVMs(context.Context, string) ([]*types.MicroVM, error) {
	return nil, errors.ErrUnsupported
}

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The fake Host SHALL honour the `cwd`, `env`, `timeout_seconds`,
//# `has_stdin` and `stdin_eof` fields of an exec request and SHALL accept
//# and ignore `user`.

// Exec implements flintlock.HostClient.
func (c *Client) Exec(context.Context) (flintlock.ExecStream, error) {
	return nil, errors.ErrUnsupported
}

// SSHProxy implements flintlock.HostClient.
func (c *Client) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.ErrUnsupported
}

// Close implements flintlock.HostClient.
func (c *Client) Close() error { return nil }

//= docs/requirements/10-test-doubles.md#fake-host
//= type=todo
//= tracking-issue=TBD
//# The fake Host SHALL move a MicroVM from `PENDING` to `CREATED` after a
//# configurable boot delay and SHALL populate `vsock_path` in its status.

// CreateMicroVM implements flintlock.HostAdminClient.
func (c *Client) CreateMicroVM(context.Context, *types.MicroVMSpec) (*types.MicroVM, error) {
	return nil, errors.ErrUnsupported
}

// DeleteMicroVM implements flintlock.HostAdminClient.
func (c *Client) DeleteMicroVM(context.Context, string) error { return errors.ErrUnsupported }

// Dialer hands out in-memory clients for a set of fake Hosts keyed by name.
// It implements both flintlock.Dialer and flintlock.AdminDialer; the
// Scheduler is given it as a Dialer and the fake Pool Manager as an
// AdminDialer, which keeps the HO-007 split intact in tests.
type Dialer struct {
	mu    sync.Mutex
	hosts map[string]*Host
}

// NewDialer builds a Dialer over hosts.
func NewDialer(hosts ...*Host) *Dialer {
	d := &Dialer{hosts: make(map[string]*Host, len(hosts))}
	for _, h := range hosts {
		d.hosts[h.cfg.Name] = h
	}
	return d
}

// Dial implements flintlock.Dialer. It matches on ep.Name, returns
// flintlock.ErrUnknownHost for a Host it does not have, and hides the admin
// methods of the underlying client.
func (d *Dialer) Dial(ctx context.Context, ep flintlock.Endpoint) (flintlock.HostClient, error) {
	c, err := d.DialAdmin(ctx, ep)
	if err != nil {
		return nil, err
	}
	// Wrap so that a type assertion to flintlock.HostAdminClient fails in
	// tests exactly as it would against a real Runner-side client (HO-007).
	return struct{ flintlock.HostClient }{c}, nil
}

// DialAdmin implements flintlock.AdminDialer.
func (d *Dialer) DialAdmin(_ context.Context, ep flintlock.Endpoint) (flintlock.PoolHostClient, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h, ok := d.hosts[ep.Name]
	if !ok {
		return nil, flintlock.ErrUnknownHost
	}
	return h.Client(), nil
}

// Compile-time interface checks.
var (
	_ flintlock.PoolHostClient    = (*Client)(nil)
	_ flintlock.HostFaultInjector = (*Host)(nil)
	_ flintlock.Dialer            = (*Dialer)(nil)
	_ flintlock.AdminDialer       = (*Dialer)(nil)
)
