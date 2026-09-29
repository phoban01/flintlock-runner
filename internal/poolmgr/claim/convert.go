package claim

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Labels and annotations the backend puts on the resources it writes.
const (
	// LabelRunner and LabelProfile are the Runner and the Profile a Pool was
	// declared from, and LabelRunner is on every claim too. They are the
	// labels of the Pool's MicroVM template (PL-014), under the same keys.
	LabelRunner  = poolmgr.LabelRunner
	LabelProfile = poolmgr.LabelProfile
	// LabelArch is the well-known architecture label of a Node.
	LabelArch = "kubernetes.io/arch"
	// annotationPoolNamespace is the Runner namespace of the PoolRef a Pool
	// was declared under (PL-017). The Pool resource itself lives in the
	// backend's Kubernetes namespace.
	annotationPoolNamespace = kubelabels.Prefix + "pool-namespace"
)

// poolFromSpec builds the Pool resource of a PoolSpec, field by field as
// battery-operator's PoolSpec maps battery's. The template loses its
// namespace and id, which battery-operator sets, and allow_guest_agent,
// which battery forces. The Hosts come from the Profile: see nodeSelector.
func poolFromSpec(spec poolmgr.PoolSpec, profile config.Profile, namespace, runner string) (*batteryv1alpha1.Pool, error) {
	if spec.Template == nil {
		return nil, fmt.Errorf("%w: pool %s has no template", poolmgr.ErrInvalid, spec.Ref)
	}
	replenishment, err := replenishmentFromBattery(spec.Replenishment)
	if err != nil {
		return nil, fmt.Errorf("pool %s: %w", spec.Ref, err)
	}
	policy, err := hookPolicyFromBattery(spec.HookFailurePolicy)
	if err != nil {
		return nil, fmt.Errorf("pool %s: %w", spec.Ref, err)
	}
	pool := &batteryv1alpha1.Pool{
		ObjectMeta: metav1.ObjectMeta{
			Name:        spec.Ref.Name,
			Namespace:   namespace,
			Labels:      map[string]string{LabelRunner: runner, LabelProfile: spec.Template.GetLabels()[LabelProfile]},
			Annotations: map[string]string{annotationPoolNamespace: spec.Ref.Namespace},
		},
		Spec: batteryv1alpha1.PoolSpec{
			Template:      templateFromFlintlock(spec.Template),
			Size:          spec.Size,
			Placement:     batteryv1alpha1.PoolPlacement{NodeSelector: nodeSelector(profile)},
			Replenishment: replenishment,
			Hooks: batteryv1alpha1.PoolHooks{
				Create:        slices.Clone(spec.CreateCommands),
				PreLease:      slices.Clone(spec.PreLeaseCommands),
				FailurePolicy: policy,
			},
			Lease: batteryv1alpha1.PoolLease{
				HeartbeatInterval: duration(spec.HeartbeatInterval),
				ExpiryThreshold:   duration(spec.HeartbeatExpiryThreshold),
			},
		},
	}
	return pool, nil
}

// duration is d as an API duration, or nil for the CRD's default when d is
// not positive.
func duration(d time.Duration) *metav1.Duration {
	if d <= 0 {
		return nil
	}
	return &metav1.Duration{Duration: d}
}

// nodeSelector selects the Nodes of the Profile's architecture that carry
// every label of its Host selector. A Host selector key without a domain is
// looked for under this project's, because those are the labels a Host's
// Node carries; a key that names its own domain is used as it is.
func nodeSelector(profile config.Profile) map[string]string {
	sel := map[string]string{}
	if profile.Arch != "" {
		sel[LabelArch] = string(profile.Arch)
	}
	for key, value := range profile.HostSelector {
		if !strings.Contains(key, "/") {
			key = kubelabels.Prefix + key
		}
		sel[key] = value
	}
	return sel
}

// specFromPool is the PoolSpec of a Pool resource, for GetPool and
// ListPools. The PoolRef is the one the Pool was declared under. A Pool has
// no Host names, so FlintlockHosts stays empty: the claim names the Host,
// and nothing looks the MicroVM up on the Pool's Hosts.
func specFromPool(pool *batteryv1alpha1.Pool, runnerNamespace string) poolmgr.PoolSpec {
	s := pool.Spec
	out := poolmgr.PoolSpec{
		Ref:               refOf(pool, runnerNamespace),
		Template:          templateToFlintlock(s.Template),
		Size:              s.Size,
		Replenishment:     replenishmentToBattery(s.Replenishment),
		CreateCommands:    slices.Clone(s.Hooks.Create),
		PreLeaseCommands:  slices.Clone(s.Hooks.PreLease),
		HookFailurePolicy: hookPolicyToBattery(s.Hooks.FailurePolicy),
	}
	if d := s.Lease.HeartbeatInterval; d != nil {
		out.HeartbeatInterval = d.Duration
	}
	if d := s.Lease.ExpiryThreshold; d != nil {
		out.HeartbeatExpiryThreshold = d.Duration
	}
	return out
}

// statusFromPool is battery's counts, as the Pool's status reports them.
func statusFromPool(pool *batteryv1alpha1.Pool) poolmgr.PoolStatus {
	return poolmgr.PoolStatus{
		Available:    pool.Status.Available,
		Leased:       pool.Status.Leased,
		Provisioning: pool.Status.Provisioning,
		Quarantined:  pool.Status.Quarantined,
	}
}

// refOf is the PoolRef a Pool was declared under: its name, and the Runner
// namespace from its annotation, or runnerNamespace for a Pool that has none.
func refOf(pool *batteryv1alpha1.Pool, runnerNamespace string) poolmgr.PoolRef {
	ns := pool.Annotations[annotationPoolNamespace]
	if ns == "" {
		ns = runnerNamespace
	}
	return poolmgr.PoolRef{Name: pool.Name, Namespace: ns}
}

func replenishmentFromBattery(r poolmgr.ReplenishmentStrategy) (batteryv1alpha1.ReplenishmentStrategy, error) {
	out := batteryv1alpha1.ReplenishmentStrategy{MinSize: clonePtr(r.MinSize)}
	switch r.Type {
	case poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE:
		out.Type = batteryv1alpha1.ReplenishImmediateOnLease
	case poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD:
		out.Type = batteryv1alpha1.ReplenishMinSizeThreshold
	case poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE:
		out.Type = batteryv1alpha1.ReplenishReplaceOnDelete
	default:
		return out, fmt.Errorf("%w: replenishment strategy %s", poolmgr.ErrInvalid, r.Type)
	}
	return out, nil
}

func replenishmentToBattery(r batteryv1alpha1.ReplenishmentStrategy) poolmgr.ReplenishmentStrategy {
	out := poolmgr.ReplenishmentStrategy{MinSize: clonePtr(r.MinSize)}
	switch r.Type {
	case batteryv1alpha1.ReplenishImmediateOnLease:
		out.Type = poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE
	case batteryv1alpha1.ReplenishMinSizeThreshold:
		out.Type = poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD
	case batteryv1alpha1.ReplenishReplaceOnDelete:
		out.Type = poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE
	}
	return out
}

func hookPolicyFromBattery(p poolmgr.HookFailurePolicy) (batteryv1alpha1.HookFailurePolicy, error) {
	switch p {
	case poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE:
		return batteryv1alpha1.HookFailureDeleteAndReplace, nil
	case poolmgrv1.HookFailurePolicy_QUARANTINE:
		return batteryv1alpha1.HookFailureQuarantine, nil
	default:
		return "", fmt.Errorf("%w: hook failure policy %s", poolmgr.ErrInvalid, p)
	}
}

func hookPolicyToBattery(p batteryv1alpha1.HookFailurePolicy) poolmgr.HookFailurePolicy {
	switch p {
	case batteryv1alpha1.HookFailureDeleteAndReplace:
		return poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE
	case batteryv1alpha1.HookFailureQuarantine:
		return poolmgrv1.HookFailurePolicy_QUARANTINE
	default:
		return poolmgrv1.HookFailurePolicy_HOOK_FAILURE_POLICY_UNSPECIFIED
	}
}

// templateFromFlintlock is the MicroVMTemplate of a flintlock MicroVMSpec.
func templateFromFlintlock(t *types.MicroVMSpec) batteryv1alpha1.MicroVMTemplate {
	out := batteryv1alpha1.MicroVMTemplate{
		Provider:   clonePtr(t.Provider),
		VCPU:       t.GetVcpu(),
		MemoryInMb: t.GetMemoryInMb(),
		Metadata:   maps.Clone(t.GetMetadata()),
		Labels:     maps.Clone(t.GetLabels()),
	}
	if k := t.GetKernel(); k != nil {
		out.Kernel = batteryv1alpha1.Kernel{
			Image:            k.GetImage(),
			Cmdline:          maps.Clone(k.GetCmdline()),
			Filename:         clonePtr(k.Filename),
			AddNetworkConfig: k.GetAddNetworkConfig(),
		}
	}
	if i := t.GetInitrd(); i != nil {
		out.Initrd = &batteryv1alpha1.Initrd{Image: i.GetImage(), Filename: clonePtr(i.Filename)}
	}
	if v := t.GetRootVolume(); v != nil {
		out.RootVolume = volumeFromFlintlock(v)
	}
	for _, v := range t.GetAdditionalVolumes() {
		out.AdditionalVolumes = append(out.AdditionalVolumes, volumeFromFlintlock(v))
	}
	for _, n := range t.GetInterfaces() {
		out.Interfaces = append(out.Interfaces, interfaceFromFlintlock(n))
	}
	if c := t.GetCpuConfig(); c != nil {
		out.CPUConfig = &batteryv1alpha1.CPUConfig{
			FeaturesToEnable:         slices.Clone(c.GetFeaturesToEnable()),
			KVMCapabilitiesToDisable: slices.Clone(c.GetKvmCapabilitiesToDisable()),
		}
	}
	return out
}

func volumeFromFlintlock(v *types.Volume) batteryv1alpha1.Volume {
	return batteryv1alpha1.Volume{
		ID:              v.GetId(),
		ReadOnly:        v.GetIsReadOnly(),
		MountPoint:      clonePtr(v.MountPoint),
		PartitionID:     clonePtr(v.PartitionId),
		SizeInMb:        clonePtr(v.SizeInMb),
		ContainerSource: clonePtr(v.GetSource().ContainerSource),
		VirtiofsSource:  clonePtr(v.GetSource().VirtiofsSource),
	}
}

func interfaceFromFlintlock(n *types.NetworkInterface) batteryv1alpha1.NetworkInterface {
	out := batteryv1alpha1.NetworkInterface{
		DeviceID: n.GetDeviceId(),
		Type:     batteryv1alpha1.NetworkInterfaceMacvtap,
		GuestMAC: clonePtr(n.GuestMac),
	}
	if n.GetType() == types.NetworkInterface_TAP {
		out.Type = batteryv1alpha1.NetworkInterfaceTap
	}
	if a := n.GetAddress(); a != nil {
		out.Address = &batteryv1alpha1.StaticAddress{
			Address:     a.GetAddress(),
			Gateway:     clonePtr(a.Gateway),
			Nameservers: slices.Clone(a.GetNameservers()),
		}
	}
	if o := n.GetOverrides(); o != nil {
		out.Overrides = &batteryv1alpha1.NetworkOverrides{BridgeName: clonePtr(o.BridgeName)}
	}
	return out
}

// templateToFlintlock is the flintlock MicroVMSpec of a MicroVMTemplate,
// with allow_guest_agent set as the SpecBuilder sets it (PL-012).
func templateToFlintlock(t batteryv1alpha1.MicroVMTemplate) *types.MicroVMSpec {
	out := &types.MicroVMSpec{
		Labels:     maps.Clone(t.Labels),
		Vcpu:       t.VCPU,
		MemoryInMb: t.MemoryInMb,
		Kernel: &types.Kernel{
			Image:            t.Kernel.Image,
			Cmdline:          maps.Clone(t.Kernel.Cmdline),
			Filename:         clonePtr(t.Kernel.Filename),
			AddNetworkConfig: t.Kernel.AddNetworkConfig,
		},
		RootVolume:      volumeToFlintlock(t.RootVolume),
		Metadata:        maps.Clone(t.Metadata),
		Provider:        clonePtr(t.Provider),
		AllowGuestAgent: true,
	}
	if t.Initrd != nil {
		out.Initrd = &types.Initrd{Image: t.Initrd.Image, Filename: clonePtr(t.Initrd.Filename)}
	}
	for _, v := range t.AdditionalVolumes {
		out.AdditionalVolumes = append(out.AdditionalVolumes, volumeToFlintlock(v))
	}
	for _, n := range t.Interfaces {
		out.Interfaces = append(out.Interfaces, interfaceToFlintlock(n))
	}
	if c := t.CPUConfig; c != nil {
		out.CpuConfig = &types.CPUConfig{
			FeaturesToEnable:         slices.Clone(c.FeaturesToEnable),
			KvmCapabilitiesToDisable: slices.Clone(c.KVMCapabilitiesToDisable),
		}
	}
	return out
}

func volumeToFlintlock(v batteryv1alpha1.Volume) *types.Volume {
	return &types.Volume{
		Id:          v.ID,
		IsReadOnly:  v.ReadOnly,
		MountPoint:  clonePtr(v.MountPoint),
		PartitionId: clonePtr(v.PartitionID),
		SizeInMb:    clonePtr(v.SizeInMb),
		Source: &types.VolumeSource{
			ContainerSource: clonePtr(v.ContainerSource),
			VirtiofsSource:  clonePtr(v.VirtiofsSource),
		},
	}
}

func interfaceToFlintlock(n batteryv1alpha1.NetworkInterface) *types.NetworkInterface {
	out := &types.NetworkInterface{
		DeviceId: n.DeviceID,
		Type:     types.NetworkInterface_MACVTAP,
		GuestMac: clonePtr(n.GuestMAC),
	}
	if n.Type == batteryv1alpha1.NetworkInterfaceTap {
		out.Type = types.NetworkInterface_TAP
	}
	if a := n.Address; a != nil {
		out.Address = &types.StaticAddress{
			Address:     a.Address,
			Gateway:     clonePtr(a.Gateway),
			Nameservers: slices.Clone(a.Nameservers),
		}
	}
	if o := n.Overrides; o != nil {
		out.Overrides = &types.NetworkOverrides{BridgeName: clonePtr(o.BridgeName)}
	}
	return out
}

// clonePtr copies the value p points to, so that a converted spec shares no
// memory with its source.
func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
