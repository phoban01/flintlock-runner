package poolmgr

import (
	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Conversions between the package's plain Go structs and the generated
// messages. They are the only place the wire shape is known, so the
// Scheduler builds PoolSpec values as literals and never touches the proto.

func refToProto(r PoolRef) *poolmgrv1.PoolRef {
	return &poolmgrv1.PoolRef{Name: r.Name, Namespace: r.Namespace}
}


// specToProto renders a PoolSpec. The template is cloned so that a caller
// that keeps its spec cannot see the message the client sent mutated, and
// so that the Pool Manager's reply cannot alias it.
func specToProto(spec PoolSpec) *poolmgrv1.PoolSpec {
	out := &poolmgrv1.PoolSpec{
		Name:                  spec.Ref.Name,
		Namespace:             spec.Ref.Namespace,
		Size:                  spec.Size,
		FlintlockHosts:        append([]string(nil), spec.FlintlockHosts...),
		ReplenishmentStrategy: &poolmgrv1.ReplenishmentStrategy{Type: spec.Replenishment.Type},
		CreateCommands:        append([]string(nil), spec.CreateCommands...),
		PreLeaseCommands:      append([]string(nil), spec.PreLeaseCommands...),
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

// specFromProto converts a wire PoolSpec back. A nil message yields the zero
// PoolSpec rather than an error, because it only reaches the client as part
// of a Pool the server sent.
func specFromProto(s *poolmgrv1.PoolSpec) PoolSpec {
	spec := PoolSpec{
		Ref:               PoolRef{Name: s.GetName(), Namespace: s.GetNamespace()},
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
	return spec
}

// poolFromProto converts a Pool and its status.
func poolFromProto(pool *poolmgrv1.Pool) *Pool {
	st := pool.GetStatus()
	return &Pool{
		Spec: specFromProto(pool.GetSpec()),
		Status: PoolStatus{
			Available:    st.GetAvailableCount(),
			Leased:       st.GetLeasedCount(),
			Provisioning: st.GetProvisioningCount(),
			Quarantined:  st.GetQuarantinedCount(),
		},
	}
}
