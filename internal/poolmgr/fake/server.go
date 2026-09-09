package fake

import (
	"context"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// newGRPCServer builds a server with the three services registered from the
// battery module's generated stubs (TD-001) and the UNAVAILABLE fault
// interceptors (TD-010). Serve and the loopback client each build one over
// the same PoolManager.
func (p *PoolManager) newGRPCServer() *grpc.Server {
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(p.unaryFaultInterceptor),
		grpc.ChainStreamInterceptor(p.streamFaultInterceptor),
	)
	poolmgrv1.RegisterPoolAdminServer(srv, &poolAdminServer{pm: p})
	poolmgrv1.RegisterLeaseServer(srv, &leaseServer{pm: p})
	poolmgrv1.RegisterEventsServer(srv, &eventsServer{pm: p})
	return srv
}

func (p *PoolManager) unaryFaultInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := p.unavailable(); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (p *PoolManager) streamFaultInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := p.unavailable(); err != nil {
		return err
	}
	return handler(srv, ss)
}

// poolAdminServer implements poolmgrv1.PoolAdminServer.
type poolAdminServer struct {
	poolmgrv1.UnimplementedPoolAdminServer
	pm *PoolManager
}

func (s *poolAdminServer) CreatePool(_ context.Context, req *poolmgrv1.CreatePoolRequest) (*poolmgrv1.Pool, error) {
	spec, err := specFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	pool, err := s.pm.createPool(spec)
	if err != nil {
		return nil, err
	}
	return poolToProto(pool), nil
}

func (s *poolAdminServer) UpdatePool(_ context.Context, req *poolmgrv1.UpdatePoolRequest) (*poolmgrv1.Pool, error) {
	spec, err := specFromProto(req.GetSpec())
	if err != nil {
		return nil, err
	}
	pool, err := s.pm.updatePool(spec)
	if err != nil {
		return nil, err
	}
	return poolToProto(pool), nil
}

func (s *poolAdminServer) DeletePool(ctx context.Context, req *poolmgrv1.DeletePoolRequest) (*emptypb.Empty, error) {
	if err := s.pm.deletePool(ctx, refFromProto(req.GetRef())); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *poolAdminServer) GetPool(_ context.Context, req *poolmgrv1.GetPoolRequest) (*poolmgrv1.Pool, error) {
	pool, err := s.pm.getPool(refFromProto(req.GetRef()))
	if err != nil {
		return nil, err
	}
	return poolToProto(pool), nil
}

func (s *poolAdminServer) ListPools(_ context.Context, req *poolmgrv1.ListPoolsRequest) (*poolmgrv1.ListPoolsResponse, error) {
	resp := &poolmgrv1.ListPoolsResponse{}
	for _, pool := range s.pm.listPools(req.GetNamespace()) {
		resp.Pools = append(resp.Pools, poolToProto(pool))
	}
	return resp, nil
}

// leaseServer implements poolmgrv1.LeaseServer.
type leaseServer struct {
	poolmgrv1.UnimplementedLeaseServer
	pm *PoolManager
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL populate the Host name on `ClaimVMResponse`
//# when the proto carries a field for it and SHALL provide a switch that
//# omits it, so that both Placement paths of the Scheduler are tested.

// ClaimVM fills ClaimVMResponse.host with the placed Host's name and address
// (TD-008); with FakeConfig.OmitHostOnClaim the field stays unset, as it is
// from a battery that predates it.
func (s *leaseServer) ClaimVM(ctx context.Context, req *poolmgrv1.ClaimVMRequest) (*poolmgrv1.ClaimVMResponse, error) {
	res, err := s.pm.claimVM(ctx, refFromProto(req.GetPool()))
	if err != nil {
		return nil, err
	}
	resp := &poolmgrv1.ClaimVMResponse{LeaseId: res.leaseID, VmUid: res.vmUID, NetworkInterfaces: res.ifaces}
	if res.host != nil {
		resp.Host = &poolmgrv1.HostInfo{Name: res.host.Name, Address: res.host.Address}
	}
	return resp, nil
}

func (s *leaseServer) Heartbeat(_ context.Context, req *poolmgrv1.HeartbeatRequest) (*poolmgrv1.HeartbeatResponse, error) {
	expires, err := s.pm.heartbeat(req.GetLeaseId())
	if err != nil {
		return nil, err
	}
	return &poolmgrv1.HeartbeatResponse{ExpiresAt: timestamppb.New(expires)}, nil
}

func (s *leaseServer) ReleaseVM(ctx context.Context, req *poolmgrv1.ReleaseVMRequest) (*emptypb.Empty, error) {
	if err := s.pm.releaseVM(ctx, req.GetLeaseId()); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

// eventsServer implements poolmgrv1.EventsServer.
type eventsServer struct {
	poolmgrv1.UnimplementedEventsServer
	pm *PoolManager
}

// Subscribe replays the recent events of the requested Pool, or of every
// Pool, then streams live events until the client goes away or the stream
// is dropped by fault injection (TD-010), which ends it with UNAVAILABLE.
func (s *eventsServer) Subscribe(req *poolmgrv1.SubscribeRequest, stream grpc.ServerStreamingServer[poolmgrv1.Event]) error {
	var filter *poolKey
	if ref := req.GetPool(); ref != nil {
		if err := s.pm.checkNamespace(ref.GetNamespace()); err != nil {
			return err
		}
		k := keyOf(refFromProto(ref))
		filter = &k
	}
	sub, backlog := s.pm.events.subscribe(filter)
	defer s.pm.events.unsubscribe(sub.id)
	for _, e := range backlog {
		if err := stream.Send(proto.Clone(e).(*poolmgrv1.Event)); err != nil {
			return err
		}
	}
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-sub.dropped:
			return status.Error(codes.Unavailable, "events stream dropped")
		case e := <-sub.ch:
			if err := stream.Send(proto.Clone(e).(*poolmgrv1.Event)); err != nil {
				return err
			}
		}
	}
}

// Proto conversions, shared with the loopback client.

func refFromProto(r *poolmgrv1.PoolRef) poolmgr.PoolRef {
	return poolmgr.PoolRef{Name: r.GetName(), Namespace: r.GetNamespace()}
}

func refToProto(r poolmgr.PoolRef) *poolmgrv1.PoolRef {
	return &poolmgrv1.PoolRef{Name: r.Name, Namespace: r.Namespace}
}

// specFromProto converts a wire PoolSpec. A nil spec is INVALID_ARGUMENT.
func specFromProto(s *poolmgrv1.PoolSpec) (poolmgr.PoolSpec, error) {
	if s == nil {
		return poolmgr.PoolSpec{}, errInvalid("spec is required")
	}
	spec := poolmgr.PoolSpec{
		Ref:               poolmgr.PoolRef{Name: s.GetName(), Namespace: s.GetNamespace()},
		Size:              s.GetSize(),
		FlintlockHosts:    s.GetFlintlockHosts(),
		CreateCommands:    s.GetCreateCommands(),
		PreLeaseCommands:  s.GetPreLeaseCommands(),
		HookFailurePolicy: s.GetHookFailurePolicy(),
	}
	if t := s.GetMicrovmTemplate(); t != nil {
		spec.Template = proto.Clone(t).(*types.MicroVMSpec)
	}
	if rs := s.GetReplenishmentStrategy(); rs != nil {
		spec.Replenishment.Type = rs.GetType()
		if rs.MinSize != nil {
			v := rs.GetMinSize()
			spec.Replenishment.MinSize = &v
		}
	}
	if d := s.GetHeartbeatInterval(); d != nil {
		spec.HeartbeatInterval = d.AsDuration()
	}
	if d := s.GetHeartbeatExpiryThreshold(); d != nil {
		spec.HeartbeatExpiryThreshold = d.AsDuration()
	}
	return spec, nil
}

func specToProto(spec poolmgr.PoolSpec) *poolmgrv1.PoolSpec {
	out := &poolmgrv1.PoolSpec{
		Name:                  spec.Ref.Name,
		Namespace:             spec.Ref.Namespace,
		Size:                  spec.Size,
		FlintlockHosts:        spec.FlintlockHosts,
		ReplenishmentStrategy: &poolmgrv1.ReplenishmentStrategy{Type: spec.Replenishment.Type},
		CreateCommands:        spec.CreateCommands,
		PreLeaseCommands:      spec.PreLeaseCommands,
		HookFailurePolicy:     spec.HookFailurePolicy,
	}
	if spec.Template != nil {
		out.MicrovmTemplate = proto.Clone(spec.Template).(*types.MicroVMSpec)
	}
	if spec.Replenishment.MinSize != nil {
		v := *spec.Replenishment.MinSize
		out.ReplenishmentStrategy.MinSize = &v
	}
	if spec.HeartbeatInterval > 0 {
		out.HeartbeatInterval = durationpb.New(spec.HeartbeatInterval)
	}
	if spec.HeartbeatExpiryThreshold > 0 {
		out.HeartbeatExpiryThreshold = durationpb.New(spec.HeartbeatExpiryThreshold)
	}
	return out
}

func poolToProto(pool *poolmgr.Pool) *poolmgrv1.Pool {
	return &poolmgrv1.Pool{
		Spec: specToProto(pool.Spec),
		Status: &poolmgrv1.PoolStatus{
			AvailableCount:    pool.Status.Available,
			LeasedCount:       pool.Status.Leased,
			ProvisioningCount: pool.Status.Provisioning,
			QuarantinedCount:  pool.Status.Quarantined,
		},
	}
}

func poolFromProto(pool *poolmgrv1.Pool) (*poolmgr.Pool, error) {
	spec, err := specFromProto(pool.GetSpec())
	if err != nil {
		return nil, err
	}
	st := pool.GetStatus()
	return &poolmgr.Pool{Spec: spec, Status: poolmgr.PoolStatus{
		Available:    st.GetAvailableCount(),
		Leased:       st.GetLeasedCount(),
		Provisioning: st.GetProvisioningCount(),
		Quarantined:  st.GetQuarantinedCount(),
	}}, nil
}
