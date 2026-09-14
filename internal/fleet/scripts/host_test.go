package scripts

import (
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// section returns the text of script from the first from up to the next to.
func section(t *testing.T, script, from, to string) string {
	t.Helper()
	i := strings.Index(script, from)
	if i < 0 {
		t.Fatalf("script lacks %q", from)
	}
	rest := script[i:]
	if to == "" {
		return rest
	}
	j := strings.Index(rest[len(from):], to)
	if j < 0 {
		t.Fatalf("script lacks %q after %q", to, from)
	}
	return rest[:len(from)+j]
}

// before fails unless a occurs in script, and before b.
func before(t *testing.T, script, a, b string) {
	t.Helper()
	i, j := strings.Index(script, a), strings.Index(script, b)
	if i < 0 || j < 0 || i > j {
		t.Errorf("want %q (at %d) before %q (at %d)", a, i, b, j)
	}
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL provision each instance by running
//# the flintlock host provisioner in unattended mode, installing containerd,
//# Firecracker, Cloud Hypervisor and `flintlockd` at the versions pinned in the
//# configuration.

func TestFlintlockStepRunsPinnedProvisionerUnattended(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepFlintlock, fullInput())
	mustContain(t, s,
		"export FLINTLOCK='v0.14.0'",
		"export CONTAINERD='v1.7.22'",
		"export FIRECRACKER='v1.10.1'",
		"export CLOUD_HYPERVISOR='v41.0'",
		`"${provisioner[@]}" all --unattended`,
	)
	// The versions are checked after the provisioner ran.
	before(t, s, `"${provisioner[@]}" all --unattended`, `after provisioning, want $want`)
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL use the `flintlock-provision` binary
//# when the pinned flintlock release ships it and SHALL fall back to the
//# `provision.sh` script from the same release otherwise.

func TestFlintlockStepFallsBackToProvisionScriptOnlyWhenNotShipped(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepFlintlock, fullInput())
	mustContain(t, s,
		`release="https://github.com/liquidmetal-dev/flintlock/releases/download/$FLINTLOCK"`,
		`"$release/flintlock-provision_$FLR_ARCH"`)
	shipped := section(t, s, "200)\n", ";;")
	mustContain(t, shipped, `provisioner=("$work/flintlock-provision")`)
	notShipped := section(t, s, "404)\n", ";;")
	// The fallback is provision.sh from the same release tag.
	mustContain(t, notShipped,
		`https://raw.githubusercontent.com/liquidmetal-dev/flintlock/$FLINTLOCK/hack/scripts/provision.sh`,
		`provisioner=(bash "$work/provision.sh")`)
	// Any other status is an error, not a fallback.
	other := section(t, s, "*)\n  die \"downloading flintlock-provision", "esac")
	mustNotContain(t, other, "provision.sh")
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL create the containerd devicemapper
//# thin pool on the block device named in the configuration.

func TestThinPoolStepCreatesPoolOnConfiguredDevice(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepThinPool, fullInput())
	mustContain(t, s,
		"vg='flintlock'",
		"dev='/dev/nvme1n1'",
		`pvcreate -q "$dev"`,
		`vgcreate -q "$vg" "$dev"`,
		`--thinpool "$vg/thinpool" --poolmetadata "$vg/thinpoolmeta"`,
	)
	// The provisioner is pointed at the same device and pool name.
	f := render(t, fleet.StepFlintlock, fullInput())
	mustContain(t, f, "--disk '/dev/nvme1n1'", "--thinpool 'flintlock'")
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# If the configured thin pool already exists on an instance, then
//# the Fleet Controller SHALL NOT recreate it or wipe its device.

func TestThinPoolStepNeverRecreatesOrWipes(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepThinPool, fullInput())
	guard := section(t, s, `if lvs --noheadings -o lv_attr "$vg/thinpool"`, "fi")
	mustContain(t, guard, "exit 0")
	before(t, s, `if lvs --noheadings -o lv_attr "$vg/thinpool"`, "pvcreate")
	// A device with any signature is refused before pvcreate touches it.
	before(t, s, `die "$dev is not blank`, `pvcreate -q "$dev"`)
	mustNotContain(t, s, "wipefs", "--wipesignatures", "mkfs", "dd if=", "pvcreate -f", "pvcreate -y")
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL configure `flintlockd` to listen on
//# the instance's private address on the configured port, to require the
//# configured basic auth token and to serve TLS with the configured
//# certificates.

func TestFlintlockdListensOnPrivateAddressWithTokenAndTLS(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Fleet.Flintlockd.Port = 9443
	s := render(t, fleet.StepFlintlockd, in)
	mustContain(t, s,
		"FLR_ADDRESS='10.0.1.10'",
		`echo "grpc-endpoint: \"$FLR_ADDRESS:9443\""`,
		"token=$(secret flintlockd_token '/flr/host-token')",
		`printf "basic-auth-token: '%s'\n"`,
		"echo 'insecure: false'",
		"echo 'tls-cert: /etc/flintlock-runner/tls/host.pem'",
		"echo 'tls-key: /etc/flintlock-runner/tls/host.key'",
		"cert=$(secret tls_cert '/flr/tls-cert')",
		"key=$(secret tls_key '/flr/tls-key')",
		"flintlockd_config /etc/opt/flintlockd/config.yaml",
	)
	// The token itself never appears in the script (SE-014).
	mustNotContain(t, s, string(in.Fleet.Flintlockd.Token))
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL enable the `flintlockd` exec API on
//# every Host and SHALL enable the SSH proxy API where any Profile uses the
//# proxied `ssh` Guest Transport.

func TestFlintlockdGuestAgentAPIs(t *testing.T) {
	t.Parallel()
	exec := render(t, fleet.StepFlintlockd, fullInput())
	mustContain(t, exec, "echo 'enable-exec-api: true'", "echo 'enable-ssh-proxy-api: false'")
	ssh := render(t, fleet.StepFlintlockd, sparseInput())
	mustContain(t, ssh, "echo 'enable-exec-api: true'", "echo 'enable-ssh-proxy-api: true'")
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL NOT start `flintlockd` in insecure
//# mode unless the configuration explicitly requests it.

func TestFlintlockdInsecureOnlyWhenRequested(t *testing.T) {
	t.Parallel()
	for _, step := range []fleet.Step{fleet.StepFlintlock, fleet.StepFlintlockd} {
		s := render(t, step, fullInput())
		mustNotContain(t, s, "insecure: true", "--insecure", " -k ")
		mustContain(t, s, "echo 'insecure: false'")
	}
	s := render(t, fleet.StepFlintlockd, sparseInput())
	mustContain(t, s, "echo 'insecure: true'")
	mustNotContain(t, s, "tls-cert:")
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL select binaries and images matching
//# the instance's architecture.

func TestArchitectureSelectsBinariesAndImages(t *testing.T) {
	t.Parallel()
	arm := render(t, fleet.StepPrepull, fullInput())
	mustContain(t, arm, "FLR_ARCH='arm64'",
		"pull_image 'arm64' 'ghcr.io/example/kernel:6.1-arm64'",
		`[ -z "$arch" ] || [ "$arch" = "$FLR_ARCH" ] || return 0`,
		`--platform "linux/$FLR_ARCH"`)
	mustNotContain(t, arm, "kernel:6.1-amd64", "rootfs-go:1.25-amd64")

	in := fullInput()
	in.Instance.Arch = config.ArchAMD64
	amd := render(t, fleet.StepPrepull, in)
	mustContain(t, amd, "FLR_ARCH='amd64'", "pull_image 'amd64' 'ghcr.io/example/kernel:6.1-amd64'")
	mustNotContain(t, amd, "kernel:6.1-arm64")
	f := render(t, fleet.StepFlintlock, in)
	mustContain(t, f, "FLR_ARCH='amd64'", `flintlock-provision_$FLR_ARCH`)
	// A Host whose kernel disagrees with the discovered architecture is
	// refused.
	mustContain(t, f, `[ "$FLR_ARCH" = "$(host_arch)" ] || die`)

	in.Instance.Arch = "riscv64"
	sc, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Render(fleet.StepFlintlock, in); err == nil {
		t.Error("rendered for an unsupported architecture")
	}
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL pre-pull the kernel and root
//# filesystem images of every Profile into the flintlock containerd namespace
//# on each Host after provisioning.

func TestPrepullPullsEveryProfileImageIntoFlintlockNamespace(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Profiles = append(in.Profiles, config.Profile{
		Name: "with-initrd", Arch: config.ArchARM64,
		Kernel: config.Kernel{Image: "ghcr.io/example/kernel:6.6"}, Initrd: &config.Initrd{Image: "ghcr.io/example/initrd:1"},
		RootFS: "ghcr.io/example/rootfs-base:2",
	})
	s := render(t, fleet.StepPrepull, in)
	mustContain(t, s,
		"ctr_ns=(ctr --address '/run/containerd/containerd.sock' --namespace 'flintlock')",
		"pull_image 'arm64' 'ghcr.io/example/kernel:6.1-arm64'",
		"pull_image 'arm64' 'ghcr.io/example/rootfs-go:1.25@sha256:",
		"pull_image 'arm64' 'ghcr.io/example/tools:1'",
		"pull_image 'arm64' 'ghcr.io/example/kernel:6.6'",
		"pull_image 'arm64' 'ghcr.io/example/initrd:1'",
		"pull_image 'arm64' 'ghcr.io/example/rootfs-base:2'",
		`"${ctr_ns[@]}" images pull`,
	)
	if n := strings.Count(s, "\npull_image "); n != 6 {
		t.Errorf("pull_image lines = %d, want 6", n)
	}
}

//= docs/requirements/06-fleet.md#host-provisioning
//= type=test
//# The Fleet Controller SHALL record the installed versions of
//# flintlock, Firecracker, Cloud Hypervisor and containerd in the Inventory.

func TestDetectReportsInstalledVersions(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepDetect, fullInput())
	for _, c := range []string{"flintlock", "firecracker", "cloud_hypervisor", "containerd"} {
		mustContain(t, s, "printf '::version:: "+c+` %s\n' "${FLR_INSTALLED[`+c+`]}"`)
	}
	out := ParseOutput("noise\n::version:: flintlock v0.14.0\n::version:: firecracker v1.10.1\n" +
		"::version:: cloud_hypervisor v41.0\n::version:: containerd none\n::thinpool:: present\n")
	want := config.InstalledVersions{Flintlock: "v0.14.0", Firecracker: "v1.10.1", CloudHypervisor: "v41.0"}
	if out.Versions != want || !out.ThinPool {
		t.Errorf("parsed %+v thinpool %v, want %+v", out.Versions, out.ThinPool, want)
	}
}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# If provisioning finds no usable `/dev/kvm` on an instance, then
//# the Fleet Controller SHALL exclude it from the Inventory and report it as
//# unsupported because KVM is unavailable.

func TestDetectChecksKVMIsUsable(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepDetect, fullInput())
	check := section(t, s, "kvm_status() {", "\n}\n")
	mustContain(t, check,
		"if [ ! -e /dev/kvm ]; then\n    echo '/dev/kvm does not exist'",
		"elif [ ! -c /dev/kvm ]; then\n    echo '/dev/kvm is not a character device'",
		// Root, which flintlockd runs as, has to be able to open it for
		// reading and writing: a node with no KVM driver behind it fails.
		"elif ! { : <>/dev/kvm; } 2>/dev/null; then\n    echo '/dev/kvm cannot be opened for reading and writing'",
		"else\n    echo ok\n",
	)
	// The check installs nothing.
	mustNotContain(t, check, "ensure_packages", "apt-get", "modprobe", "changed ")
	// detect reports the verdict, before everything else it does.
	mustContain(t, s, "kvm=$(kvm_status)\nif [ \"$kvm\" = ok ]; then\n  echo '::kvm:: ok'\nelse\n  printf '::kvm:: unavailable %s\\n' \"$kvm\"\nfi\n")
	if strings.Index(s, "kvm=$(kvm_status)") > strings.Index(s, "\ninstalled_versions\n") {
		t.Error("the KVM check runs after the version detection")
	}

	for stdout, want := range map[string]Output{
		"::kvm:: ok\n": {KVM: true},
		"::kvm:: unavailable /dev/kvm does not exist\n": {KVMUnavailable: "/dev/kvm does not exist"},
		"::thinpool:: absent\n":                         {},
	} {
		got := ParseOutput(stdout)
		if got.KVM != want.KVM || got.KVMUnavailable != want.KVMUnavailable {
			t.Errorf("ParseOutput(%q) kvm = %v %q, want %v %q", stdout, got.KVM, got.KVMUnavailable, want.KVM, want.KVMUnavailable)
		}
	}
}

//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
//= type=test
//# The Fleet Controller SHALL compute each Host's capacity as the
//# instance's vCPU and memory minus the configured Host reserve.

func TestDetectReportsHostCapacity(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepDetect, fullInput())
	mustContain(t, s,
		"vcpu=$(getconf _NPROCESSORS_ONLN 2>/dev/null || nproc)",
		"if [ \"$key\" = MemTotal: ]; then\n    mem_kib=$value",
		"done </proc/meminfo",
		`printf '::capacity:: vcpu %s\n' "$vcpu"`,
		`printf '::capacity:: memory_mb %s\n' "$((mem_kib / 1024))"`,
	)
	out := ParseOutput("::capacity:: vcpu 16\n::capacity:: memory_mb 31536\n::capacity:: vcpu x\n")
	if out.VCPU != 16 || out.MemoryMB != 31536 {
		t.Errorf("parsed capacity %d vCPU %d MB, want 16 and 31536", out.VCPU, out.MemoryMB)
	}
}

// TestScriptsCarryNoSecrets renders every step with secrets configured and
// checks none of them is in any script (SE-014).
func TestScriptsCarryNoSecrets(t *testing.T) {
	t.Parallel()
	in := fullInput()
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range s.All() {
		sc, err := s.Render(step, in)
		if err != nil {
			t.Fatal(err)
		}
		mustNotContain(t, sc.Content, string(in.Fleet.Flintlockd.Token))
	}
}

func TestRenderRejectsUnsafeValues(t *testing.T) {
	t.Parallel()
	s, err := New()
	if err != nil {
		t.Fatal(err)
	}
	in := fullInput()
	in.HostServices.HTTPCache.Upstreams[0].Name = "npm\nrm -rf /"
	if _, err := s.Render(fleet.StepHostServices, in); err == nil {
		t.Error("rendered an upstream name with a newline")
	}
	in = fullInput()
	in.Inventory.Hosts[1].Endpoint = "peer.example.com:9090"
	if _, err := s.Render(fleet.StepNetworking, in); err == nil {
		t.Error("rendered a peer that cannot go into the firewall")
	}
	if _, err := s.Render("nope", fullInput()); err == nil {
		t.Error("rendered an unknown step")
	}
	// A quoted value cannot escape its quotes.
	in = fullInput()
	in.Fleet.ThinPoolDevice = "/dev/x'; reboot; '"
	sc, err := s.Render(fleet.StepThinPool, in)
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, sc.Content, `dev='/dev/x'\''; reboot; '\'''`)
}
