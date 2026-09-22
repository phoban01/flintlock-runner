package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// deployDir is deploy/agent, three directories above this file's.
func deployDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "agent")
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Fleet Manifests SHALL run the Exec Agent in the Host Agent
//# on every Host.

//= docs/requirements/12-cluster-fleet.md#exec-agent
//= type=test
//# The Exec Agent SHALL reach `flintlockd` only through the local
//# endpoint of HI-042, and the Fleet Manifests SHALL run it as the user id
//# that HI-063 admits there and run no other container as that user id.

// TestHostAgentPatch reads deploy/agent/host-agent-patch.yaml strictly as
// the Host Agent's DaemonSet and checks that it adds the Exec Agent as
// `flr agent` with its configuration, running as the user id the Host Image
// admits to flintlockd and as no other, and that no other container of the
// patch runs as that user id. The configuration it reads, which the Host
// Agent's render.sh makes for each Host from
// deploy/host-agent/config/agent.yaml.tmpl, loads, with a local flintlockd,
// the user id the Host admits, its certificate where the patch mounts it and
// every Host Service.
func TestHostAgentPatch(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(deployDir(), "host-agent-patch.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(data, &ds); err != nil {
		t.Fatal(err)
	}
	if ds.Kind != "DaemonSet" || ds.Name != "flintlock-host-agent" || ds.Namespace != "flintlock-system" {
		t.Errorf("the patch targets %s %s/%s, want the Host Agent's DaemonSet", ds.Kind, ds.Namespace, ds.Name)
	}
	var agentContainer *corev1.Container
	all := append(append([]corev1.Container{}, ds.Spec.Template.Spec.InitContainers...), ds.Spec.Template.Spec.Containers...)
	for i := range all {
		c := &all[i]
		if c.Name == "exec-agent" {
			agentContainer = c
			continue
		}
		if c.SecurityContext != nil && c.SecurityContext.RunAsUser != nil && *c.SecurityContext.RunAsUser == DefaultFlintlockdUserID {
			t.Errorf("container %s runs as the user id flintlockd admits", c.Name)
		}
	}
	if agentContainer == nil {
		t.Fatal("the patch has no exec-agent container")
	}
	if len(agentContainer.Args) == 0 || agentContainer.Args[0] != "agent" {
		t.Errorf("the exec-agent container runs %v, want `flr agent`", agentContainer.Args)
	}
	sc := agentContainer.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != DefaultFlintlockdUserID || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Errorf("the exec-agent container's security context is %+v, want it to run as user %d and not as root", sc, DefaultFlintlockdUserID)
	}
	if pod := ds.Spec.Template.Spec.SecurityContext; pod != nil && pod.RunAsUser != nil && *pod.RunAsUser == DefaultFlintlockdUserID {
		t.Error("the pod's default user is the one flintlockd admits, so every container would run as it")
	}

	// The configuration the container reads is the one the Host Agent's
	// render init container makes for its Host from agent.yaml.tmpl.
	const rendered = "/etc/flr/rendered/agent.yaml"
	if !slices.Contains(agentContainer.Args, rendered) {
		t.Errorf("the exec-agent container runs %v, want it to read %s", agentContainer.Args, rendered)
	}
	var tlsDir string
	for _, m := range agentContainer.VolumeMounts {
		if m.Name == "exec-agent-tls" {
			tlsDir = m.MountPath
		}
	}

	cfg := renderAgentConfig(t, "10250")
	if cfg.FlintlockdUserID != DefaultFlintlockdUserID || cfg.Flintlockd != DefaultFlintlockd {
		t.Errorf("the rendered configuration has flintlockd %s and user %d", cfg.Flintlockd, cfg.FlintlockdUserID)
	}
	if cfg.TLS.CertFile != filepath.Join(tlsDir, "tls.crt") || cfg.TLS.KeyFile != filepath.Join(tlsDir, "tls.key") {
		t.Errorf("the rendered configuration reads its certificate from %s and %s, not the volume mounted at %q", cfg.TLS.CertFile, cfg.TLS.KeyFile, tlsDir)
	}
	if got, want := cfg.EnabledHostServices(), kubelabels.HostServiceNames(); !slices.Equal(got, want) {
		t.Errorf("the rendered configuration probes and publishes %v, want every Host Service %v", got, want)
	}
	if cfg.BridgeGateway != "10.200.0.1" {
		t.Errorf("the rendered bridge gateway is %q, want the sample Host's 10.200.0.1", cfg.BridgeGateway)
	}
	// A Host that admits another user id to flintlockd gets an agent that
	// knows it, and refuses to start as 10250 there.
	if cfg := renderAgentConfig(t, "20000"); cfg.FlintlockdUserID != 20000 {
		t.Errorf("on a Host that admits user 20000 the rendered flintlockd_user_id is %d", cfg.FlintlockdUserID)
	}
}

// renderAgentConfig runs deploy/host-agent/config/render.sh, as the Host
// Agent's render init container does, over the Exec Agent's template and
// every Host Service's fragment, for a sample Host whose host.env admits
// uid to flintlockd, and loads the configuration it writes.
func renderAgentConfig(t *testing.T, uid string) *Config {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run render.sh with")
	}
	hostAgent := filepath.Join(deployDir(), "..", "host-agent")
	templates := t.TempDir()
	sources := []string{
		filepath.Join(hostAgent, "config", "render.sh"),
		filepath.Join(hostAgent, "config", "agent.yaml.tmpl"),
	}
	fragments, err := filepath.Glob(filepath.Join(hostAgent, "services", "*", "agent.host-service.*.yaml"))
	if err != nil || len(fragments) == 0 {
		t.Fatalf("no Host Service fragments under %s: %v", hostAgent, err)
	}
	for _, src := range append(sources, fragments...) {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(templates, filepath.Base(src)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	hostEnv := filepath.Join(dir, "host.env")
	env := "FLR_GATEWAY=10.200.0.1\nFLR_POD_PROVIDER_UID=" + uid + "\n"
	if err := os.WriteFile(hostEnv, []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "rendered")
	cmd := exec.Command(sh, filepath.Join(templates, "render.sh"))
	cmd.Env = append(os.Environ(), "FLR_TEMPLATES="+templates, "FLR_HOST_ENV="+hostEnv, "FLR_OUT="+out)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("render.sh: %v\n%s", err, b)
	}
	cfg, err := LoadConfig(filepath.Join(out, "agent.yaml"), func(c *Config) { c.HostNode = "host-1" })
	if err != nil {
		t.Fatalf("the rendered configuration does not load: %v", err)
	}
	return cfg
}
