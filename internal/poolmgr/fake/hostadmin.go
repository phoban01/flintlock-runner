package fake

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// hostRecord is one Host the fake holds, as battery's store keeps it. The
// fake records the address and TLS settings but never dials them: it
// reaches a Host through the HostSource, by name (TD-002).
type hostRecord struct {
	host      poolmgr.Host
	updatedAt time.Time
}

// heldLocked returns the record of a Host the fake holds. Unless
// EmptyHostRegistry is set, a name the HostSource knows is held until
// RemoveHost removes it; its record, made on first use, has the source's
// address and no TLS settings, as battery v0.5.0 keeps a Host it took over
// from a v0.4 database.
func (p *PoolManager) heldLocked(name string) (*hostRecord, bool) {
	if r, ok := p.hosts[name]; ok {
		return r, true
	}
	if p.cfg.EmptyHostRegistry || p.removedHosts[name] || !slices.Contains(p.cfg.Hosts.Names(), name) {
		return nil, false
	}
	r := &hostRecord{host: poolmgr.Host{Name: name, Address: p.hostAddress(name)}, updatedAt: p.cfg.Clock.Now()}
	p.hosts[name] = r
	return r, true
}

// heldNamesLocked lists every Host the fake holds, sorted.
func (p *PoolManager) heldNamesLocked() []string {
	if !p.cfg.EmptyHostRegistry {
		for _, name := range p.cfg.Hosts.Names() {
			p.heldLocked(name)
		}
	}
	names := make([]string, 0, len(p.hosts))
	for name := range p.hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL refuse with `INVALID_ARGUMENT` a
//# Pool whose `flintlock_hosts` names a Host it does not hold, and SHALL
//# place MicroVMs only on Hosts it holds.

// checkHostsLocked is battery's checkFlintlockHosts: a Pool may name only
// Hosts the Pool Manager holds.
func (p *PoolManager) checkHostsLocked(spec poolmgr.PoolSpec) error {
	var unknown []string
	for _, name := range spec.FlintlockHosts {
		if _, ok := p.heldLocked(name); !ok && !slices.Contains(unknown, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return errInvalid("spec.flintlock_hosts: unknown hosts %s; add them with HostAdmin.AddHost first", strings.Join(unknown, ", "))
	}
	return nil
}

// vmCountLocked is battery's CountVMsByHost: every MicroVM on the Host, in
// any phase.
func (p *PoolManager) vmCountLocked(name string) int32 {
	var n int32
	for _, vm := range p.vms {
		if vm.host == name {
			n++
		}
	}
	return n
}

func (p *PoolManager) hostStatusLocked(r *hostRecord) *poolmgrv1.HostStatus {
	h := poolmgr.HostToProto(r.host)
	h.UpdatedAt = timestamppb.New(r.updatedAt)
	return &poolmgrv1.HostStatus{Host: h, VmCount: p.vmCountLocked(r.host.Name)}
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL serve `AddHost`, `UpdateHost`,
//# `RemoveHost`, `GetHost` and `ListHosts` of the `poolmgr.v1alpha1`
//# `HostAdmin` service, and SHALL answer `ALREADY_EXISTS` to `AddHost` for a
//# Host it holds and `NOT_FOUND` to the others for a Host it does not hold.

// hostAdminServer implements the poolmgrv1.HostAdminServer calls the Fleet
// Controller makes, with battery v0.5.0's answers. CordonHost and
// UncordonHost stay unimplemented: nothing here cordons a Host.
type hostAdminServer struct {
	poolmgrv1.UnimplementedHostAdminServer
	pm *PoolManager
}

// ListHosts implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) ListHosts(context.Context, *poolmgrv1.ListHostsRequest) (*poolmgrv1.ListHostsResponse, error) {
	p := s.pm
	p.mu.Lock()
	defer p.mu.Unlock()
	resp := &poolmgrv1.ListHostsResponse{}
	for _, name := range p.heldNamesLocked() {
		resp.Hosts = append(resp.Hosts, p.hostStatusLocked(p.hosts[name]))
	}
	return resp, nil
}

// GetHost implements poolmgrv1.HostAdminServer.
func (s *hostAdminServer) GetHost(_ context.Context, req *poolmgrv1.GetHostRequest) (*poolmgrv1.HostStatus, error) {
	if req.GetName() == "" {
		return nil, errInvalid("name is required")
	}
	p := s.pm
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.heldLocked(req.GetName())
	if !ok {
		return nil, errNotFound("host %q not found", req.GetName())
	}
	return p.hostStatusLocked(r), nil
}

// AddHost implements poolmgrv1.HostAdminServer: ALREADY_EXISTS for a Host
// the fake holds. Without skip_validation, a Host the HostSource cannot
// resolve is FAILED_PRECONDITION, as battery answers for one it cannot dial.
func (s *hostAdminServer) AddHost(_ context.Context, req *poolmgrv1.AddHostRequest) (*poolmgrv1.Host, error) {
	h := poolmgr.HostFromProto(req.GetHost())
	if h.Name == "" {
		return nil, errInvalid("host.name is required")
	}
	p := s.pm
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.heldLocked(h.Name); ok {
		return nil, status.Errorf(codes.AlreadyExists, "host %q already exists", h.Name)
	}
	if err := p.validateHostLocked(h.Name, req.GetSkipValidation()); err != nil {
		return nil, err
	}
	r := &hostRecord{host: h, updatedAt: p.cfg.Clock.Now()}
	p.hosts[h.Name] = r
	delete(p.removedHosts, h.Name)
	p.kickReconcile()
	return p.hostStatusLocked(r).GetHost(), nil
}

// UpdateHost implements poolmgrv1.HostAdminServer: NOT_FOUND for a Host the
// fake does not hold; the cordon fields are never touched.
func (s *hostAdminServer) UpdateHost(_ context.Context, req *poolmgrv1.UpdateHostRequest) (*poolmgrv1.Host, error) {
	h := poolmgr.HostFromProto(req.GetHost())
	if h.Name == "" {
		return nil, errInvalid("host.name is required")
	}
	p := s.pm
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.heldLocked(h.Name)
	if !ok {
		return nil, errNotFound("host %q not found", h.Name)
	}
	if err := p.validateHostLocked(h.Name, req.GetSkipValidation()); err != nil {
		return nil, err
	}
	r.host = h
	r.updatedAt = p.cfg.Clock.Now()
	return p.hostStatusLocked(r).GetHost(), nil
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL refuse with `FAILED_PRECONDITION` to
//# remove a Host that a Pool names or that a MicroVM is on.

// RemoveHost implements poolmgrv1.HostAdminServer: NOT_FOUND for a Host the
// fake does not hold, and FAILED_PRECONDITION, with battery's message,
// while a Pool names the Host or a MicroVM is on it.
func (s *hostAdminServer) RemoveHost(_ context.Context, req *poolmgrv1.RemoveHostRequest) (*emptypb.Empty, error) {
	name := req.GetName()
	if name == "" {
		return nil, errInvalid("name is required")
	}
	p := s.pm
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.heldLocked(name); !ok {
		return nil, errNotFound("host %q not found", name)
	}
	var pools []string
	for _, ps := range p.pools {
		if slices.Contains(ps.spec.FlintlockHosts, name) {
			pools = append(pools, ps.key.name)
		}
	}
	sort.Strings(pools)
	count := p.vmCountLocked(name)
	if len(pools) > 0 || count > 0 {
		var reasons []string
		if len(pools) > 0 {
			reasons = append(reasons, fmt.Sprintf("named in flintlock_hosts by pools %s", strings.Join(pools, ", ")))
		}
		if count > 0 {
			reasons = append(reasons, fmt.Sprintf("vm_count is %d", count))
		}
		return nil, status.Errorf(codes.FailedPrecondition,
			"host %q is still in use (%s): cordon it, drop it from each pool spec, and wait for vm_count to reach 0",
			name, strings.Join(reasons, "; "))
	}
	delete(p.hosts, name)
	p.removedHosts[name] = true
	return &emptypb.Empty{}, nil
}

// validateHostLocked is AddHost's and UpdateHost's check of a Host before
// the fake records it: unless skip is set, the HostSource has to resolve
// the name, where battery dials the address.
func (p *PoolManager) validateHostLocked(name string, skip bool) error {
	if skip {
		return nil
	}
	if _, err := p.cfg.Hosts.Host(name); err != nil {
		return status.Errorf(codes.FailedPrecondition, "validate host %q: %v", name, err)
	}
	return nil
}

// HostAdmin returns an in-process poolmgr.HostAdminConn bound to this
// fake: the Runner's own HostAdmin client over the in-memory connection
// Client uses. Taken after the fake has shut down, it fails with
// ErrStopped.
func (p *PoolManager) HostAdmin() (poolmgr.HostAdminConn, error) {
	p.mu.Lock()
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		return nil, ErrStopped
	}
	lis := p.loop.lis
	c, err := poolmgr.DialHostAdmin(poolmgr.ClientConfig{
		Endpoint: "passthrough:///fake-poolmgr",
		TLS:      config.ClientTLS{Insecure: true},
		DialOptions: []grpc.DialOption{
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		},
	})
	if err != nil {
		return nil, errors.Join(errors.New("fake poolmgr: loopback"), err)
	}
	return c, nil
}
