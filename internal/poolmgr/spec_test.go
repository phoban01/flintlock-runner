package poolmgr_test

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/protobuf/proto"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL build a Pool's MicroVM template from the Profile:
//# `vcpu`, `memory_in_mb`, `kernel`, `initrd` where set, `root_volume`,
//# `additional_volumes`, `interfaces`, `metadata` and `provider` where set.

// TestTemplateIsBuiltFromTheProfile checks every field of the template
// against the Profile it came from, including the three that are only
// present when the Profile sets them.
func TestTemplateIsBuiltFromTheProfile(t *testing.T) {
	t.Parallel()
	profile := withDefaults(config.Profile{
		Name:     "big",
		Arch:     config.ArchARM64,
		VCPU:     4,
		MemoryMB: 8192,
		Kernel: config.Kernel{
			Image:    "ghcr.io/example/kernel:6.6",
			Filename: "boot/vmlinux",
			Cmdline:  map[string]string{"console": "ttyS0"},
		},
		Initrd:   &config.Initrd{Image: "ghcr.io/example/initrd:v1", Filename: "initrd.img"},
		RootFS:   "ghcr.io/example/rootfs@sha256:" + strings.Repeat("a", 64),
		Provider: "cloudhypervisor",
		AdditionalVolumes: []config.Volume{{
			ID: "scratch", Image: "ghcr.io/example/scratch:v1",
			MountPoint: "/scratch", ReadOnly: true, SizeMB: 4096,
		}},
		Pool: config.PoolSettings{Size: 3},
	})

	spec := specFor(t, profile, "host-a")
	tmpl := templateOf(t, spec)

	if tmpl.GetVcpu() != 4 || tmpl.GetMemoryInMb() != 8192 {
		t.Errorf("vcpu/memory = %d/%d, want 4/8192", tmpl.GetVcpu(), tmpl.GetMemoryInMb())
	}
	if got := tmpl.GetKernel(); got.GetImage() != profile.Kernel.Image ||
		got.GetFilename() != profile.Kernel.Filename ||
		got.GetCmdline()["console"] != "ttyS0" {
		t.Errorf("kernel = %+v, want the profile's %+v", got, profile.Kernel)
	}
	if got := tmpl.GetInitrd(); got.GetImage() != profile.Initrd.Image || got.GetFilename() != profile.Initrd.Filename {
		t.Errorf("initrd = %+v, want the profile's %+v", got, profile.Initrd)
	}
	if got := tmpl.GetRootVolume().GetSource().GetContainerSource(); got != profile.RootFS {
		t.Errorf("root volume source = %q, want %q", got, profile.RootFS)
	}
	extra := tmpl.GetAdditionalVolumes()
	if len(extra) != 1 {
		t.Fatalf("additional volumes = %+v, want one", extra)
	}
	vol := profile.AdditionalVolumes[0]
	switch {
	case extra[0].GetId() != vol.ID:
		t.Errorf("volume id = %q, want %q", extra[0].GetId(), vol.ID)
	case extra[0].GetSource().GetContainerSource() != vol.Image:
		t.Errorf("volume source = %q, want %q", extra[0].GetSource().GetContainerSource(), vol.Image)
	case extra[0].GetMountPoint() != vol.MountPoint:
		t.Errorf("volume mount point = %q, want %q", extra[0].GetMountPoint(), vol.MountPoint)
	case !extra[0].GetIsReadOnly():
		t.Error("volume is not read only, want the profile's read_only")
	case extra[0].GetSizeInMb() != int32(vol.SizeMB):
		t.Errorf("volume size = %d, want %d", extra[0].GetSizeInMb(), vol.SizeMB)
	}
	if len(tmpl.GetInterfaces()) != 1 {
		t.Errorf("interfaces = %+v, want exactly one", tmpl.GetInterfaces())
	}
	if got := tmpl.GetProvider(); got != profile.Provider {
		t.Errorf("provider = %q, want %q", got, profile.Provider)
	}

	// The optional fields are absent when the Profile does not set them.
	plain := specFor(t, testProfile("plain", 1), "host-a")
	bare := templateOf(t, plain)
	if bare.Initrd != nil {
		t.Errorf("initrd = %+v for a profile with none, want nil", bare.Initrd)
	}
	if bare.Provider != nil {
		t.Errorf("provider = %q for a profile with none, want unset", bare.GetProvider())
	}
	if len(bare.GetAdditionalVolumes()) != 0 {
		t.Errorf("additional volumes = %+v for a profile with none, want none", bare.GetAdditionalVolumes())
	}
	if len(bare.GetMetadata()) != 0 {
		t.Errorf("metadata = %v for a profile with no user-data, want none", bare.GetMetadata())
	}
}

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL set the template's `namespace` to the Runner
//# namespace and SHALL leave its `id` empty for the Pool Manager to assign.

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# The Scheduler SHALL set `allow_guest_agent` in every Pool's MicroVM
//# template so that the `exec` Guest Transport works.

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL set `add_network_config` on the template's kernel
//# spec so that flintlock generates the guest network configuration from
//# the interfaces.

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL NOT use `eth0` as a network interface `device_id`.

// TestTemplateFixedFields covers the four fields that do not come from the
// Profile at all: the namespace and the empty id, the guest agent switch
// the exec Guest Transport needs, the kernel's add_network_config, and the
// device_id of the single interface, which Firecracker forbids from being
// eth0.
func TestTemplateFixedFields(t *testing.T) {
	t.Parallel()
	spec := specFor(t, testProfile("small", 1), "host-a")
	tmpl := templateOf(t, spec)

	if tmpl.GetNamespace() != testNamespace {
		t.Errorf("template namespace = %q, want %q", tmpl.GetNamespace(), testNamespace)
	}
	if tmpl.GetId() != "" {
		t.Errorf("template id = %q, want it left empty for the pool manager", tmpl.GetId())
	}
	if tmpl.Uid != nil {
		t.Errorf("template uid = %q, want it left unset", tmpl.GetUid())
	}
	if !tmpl.GetAllowGuestAgent() {
		t.Error("allow_guest_agent is false; the exec guest transport needs the vsock device")
	}
	if !tmpl.GetKernel().GetAddNetworkConfig() {
		t.Error("kernel.add_network_config is false; the guest would get no network configuration")
	}
	ifaces := tmpl.GetInterfaces()
	if len(ifaces) != 1 {
		t.Fatalf("interfaces = %+v, want exactly one", ifaces)
	}
	if got := ifaces[0].GetDeviceId(); got == "eth0" {
		t.Error("interface device_id is eth0, which firecracker reserves")
	} else if got != poolmgr.GuestDeviceID {
		t.Errorf("interface device_id = %q, want %q", got, poolmgr.GuestDeviceID)
	}
	if got := ifaces[0].GetType(); got != types.NetworkInterface_TAP {
		t.Errorf("interface type = %v, want TAP", got)
	}
	if poolmgr.GuestDeviceID == "eth0" {
		t.Error("GuestDeviceID is eth0")
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# The Scheduler SHALL label every Pool's MicroVM template with
//# `gitlab-runner.flintlock.dev/runner` set to the Runner name and
//# `gitlab-runner.flintlock.dev/profile` set to the Profile name.

// TestTemplateCarriesTheRunnerAndProfileLabels checks both labels, and that
// they follow the Runner name and the Profile name rather than being fixed.
func TestTemplateCarriesTheRunnerAndProfileLabels(t *testing.T) {
	t.Parallel()
	spec, err := poolmgr.NewSpecBuilder().Build(testProfile("builder", 1), poolmgr.SpecInput{
		RunnerName: "runner-seven",
		Namespace:  testNamespace,
		Hosts:      []string{"host-a"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	labels := templateOf(t, spec).GetLabels()
	if got := labels["gitlab-runner.flintlock.dev/runner"]; got != "runner-seven" {
		t.Errorf("runner label = %q, want %q", got, "runner-seven")
	}
	if got := labels["gitlab-runner.flintlock.dev/profile"]; got != "builder" {
		t.Errorf("profile label = %q, want %q", got, "builder")
	}
	if poolmgr.LabelRunner != "gitlab-runner.flintlock.dev/runner" || poolmgr.LabelProfile != "gitlab-runner.flintlock.dev/profile" {
		t.Errorf("label keys are %q and %q, want the two the specification names",
			poolmgr.LabelRunner, poolmgr.LabelProfile)
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# The Scheduler SHALL set the Pool's replenishment strategy to
//# immediate-on-lease unless the Profile's pool settings choose another.

// TestReplenishmentDefaultsToImmediateOnLease covers the default and each
// strategy a Profile can choose instead.
func TestReplenishmentDefaultsToImmediateOnLease(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		settings config.PoolSettings
		want     poolmgr.ReplenishmentStrategyType
		wantMin  *int32
		wantErr  bool
	}{
		{
			name:     "unset",
			settings: config.PoolSettings{Size: 1},
			want:     poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		},
		{
			name:     "immediate on lease",
			settings: config.PoolSettings{Size: 1, Strategy: config.ReplenishImmediateOnLease},
			want:     poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE,
		},
		{
			name:     "min size threshold",
			settings: config.PoolSettings{Size: 4, Strategy: config.ReplenishMinSizeThreshold, MinSize: ptr(2)},
			want:     poolmgrv1.ReplenishmentStrategyType_MIN_SIZE_THRESHOLD,
			wantMin:  ptr(int32(2)),
		},
		{
			name:     "replace on delete",
			settings: config.PoolSettings{Size: 1, Strategy: config.ReplenishReplaceOnDelete},
			want:     poolmgrv1.ReplenishmentStrategyType_REPLACE_ON_DELETE,
		},
		{
			name:     "unknown",
			settings: config.PoolSettings{Size: 1, Strategy: config.ReplenishmentStrategy("sometimes")},
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			profile := testProfile("p", 1)
			profile.Pool = tc.settings
			profile.Pool.Name, profile.Pool.Namespace = "p", testNamespace
			profile.Pool.HeartbeatInterval, profile.Pool.HeartbeatExpiry = time.Second, 3*time.Second

			spec, err := poolmgr.NewSpecBuilder().Build(profile, poolmgr.SpecInput{Namespace: testNamespace})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Build accepted strategy %q", tc.settings.Strategy)
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			if spec.Replenishment.Type != tc.want {
				t.Errorf("strategy = %v, want %v", spec.Replenishment.Type, tc.want)
			}
			switch {
			case tc.wantMin == nil && spec.Replenishment.MinSize != nil:
				t.Errorf("min size = %d, want none", *spec.Replenishment.MinSize)
			case tc.wantMin != nil && (spec.Replenishment.MinSize == nil || *spec.Replenishment.MinSize != *tc.wantMin):
				t.Errorf("min size = %v, want %d", spec.Replenishment.MinSize, *tc.wantMin)
			}
		})
	}
}

// TestPoolSettingsReachTheSpec checks the rest of what PL-010 derives from
// the Profile's pool settings: the size, the hooks, the hook failure policy
// and the heartbeat settings.
func TestPoolSettingsReachTheSpec(t *testing.T) {
	t.Parallel()
	profile := withDefaults(config.Profile{
		Name:     "hooked",
		Arch:     config.ArchARM64,
		VCPU:     1,
		MemoryMB: 512,
		Kernel:   config.Kernel{Image: "ghcr.io/example/kernel:6.6"},
		RootFS:   "ghcr.io/example/rootfs:v1",
		Pool: config.PoolSettings{
			Size:              5,
			CreateHooks:       []string{"cloud-init status --wait"},
			PreLeaseHooks:     []string{"rm -rf /builds/*"},
			HookFailurePolicy: config.HookFailureQuarantine,
			HeartbeatInterval: 7 * time.Second,
			HeartbeatExpiry:   21 * time.Second,
		},
	})
	spec := specFor(t, profile, "host-a")

	if spec.Size != 5 {
		t.Errorf("size = %d, want 5", spec.Size)
	}
	if len(spec.CreateCommands) != 1 || spec.CreateCommands[0] != profile.Pool.CreateHooks[0] {
		t.Errorf("create commands = %v, want %v", spec.CreateCommands, profile.Pool.CreateHooks)
	}
	if len(spec.PreLeaseCommands) != 1 || spec.PreLeaseCommands[0] != profile.Pool.PreLeaseHooks[0] {
		t.Errorf("pre-lease commands = %v, want %v", spec.PreLeaseCommands, profile.Pool.PreLeaseHooks)
	}
	if spec.HookFailurePolicy != poolmgrv1.HookFailurePolicy_QUARANTINE {
		t.Errorf("hook failure policy = %v, want QUARANTINE", spec.HookFailurePolicy)
	}
	if spec.HeartbeatInterval != 7*time.Second || spec.HeartbeatExpiryThreshold != 21*time.Second {
		t.Errorf("heartbeat interval/expiry = %s/%s, want 7s/21s", spec.HeartbeatInterval, spec.HeartbeatExpiryThreshold)
	}
}

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL base64-encode every value placed in the template's
//# `metadata` map and SHALL only use the keys `meta-data`, `user-data`,
//# `vendor-data` and `network-config`.

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# Where a Profile provides user-data, the Scheduler SHALL render it as a
//# template with the Profile name available and place the result in the
//# `user-data` entry.

// TestUserDataIsRenderedAndEncoded checks that a Profile's user-data is
// rendered with the Profile name available, lands base64-encoded under
// `user-data`, and that nothing else appears in the metadata map.
func TestUserDataIsRenderedAndEncoded(t *testing.T) {
	t.Parallel()
	profile := testProfile("ci-arm64", 1)
	profile.UserData = "#cloud-config\nhostname: {{ .Profile }}-guest\n"

	spec := specFor(t, profile, "host-a")
	metadata := templateOf(t, spec).GetMetadata()

	value, ok := metadata["user-data"]
	if !ok {
		t.Fatalf("metadata = %v, want a user-data entry", metadata)
	}
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatalf("user-data is not base64: %v", err)
	}
	want := "#cloud-config\nhostname: ci-arm64-guest\n"
	if string(decoded) != want {
		t.Errorf("user-data = %q, want %q", decoded, want)
	}
	allowed := map[string]bool{"meta-data": true, "user-data": true, "vendor-data": true, "network-config": true}
	for key := range metadata {
		if !allowed[key] {
			t.Errorf("metadata key %q is not one of the four cloud-init keys", key)
		}
	}
	if _, err := base64.StdEncoding.DecodeString(metadata["user-data"]); err != nil {
		t.Errorf("metadata value for user-data is not base64: %v", err)
	}
}

//= docs/requirements/04-pool-manager.md#microvm-template
//= type=test
//# The Scheduler SHALL NOT place any Job-specific value in the template,
//# because the template is instantiated before any Job exists.

// TestTemplateHasNothingJobSpecific is the guard on the one place a
// Job-specific value could be smuggled into a template: the user-data
// rendering. The data the template is rendered with carries the Profile
// name and nothing else, so a user-data template that reaches for a Job
// value fails to render instead of freezing a stale value into every
// MicroVM of the Pool. The rest of the guard is structural: Build's inputs
// are a Profile and a SpecInput, neither of which can carry a Job, so two
// builds of the same Profile produce byte-identical templates.
func TestTemplateHasNothingJobSpecific(t *testing.T) {
	t.Parallel()
	jobValues := []string{"{{ .JobID }}", "{{ .Job.Token }}", "{{ .CI_JOB_TOKEN }}"}
	for _, value := range jobValues {
		profile := testProfile("p", 1)
		profile.UserData = "#cloud-config\nruncmd: [echo " + value + "]\n"
		if _, err := poolmgr.NewSpecBuilder().Build(profile, poolmgr.SpecInput{Namespace: testNamespace}); err == nil {
			t.Errorf("Build accepted user-data referring to %s", value)
		}
	}

	profile := testProfile("p", 1)
	profile.UserData = "#cloud-config\nhostname: {{ .Profile }}\n"
	first := specFor(t, profile, "host-a")
	second := specFor(t, profile, "host-a")
	if !proto.Equal(first.Template, second.Template) {
		t.Errorf("two builds of one profile differ:\n%v\n%v", first.Template, second.Template)
	}
}

//= docs/requirements/04-pool-manager.md#pool-declaration
//= type=test
//# If two Runners would declare the same Pool name in the same namespace,
//# the Runner SHALL use its own namespace for its Pools so that they do not
//# collide.

// TestTwoRunnersDoNotCollide declares the same Profile as two Runners with
// different namespaces against one Pool Manager. Both Pools exist, each
// with its own warm MicroVMs, and claiming from one leaves the other
// untouched, which is what "do not collide" has to mean in practice.
func TestTwoRunnersDoNotCollide(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	pm := startPoolManager(t, ctx, poolmgr.FakeConfig{}, "host-a")
	c := pm.client()

	// The same Profile, with no namespace of its own, seen by two Runners.
	profile := config.Profile{
		Name:     "shared",
		Arch:     config.ArchARM64,
		VCPU:     1,
		MemoryMB: 512,
		Kernel:   config.Kernel{Image: "ghcr.io/example/kernel:6.6"},
		RootFS:   "ghcr.io/example/rootfs:v1",
		Pool: config.PoolSettings{
			Size:              1,
			HeartbeatInterval: 10 * time.Second,
			HeartbeatExpiry:   30 * time.Second,
		},
	}
	builder := poolmgr.NewSpecBuilder()
	specs := make([]poolmgr.PoolSpec, 0, 2)
	for _, namespace := range []string{"runner-one", "runner-two"} {
		spec, err := builder.Build(profile, poolmgr.SpecInput{
			RunnerName: namespace,
			Namespace:  namespace,
			Hosts:      []string{"host-a"},
		})
		if err != nil {
			t.Fatalf("Build for %s: %v", namespace, err)
		}
		if spec.Ref.Namespace != namespace {
			t.Fatalf("pool namespace = %q, want the runner's own %q", spec.Ref.Namespace, namespace)
		}
		if spec.Template.GetNamespace() != namespace {
			t.Errorf("template namespace = %q, want %q", spec.Template.GetNamespace(), namespace)
		}
		specs = append(specs, spec)
		pm.fillPool(c, spec)
	}
	if specs[0].Ref == specs[1].Ref {
		t.Fatalf("both runners declared %s", specs[0].Ref)
	}

	claim, err := c.ClaimVM(ctx, specs[0].Ref)
	if err != nil {
		t.Fatalf("ClaimVM from %s: %v", specs[0].Ref, err)
	}
	other, err := c.GetPool(ctx, specs[1].Ref)
	if err != nil {
		t.Fatalf("GetPool %s: %v", specs[1].Ref, err)
	}
	if other.Status.Available != 1 || other.Status.Leased != 0 {
		t.Errorf("the other runner's pool is %+v after a claim from %s, want it untouched",
			other.Status, specs[0].Ref)
	}
	if err := c.ReleaseVM(ctx, claim.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
}

// ptr returns a pointer to v, for the optional fields of the configuration
// and the proto.
func ptr[T any](v T) *T { return &v }
