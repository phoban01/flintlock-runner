package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
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
// patch runs as that user id. The configuration it mounts loads, with a
// local flintlockd and the same user id.
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

	var cm corev1.ConfigMap
	data, err = os.ReadFile(filepath.Join(deployDir(), "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.UnmarshalStrict(data, &cm); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(cm.Data["agent.yaml"]), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, func(c *Config) { c.HostNode = "host-1" })
	if err != nil {
		t.Fatalf("the shipped configuration does not load: %v", err)
	}
	if cfg.FlintlockdUserID != DefaultFlintlockdUserID || cfg.Flintlockd != DefaultFlintlockd {
		t.Errorf("the shipped configuration has flintlockd %s and user %d", cfg.Flintlockd, cfg.FlintlockdUserID)
	}
}
