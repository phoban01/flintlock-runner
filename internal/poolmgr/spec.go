package poolmgr

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"text/template"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// The two labels every Pool MicroVM template carries (PL-014).
const (
	// LabelRunner names the Runner that declared the Pool.
	LabelRunner = "gitlab-runner.flintlock.dev/runner"
	// LabelProfile names the Profile the Pool was derived from.
	LabelProfile = "gitlab-runner.flintlock.dev/profile"
)

// rootVolumeID is the id of the template's root volume. flintlock
// distinguishes the root volume by position, not by name, so the value only
// has to be stable.
const rootVolumeID = "root"

// Metadata keys the Pool Manager passes to flintlock's metadata service.
// PL-024 allows no others.
const (
	metaKeyMetaData      = "meta-data"
	metaKeyUserData      = "user-data"
	metaKeyVendorData    = "vendor-data"
	metaKeyNetworkConfig = "network-config"
)

// allowedMetadataKeys is the closed set of PL-024, in the order a spec
// renders them.
var allowedMetadataKeys = []string{metaKeyMetaData, metaKeyUserData, metaKeyVendorData, metaKeyNetworkConfig}

// userDataContext is what a Profile's user-data template is rendered with.
// It carries the Profile name and nothing else: a template that reaches for
// anything Job-specific fails to render rather than silently baking a stale
// value into every MicroVM of the Pool (PL-025, PL-026).
type userDataContext struct {
	// Profile is the Profile's name.
	Profile string
}

// specBuilder is the SpecBuilder implementation. It holds no state: a
// PoolSpec is a pure function of the Profile and the SpecInput.
type specBuilder struct{}

// NewSpecBuilder returns the SpecBuilder that derives a Pool from a Profile.
func NewSpecBuilder() SpecBuilder { return specBuilder{} }

//= docs/requirements/04-pool-manager.md#pool-declaration
//# When starting and on every configuration reload, the Scheduler SHALL
//# create or update one Pool per Profile, deriving the Pool's MicroVM
//# template from the Profile and the Pool's size, replenishment strategy,
//# hooks and heartbeat settings from the Profile's pool settings.

//= docs/requirements/04-pool-manager.md#pool-declaration
//# If two Runners would declare the same Pool name in the same namespace,
//# the Runner SHALL use its own namespace for its Pools so that they do not
//# collide.

// Build implements SpecBuilder. The Pool's identity is the Profile's Pool
// name, or the Profile name, in the Runner's own namespace, so that two
// Runners configured with the same Profiles declare two disjoint sets of
// Pools (PL-017); its size, replenishment strategy, hooks and heartbeat
// settings come from the Profile's pool settings, and its MicroVM template
// from the rest of the Profile (PL-020 to PL-026).
func (specBuilder) Build(p config.Profile, in SpecInput) (PoolSpec, error) {
	template, err := buildTemplate(p, in)
	if err != nil {
		return PoolSpec{}, err
	}
	strategy, err := replenishment(p.Pool)
	if err != nil {
		return PoolSpec{}, fmt.Errorf("profile %q: %w", p.Name, err)
	}
	policy, err := hookFailurePolicy(p.Pool.HookFailurePolicy)
	if err != nil {
		return PoolSpec{}, fmt.Errorf("profile %q: %w", p.Name, err)
	}
	return PoolSpec{
		Ref:                      poolRef(p, in.Namespace),
		Template:                 template,
		Size:                     int32(p.Pool.Size), //nolint:gosec // sizes are validated to be small and positive
		FlintlockHosts:           append([]string(nil), in.Hosts...),
		Replenishment:            strategy,
		CreateCommands:           append([]string(nil), p.Pool.CreateHooks...),
		PreLeaseCommands:         append([]string(nil), p.Pool.PreLeaseHooks...),
		HookFailurePolicy:        policy,
		HeartbeatInterval:        p.Pool.HeartbeatInterval,
		HeartbeatExpiryThreshold: p.Pool.HeartbeatExpiry,
	}, nil
}

// poolRef is the Pool's identity: the configured Pool name or the Profile
// name, in the configured Pool namespace or the Runner's own (CF-028,
// PL-017).
func poolRef(p config.Profile, namespace string) PoolRef {
	ref := PoolRef{Name: p.Pool.Name, Namespace: p.Pool.Namespace}
	if ref.Name == "" {
		ref.Name = p.Name
	}
	if ref.Namespace == "" {
		ref.Namespace = namespace
	}
	return ref
}

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL build a Pool's MicroVM template from the Profile:
//# `vcpu`, `memory_in_mb`, `kernel`, `initrd` where set, `root_volume`,
//# `additional_volumes`, `interfaces`, `metadata` and `provider` where set.

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL set the template's `namespace` to the Runner
//# namespace and SHALL leave its `id` empty for the Pool Manager to assign.

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL NOT place any Job-specific value in the template,
//# because the template is instantiated before any Job exists.

//= docs/requirements/04-pool-manager.md#pool-declaration
//# The Scheduler SHALL set `allow_guest_agent` in every Pool's MicroVM
//# template so that the `exec` Guest Transport works.

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL set `add_network_config` on the template's kernel
//# spec so that flintlock generates the guest network configuration from
//# the interfaces.

// buildTemplate renders the Profile as a flintlock MicroVMSpec. Its only
// inputs are the Profile and the SpecInput, neither of which can carry a
// Job: the template is instantiated by the Pool Manager long before a Job
// is known (PL-026). The id is left empty for the Pool Manager to assign
// and the namespace is the Runner's (PL-021); allow_guest_agent is set so
// that the exec Guest Transport has a channel into the guest (PL-012) and
// add_network_config so that flintlock generates the guest network
// configuration from the interfaces (PL-022).
func buildTemplate(p config.Profile, in SpecInput) (*types.MicroVMSpec, error) {
	metadata, err := buildMetadata(p)
	if err != nil {
		return nil, err
	}
	spec := &types.MicroVMSpec{
		// Id stays empty: the Pool Manager names every MicroVM it creates
		// from this template (PL-021).
		Namespace:  in.Namespace,
		Labels:     labels(p, in),
		Vcpu:       int32(p.VCPU),     //nolint:gosec // validated to be small and positive
		MemoryInMb: int32(p.MemoryMB), //nolint:gosec // validated to be small and positive
		Kernel: &types.Kernel{
			Image:   p.Kernel.Image,
			Cmdline: p.Kernel.Cmdline,
			// AddNetworkConfig makes flintlock generate the guest network
			// configuration from the interfaces below (PL-022).
			AddNetworkConfig: true,
		},
		RootVolume:        containerVolume(rootVolumeID, p.RootFS, "", false, 0),
		AdditionalVolumes: additionalVolumes(p.AdditionalVolumes),
		Interfaces:        guestInterfaces(),
		Metadata:          metadata,
		// AllowGuestAgent attaches the vsock device the exec Guest
		// Transport needs (PL-012).
		AllowGuestAgent: true,
	}
	if p.Kernel.Filename != "" {
		filename := p.Kernel.Filename
		spec.Kernel.Filename = &filename
	}
	if p.Initrd != nil {
		initrd := &types.Initrd{Image: p.Initrd.Image}
		if p.Initrd.Filename != "" {
			filename := p.Initrd.Filename
			initrd.Filename = &filename
		}
		spec.Initrd = initrd
	}
	if p.Provider != "" {
		provider := p.Provider
		spec.Provider = &provider
	}
	return spec, nil
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//# The Scheduler SHALL label every Pool's MicroVM template with
//# `gitlab-runner.flintlock.dev/runner` set to the Runner name and
//# `gitlab-runner.flintlock.dev/profile` set to the Profile name.

// labels are the two labels every Pool MicroVM carries, so that an operator
// looking at a Host can tell which Runner and which Profile a MicroVM
// belongs to.
func labels(p config.Profile, in SpecInput) map[string]string {
	return map[string]string{
		LabelRunner:  in.RunnerName,
		LabelProfile: p.Name,
	}
}

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL NOT use `eth0` as a network interface `device_id`.

// guestInterfaces is the template's single network interface: a TAP device
// on the Host's guest bridge, with DHCP addressing. Its device_id is
// GuestDeviceID and never `eth0`, which Firecracker reserves and which
// makes MicroVM creation fail with a misleading error.
func guestInterfaces() []*types.NetworkInterface {
	return []*types.NetworkInterface{{
		DeviceId: GuestDeviceID,
		Type:     types.NetworkInterface_TAP,
	}}
}

// additionalVolumes converts the Profile's extra volumes. Only volumes the
// Profile declares are attached (SE-003).
func additionalVolumes(vols []config.Volume) []*types.Volume {
	if len(vols) == 0 {
		return nil
	}
	out := make([]*types.Volume, 0, len(vols))
	for _, v := range vols {
		out = append(out, containerVolume(v.ID, v.Image, v.MountPoint, v.ReadOnly, v.SizeMB))
	}
	return out
}

// containerVolume builds one volume sourced from an OCI image.
func containerVolume(id, image, mountPoint string, readOnly bool, sizeMB int) *types.Volume {
	source := image
	vol := &types.Volume{
		Id:         id,
		IsReadOnly: readOnly,
		Source:     &types.VolumeSource{ContainerSource: &source},
	}
	if mountPoint != "" {
		mp := mountPoint
		vol.MountPoint = &mp
	}
	if sizeMB > 0 {
		size := int32(sizeMB) //nolint:gosec // validated to be small and positive
		vol.SizeInMb = &size
	}
	return vol
}

//= docs/requirements/04-pool-manager.md#microvm-template
//# The Scheduler SHALL base64-encode every value placed in the template's
//# `metadata` map and SHALL only use the keys `meta-data`, `user-data`,
//# `vendor-data` and `network-config`.

//= docs/requirements/04-pool-manager.md#microvm-template
//# Where a Profile provides user-data, the Scheduler SHALL render it as a
//# template with the Profile name available and place the result in the
//# `user-data` entry.

// buildMetadata renders the Profile's cloud-init data. The only entry a
// Profile can produce is user-data, rendered as a text/template with the
// Profile name available; every value is base64-encoded, as flintlock's
// metadata service expects, and no key outside the four allowed ones is
// ever written.
func buildMetadata(p config.Profile) (map[string]string, error) {
	if strings.TrimSpace(p.UserData) == "" {
		return nil, nil
	}
	tmpl, err := template.New("user-data").Option("missingkey=error").Parse(p.UserData)
	if err != nil {
		return nil, fmt.Errorf("profile %q: parsing user_data: %w", p.Name, err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, userDataContext{Profile: p.Name}); err != nil {
		return nil, fmt.Errorf("profile %q: rendering user_data: %w", p.Name, err)
	}
	return encodeMetadata(map[string]string{metaKeyUserData: buf.String()})
}

// encodeMetadata base64-encodes every value and rejects any key outside the
// allowed set, so that a later change cannot smuggle one past PL-024.
func encodeMetadata(entries map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(entries))
	for _, key := range sortedKeys(entries) {
		if !metadataKeyAllowed(key) {
			return nil, fmt.Errorf("poolmgr: metadata key %q is not one of %s", key, strings.Join(allowedMetadataKeys, ", "))
		}
		out[key] = base64.StdEncoding.EncodeToString([]byte(entries[key]))
	}
	return out, nil
}

// metadataKeyAllowed reports whether key is one of the four cloud-init keys
// PL-024 permits.
func metadataKeyAllowed(key string) bool {
	for _, allowed := range allowedMetadataKeys {
		if key == allowed {
			return true
		}
	}
	return false
}

// sortedKeys returns m's keys in order, so that an error names the first
// offending key deterministically.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//# The Scheduler SHALL set the Pool's replenishment strategy to
//# immediate-on-lease unless the Profile's pool settings choose another.

// replenishment maps the Profile's pool settings onto the proto strategy.
// An unset strategy is immediate-on-lease, which is what lets a Pool absorb
// a burst: every claim starts a replacement boot.
func replenishment(s config.PoolSettings) (ReplenishmentStrategy, error) {
	out := ReplenishmentStrategy{}
	switch s.Strategy {
	case "", config.ReplenishImmediateOnLease:
		out.Type = poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE
	case config.ReplenishMinSizeThreshold:
		out.Type = poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD
	case config.ReplenishReplaceOnDelete:
		out.Type = poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE
	default:
		return ReplenishmentStrategy{}, fmt.Errorf("unknown replenishment strategy %q", s.Strategy)
	}
	if out.Type == poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD && s.MinSize != nil {
		min := int32(*s.MinSize) //nolint:gosec // validated to be small and positive
		out.MinSize = &min
	}
	return out, nil
}

// hookFailurePolicy maps the Profile's hook failure policy onto the proto
// enum. An unset policy is delete-and-replace, which keeps a Pool of
// healthy MicroVMs without operator intervention.
func hookFailurePolicy(p config.HookFailurePolicy) (HookFailurePolicy, error) {
	switch p {
	case "", config.HookFailureDeleteAndReplace:
		return poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE, nil
	case config.HookFailureQuarantine:
		return poolmgrv1.HookFailurePolicy_QUARANTINE, nil
	default:
		return poolmgrv1.HookFailurePolicy_HOOK_FAILURE_POLICY_UNSPECIFIED, fmt.Errorf("unknown hook failure policy %q", p)
	}
}

// Compile-time interface check.
var _ SpecBuilder = specBuilder{}
