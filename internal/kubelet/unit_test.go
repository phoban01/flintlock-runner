package kubelet

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// validConfig is a configuration that passes Validate.
func validConfig() *Config {
	cfg := &Config{
		HostNode:    "host-a",
		MaxMicroVMs: 8,
		TLS:         ServerTLS{CertFile: "cert.pem", KeyFile: "key.pem", ClientCAFile: "ca.pem"},
		Guard:       Guard{Namespace: "flintlock-system"},
	}
	cfg.ApplyDefaults()
	return cfg
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL reach `flintlockd` only through the
//# local endpoint of HI-042.

func TestFlintlockdEndpointHasToBeLocal(t *testing.T) {
	t.Parallel()
	cases := []struct {
		endpoint string
		ok       bool
	}{
		{"unix:///run/flintlock/flintlockd.sock", true},
		{"127.0.0.1:9090", true},
		{"[::1]:9090", true},
		{"127.8.8.8:9090", true},
		{"", false},
		{"unix://relative.sock", false},
		{"unix:/run/flintlock.sock", false},
		{"10.0.0.5:9090", false},
		{"0.0.0.0:9090", false},
		{"[::]:9090", false},
		{"localhost:9090", false},
		{"flintlockd.example.com:9090", false},
		{"dns:///127.0.0.1:9090", false},
		{"https://127.0.0.1:9090", false},
		{"127.0.0.1", false},
	}
	for _, tc := range cases {
		err := ValidateLocalEndpoint(tc.endpoint)
		if (err == nil) != tc.ok {
			t.Errorf("ValidateLocalEndpoint(%q) = %v, want ok=%v", tc.endpoint, err, tc.ok)
		}
	}

	t.Run("the configuration refuses it", func(t *testing.T) {
		cfg := validConfig()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("the valid configuration is refused: %v", err)
		}
		cfg.Flintlockd = "10.0.0.5:9090"
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "flintlockd:") {
			t.Fatalf("Validate = %v, want an error naming flintlockd", err)
		}
	})

	t.Run("the dialler refuses it", func(t *testing.T) {
		if _, err := DialLocal(context.Background(), "host-a", "10.0.0.5:9090"); err == nil {
			t.Fatal("DialLocal connected to an address that is not local")
		}
	})

	t.Run("a loopback endpoint is dialled and used", func(t *testing.T) {
		host := hostfake.New(flintlock.FakeHostConfig{Name: "host-a", SandboxRoot: t.TempDir(), ExecEnabled: true})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- host.Serve(ctx) }()
		t.Cleanup(func() { cancel(); <-done; _ = host.Close() })
		<-host.Ready()

		client, err := DialLocal(ctx, "host-a", host.Addr())
		if err != nil {
			t.Fatalf("DialLocal(%s): %v", host.Addr(), err)
		}
		t.Cleanup(func() { _ = client.Close() })
		pod := microVMPodForTest()
		spec, err := buildMicroVMSpec(pod, "ns", "")
		if err != nil {
			t.Fatal(err)
		}
		vm, err := client.CreateMicroVM(ctx, spec)
		if err != nil {
			t.Fatalf("CreateMicroVM: %v", err)
		}
		if err := client.DeleteMicroVM(ctx, vm.GetSpec().GetUid()); err != nil {
			t.Fatalf("DeleteMicroVM: %v", err)
		}
		if err := client.DeleteMicroVM(ctx, vm.GetSpec().GetUid()); !isNotFound(err) {
			t.Fatalf("second DeleteMicroVM = %v, want ErrNotFound", err)
		}
	})
}

// TestConfigAcceptsTheRunnersHostServiceNames checks that every Host
// Service name the Runner reads (kubelabels) is one the provider accepts.
func TestConfigAcceptsTheRunnersHostServiceNames(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.BridgeGateway = "10.200.0.1"
	cfg.HostServices = map[string]HostService{}
	for i, name := range kubelabels.HostServiceNames() {
		cfg.HostServices[name] = HostService{Enabled: true, Port: 3000 + i}
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate with every Host Service = %v", err)
	}
}

func TestConfigValidation(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*Config){
		"host_node":          func(c *Config) { c.HostNode = "" },
		"max_microvms":       func(c *Config) { c.MaxMicroVMs = 0 },
		"host_reserve.cpu":   func(c *Config) { c.HostReserve.CPU = "lots" },
		"tls.client_ca_file": func(c *Config) { c.TLS.ClientCAFile = "" },
		"tls.cert_file":      func(c *Config) { c.TLS.CertFile = "" },
		"guard.namespace":    func(c *Config) { c.Guard.Namespace = "" },
		"bridge_gateway": func(c *Config) {
			c.HostServices = map[string]HostService{kubelabels.HostServiceGoProxy: {Enabled: true, Port: 3000}}
		},
		"host_services.go_proxy.port": func(c *Config) {
			c.BridgeGateway = "10.200.0.1"
			c.HostServices = map[string]HostService{kubelabels.HostServiceGoProxy: {Enabled: true}}
		},
		// A name the Runner does not read, enabled or not, is a typo that
		// would publish a service nobody uses.
		"host_services.goproxy": func(c *Config) {
			c.BridgeGateway = "10.200.0.1"
			c.HostServices = map[string]HostService{"goproxy": {Enabled: true, Port: 3000}}
		},
		"host_services.registry": func(c *Config) {
			c.HostServices = map[string]HostService{"registry": {Enabled: false, Port: 5000}}
		},
	}
	for field, breakIt := range cases {
		cfg := validConfig()
		breakIt(cfg)
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), field+":") {
			t.Errorf("breaking %s: Validate = %v, want an error naming the field", field, err)
		}
	}

	t.Run("a file is loaded, defaulted and unknown keys refused", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "kubelet.yaml")
		body := "max_microvms: 4\nlease_duration: 90s\nnot_ready_dir: /tmp/reasons\ntls: {cert_file: c, key_file: k, client_ca_file: ca}\nguard: {namespace: flintlock-system}\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfigWith(path, func(c *Config) { c.HostNode = "from-flag" })
		if err != nil {
			t.Fatalf("LoadConfigWith: %v", err)
		}
		if cfg.HostNode != "from-flag" || cfg.LeaseDuration != 90*time.Second || cfg.NotReadyDir != "/tmp/reasons" ||
			cfg.Flintlockd != DefaultFlintlockd || cfg.DrainTimeout != DefaultDrainTimeout {
			t.Errorf("unexpected configuration: %+v", cfg)
		}
		// The Host's own kubelet holds 10250 in the network namespace the
		// Pod Provider shares with it.
		if cfg.Listen != ":10260" {
			t.Errorf("listen defaults to %q, want :10260", cfg.Listen)
		}
		if err := os.WriteFile(path, []byte(body+"surprise: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path); err == nil {
			t.Error("an unknown key was accepted")
		}
	})
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), flintlock.ErrNotFound.Error())
}

// microVMPodForTest is a pod of the only shape the provider supports.
func microVMPodForTest() *corev1.Pod {
	noToken := false
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-abc", Namespace: "runners", UID: "2f1c0b7e-0000-4000-8000-000000000001",
			Annotations: map[string]string{
				kubelabels.AnnotationKernelImage:   "ghcr.io/example/kernel:6.1",
				kubelabels.AnnotationKernelCmdline: "console=ttyS0 reboot=k ro",
				kubelabels.AnnotationHypervisor:    "firecracker",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &noToken,
			Containers: []corev1.Container{{
				Name:  "microvm",
				Image: "ghcr.io/example/rootfs:ubuntu",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("1500m"),
					corev1.ResourceMemory: resource.MustParse("2Gi"),
				}},
			}},
		},
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# When a pod is bound to the Virtual Node, the Pod Provider SHALL
//# create one MicroVM for it through `flintlockd`, using the image of the
//# pod's only container as the root filesystem image, the container's CPU
//# and memory limits as the MicroVM's vCPU count and memory, and the pod's
//# `gitlab-runner.flintlock.dev` annotations for the kernel image, the
//# kernel command line and the hypervisor.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# The Pod Provider SHALL label the MicroVM with the pod's UID,
//# namespace and name and SHALL set `allow_guest_agent` on it.

func TestPodBecomesMicroVMSpec(t *testing.T) {
	t.Parallel()
	pod := microVMPodForTest()
	spec, err := buildMicroVMSpec(pod, "flintlock-runner", "#cloud-config\n")
	if err != nil {
		t.Fatalf("buildMicroVMSpec: %v", err)
	}
	if got := spec.GetRootVolume().GetSource().GetContainerSource(); got != "ghcr.io/example/rootfs:ubuntu" {
		t.Errorf("root volume image = %q", got)
	}
	if spec.GetVcpu() != 2 || spec.GetMemoryInMb() != 2048 {
		t.Errorf("vcpu, memory = %d, %d; want 2 (1500m rounded up), 2048", spec.GetVcpu(), spec.GetMemoryInMb())
	}
	if spec.GetKernel().GetImage() != "ghcr.io/example/kernel:6.1" {
		t.Errorf("kernel image = %q", spec.GetKernel().GetImage())
	}
	cmdline := spec.GetKernel().GetCmdline()
	if len(cmdline) != 3 || cmdline["console"] != "ttyS0" || cmdline["reboot"] != "k" || cmdline["ro"] != "" {
		t.Errorf("kernel cmdline = %v", cmdline)
	}
	if spec.GetProvider() != "firecracker" {
		t.Errorf("provider = %q", spec.GetProvider())
	}
	if spec.GetInitrd() != nil {
		t.Errorf("an initrd was set for a pod that names none: %v", spec.GetInitrd())
	}
	withInitrd := microVMPodForTest()
	withInitrd.Annotations[annotationInitrdImage] = "ghcr.io/example/initrd:1"
	withInitrd.Annotations[annotationInitrdFilename] = "initrd.img"
	if s, err := buildMicroVMSpec(withInitrd, "ns", ""); err != nil || s.GetInitrd().GetImage() != "ghcr.io/example/initrd:1" || s.GetInitrd().GetFilename() != "initrd.img" {
		t.Errorf("initrd = %v, %v", s.GetInitrd(), err)
	}
	if spec.GetNamespace() != "flintlock-runner" {
		t.Errorf("namespace = %q", spec.GetNamespace())
	}
	labels := spec.GetLabels()
	if labels[vmLabelPodUID] != string(pod.UID) || labels[vmLabelPodNamespace] != "runners" || labels[vmLabelPodName] != "pool-abc" {
		t.Errorf("labels = %v", labels)
	}
	if !spec.GetAllowGuestAgent() {
		t.Error("allow_guest_agent is not set")
	}
	for _, iface := range spec.GetInterfaces() {
		if iface.GetDeviceId() == "eth0" {
			t.Error("the guest interface is eth0, which Firecracker reserves")
		}
	}
	userData, err := base64.StdEncoding.DecodeString(spec.GetMetadata()["user-data"])
	if err != nil || string(userData) != "#cloud-config\n" {
		t.Errorf("user-data = %q, %v", userData, err)
	}

	for field, breakIt := range map[string]func(*corev1.Pod){
		"limits.cpu":    func(p *corev1.Pod) { delete(p.Spec.Containers[0].Resources.Limits, corev1.ResourceCPU) },
		"limits.memory": func(p *corev1.Pod) { delete(p.Spec.Containers[0].Resources.Limits, corev1.ResourceMemory) },
		"kernel-image":  func(p *corev1.Pod) { delete(p.Annotations, kubelabels.AnnotationKernelImage) },
	} {
		broken := microVMPodForTest()
		breakIt(broken)
		if _, err := buildMicroVMSpec(broken, "ns", ""); err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("without %s: err = %v, want one naming it", field, err)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//= type=test
//# If a pod has more than one container, an init container, a
//# volume, a host namespace or a mounted service account token, then the Pod
//# Provider SHALL mark the pod failed with a reason naming the unsupported
//# field and SHALL NOT create a MicroVM for it.

func TestUnsupportedPodShapesNameTheField(t *testing.T) {
	t.Parallel()
	if err := validatePod(microVMPodForTest()); err != nil {
		t.Fatalf("the supported shape is refused: %v", err)
	}
	for field, breakIt := range unsupportedShapes() {
		pod := microVMPodForTest()
		breakIt(pod)
		err := validatePod(pod)
		if err == nil || !strings.HasPrefix(err.Error(), field+" ") {
			t.Errorf("%s: validatePod = %v, want an error naming the field", field, err)
		}
	}
}

// unsupportedShapes maps the field an error has to name to a mutation that
// makes a supported pod unsupported through that field.
func unsupportedShapes() map[string]func(*corev1.Pod) {
	return map[string]func(*corev1.Pod){
		"spec.containers": func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "busybox"})
		},
		"spec.initContainers": func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "init", Image: "busybox"}}
		},
		"spec.volumes": func(p *corev1.Pod) {
			p.Spec.Volumes = []corev1.Volume{{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		},
		"spec.hostNetwork":                  func(p *corev1.Pod) { p.Spec.HostNetwork = true },
		"spec.hostPID":                      func(p *corev1.Pod) { p.Spec.HostPID = true },
		"spec.hostIPC":                      func(p *corev1.Pod) { p.Spec.HostIPC = true },
		"spec.automountServiceAccountToken": func(p *corev1.Pod) { p.Spec.AutomountServiceAccountToken = nil },
	}
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# If a unit of the Host Image reports a not ready reason, then the
//# Pod Provider SHALL report the Virtual Node not ready with that reason in
//# the condition's message.

func TestNotReadyReasonsContract(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "not-ready.d")
	if reasons, err := ReadNotReadyReasons(dir); err != nil || len(reasons) != 0 {
		t.Fatalf("an absent directory: %v, %v; want nothing", reasons, err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("flr-kvm-check.service", "KVM is unavailable: /dev/kvm is absent\nsecond line\n")
	write("flr-bridge.service", "")
	write(".flr-thinpool.service.tmp", "half written")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	reasons, err := ReadNotReadyReasons(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []NotReadyReason{
		{Unit: "flr-bridge.service", Reason: "not ready"},
		{Unit: "flr-kvm-check.service", Reason: "KVM is unavailable: /dev/kvm is absent"},
	}
	if len(reasons) != len(want) || reasons[0] != want[0] || reasons[1] != want[1] {
		t.Fatalf("reasons = %+v, want %+v", reasons, want)
	}

	host := hostfake.New(flintlock.FakeHostConfig{Name: "h", SandboxRoot: t.TempDir(), ExecEnabled: true})
	t.Cleanup(func() { _ = host.Close() })
	cfg := validConfig()
	cfg.NotReadyDir = dir
	got := checkReadiness(context.Background(), cfg, host.Client())
	if got.ready || got.reason != reasonHostImageNotReady || !strings.Contains(got.message, "KVM is unavailable: /dev/kvm is absent") {
		t.Fatalf("readiness = %+v, want not ready with the unit's reason in the message", got)
	}
}

//= docs/requirements/12-cluster-fleet.md#virtual-node
//= type=test
//# The Pod Provider SHALL report the Virtual Node ready only while
//# the local `flintlockd` answers `ServerInfo` with the exec service enabled
//# and every enabled Host Service accepts connections on the bridge gateway
//# address.

func TestReadinessChecks(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	_, portText, _ := net.SplitHostPort(listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	newCfg := func() *Config {
		cfg := validConfig()
		cfg.NotReadyDir = filepath.Join(t.TempDir(), "none")
		cfg.BridgeGateway = "127.0.0.1"
		cfg.HostServices = map[string]HostService{
			kubelabels.HostServiceGoProxy:        {Enabled: true, Port: port},
			kubelabels.HostServiceRegistryMirror: {Enabled: false, Port: 1}, // disabled: never probed
		}
		return cfg
	}
	newHost := func(cfg flintlock.FakeHostConfig) *hostfake.Host {
		cfg.Name, cfg.SandboxRoot = "h", t.TempDir()
		h := hostfake.New(cfg)
		t.Cleanup(func() { _ = h.Close() })
		return h
	}
	ctx := context.Background()

	if got := checkReadiness(ctx, newCfg(), newHost(flintlock.FakeHostConfig{ExecEnabled: true}).Client()); !got.ready {
		t.Errorf("healthy host: %+v, want ready", got)
	}
	if got := checkReadiness(ctx, newCfg(), newHost(flintlock.FakeHostConfig{ExecEnabled: false}).Client()); got.ready || got.reason != reasonExecDisabled {
		t.Errorf("exec disabled: %+v", got)
	}
	if got := checkReadiness(ctx, newCfg(), newHost(flintlock.FakeHostConfig{ExecEnabled: true, ServerInfoUnimplemented: true}).Client()); got.ready || got.reason != reasonFlintlockdNotReady {
		t.Errorf("ServerInfo unimplemented: %+v", got)
	}
	closed := newHost(flintlock.FakeHostConfig{ExecEnabled: true})
	client := closed.Client()
	_ = closed.Close()
	if got := checkReadiness(ctx, newCfg(), client); got.ready || got.reason != reasonFlintlockdNotReady {
		t.Errorf("flintlockd down: %+v", got)
	}

	_ = listener.Close()
	got := checkReadiness(ctx, newCfg(), newHost(flintlock.FakeHostConfig{ExecEnabled: true}).Client())
	if got.ready || got.reason != reasonHostServiceDown || !strings.Contains(got.message, kubelabels.HostServiceGoProxy) {
		t.Errorf("host service down: %+v", got)
	}
}
