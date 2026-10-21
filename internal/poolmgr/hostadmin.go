package poolmgr

import (
	"context"
	"fmt"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/grpc"
)

// hostAdminConn is the HostAdmin client: one gRPC connection carrying the
// generated HostAdmin client. Like client, it maps every gRPC status onto
// the package's sentinel errors.
type hostAdminConn struct {
	conn  *grpc.ClientConn
	hosts poolmgrv1.HostAdminClient
}

// DialHostAdmin builds a HostAdmin client for the Pool Manager cfg
// names, with the same transport credentials, keepalive and per-call
// deadline as NewClient. The Fleet Controller uses it to register the
// Inventory's Hosts (FL-127); the Runner does not.
func DialHostAdmin(cfg ClientConfig) (HostAdminConn, error) {
	conn, err := dial(cfg)
	if err != nil {
		return nil, err
	}
	return &hostAdminConn{conn: conn, hosts: poolmgrv1.NewHostAdminClient(conn)}, nil
}

// ListHosts implements HostAdmin.
func (c *hostAdminConn) ListHosts(ctx context.Context) ([]HostStatus, error) {
	resp, err := c.hosts.ListHosts(ctx, &poolmgrv1.ListHostsRequest{})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := make([]HostStatus, 0, len(resp.GetHosts()))
	for _, h := range resp.GetHosts() {
		out = append(out, HostStatusFromProto(h))
	}
	return out, nil
}

// GetHost implements HostAdmin.
func (c *hostAdminConn) GetHost(ctx context.Context, name string) (*HostStatus, error) {
	resp, err := c.hosts.GetHost(ctx, &poolmgrv1.GetHostRequest{Name: name})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	st := HostStatusFromProto(resp)
	return &st, nil
}

// AddHost implements HostAdmin. It sets skip_validation: the Pool Manager
// records the Host without dialling it, so a Host whose flintlockd is down
// for a moment is still registered, and a Pool that names it is still
// accepted. Reachability is what FL-047 and FL-054 check.
func (c *hostAdminConn) AddHost(ctx context.Context, h Host) (*Host, error) {
	resp, err := c.hosts.AddHost(ctx, &poolmgrv1.AddHostRequest{Host: HostToProto(h), SkipValidation: true})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := HostFromProto(resp)
	return &out, nil
}

// UpdateHost implements HostAdmin, without validation for the reason
// AddHost gives.
func (c *hostAdminConn) UpdateHost(ctx context.Context, h Host) (*Host, error) {
	resp, err := c.hosts.UpdateHost(ctx, &poolmgrv1.UpdateHostRequest{Host: HostToProto(h), SkipValidation: true})
	if err != nil {
		return nil, mapErr(ctx, err)
	}
	out := HostFromProto(resp)
	return &out, nil
}

// RemoveHost implements HostAdmin.
func (c *hostAdminConn) RemoveHost(ctx context.Context, name string) error {
	_, err := c.hosts.RemoveHost(ctx, &poolmgrv1.RemoveHostRequest{Name: name})
	return mapErr(ctx, err)
}

// Close implements HostAdminConn.
func (c *hostAdminConn) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("poolmgr: closing connection: %w", err)
	}
	return nil
}

// HostToProto converts h to the proto Host. The cordon fields stay unset:
// AddHost and UpdateHost ignore them.
func HostToProto(h Host) *poolmgrv1.Host {
	return &poolmgrv1.Host{
		Name:    h.Name,
		Address: h.Address,
		Tls: &poolmgrv1.HostTLS{
			Insecure: h.TLS.Insecure,
			CaFile:   h.TLS.CAFile,
			CertFile: h.TLS.CertFile,
			KeyFile:  h.TLS.KeyFile,
		},
	}
}

// HostFromProto converts a proto Host. A Host with no TLS message, as
// battery v0.5.0 keeps one it took over from a v0.4 database, has zero
// HostTLS.
func HostFromProto(h *poolmgrv1.Host) Host {
	t := h.GetTls()
	return Host{
		Name:    h.GetName(),
		Address: h.GetAddress(),
		TLS: HostTLS{
			Insecure: t.GetInsecure(),
			CAFile:   t.GetCaFile(),
			CertFile: t.GetCertFile(),
			KeyFile:  t.GetKeyFile(),
		},
	}
}

// HostStatusFromProto converts a proto HostStatus.
func HostStatusFromProto(s *poolmgrv1.HostStatus) HostStatus {
	return HostStatus{
		Host:     HostFromProto(s.GetHost()),
		Cordoned: s.GetHost().GetCordoned(),
		VMCount:  s.GetVmCount(),
	}
}

// Compile-time interface check.
var _ HostAdminConn = (*hostAdminConn)(nil)
