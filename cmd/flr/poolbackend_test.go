package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

const testKubeconfig = `apiVersion: v1
kind: Config
current-context: fleet
clusters:
  - name: fleet
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: fleet
    context:
      cluster: fleet
      user: runner
users:
  - name: runner
    user:
      token: not-a-real-token
`

// TestPoolBackendSelection checks that battery stays the backend of a
// configuration that names none, and that pool_manager.backend: kubernetes
// builds the Kubernetes pool backend from the named kubeconfig, without
// needing the API server to be up, as the battery client needs no battery.
func TestPoolBackendSelection(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := &config.Config{
		GitLab:      config.GitLab{Name: "runner-a"},
		PoolManager: config.PoolManager{Endpoint: "127.0.0.1:1", TLS: config.ClientTLS{Insecure: true}},
		Scheduler:   config.Scheduler{Namespace: "ci"},
	}
	battery, err := newPoolClient(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = battery.Close() }()
	if _, isKube := battery.Client.(*kube.Backend); isKube || battery.setProfiles != nil {
		t.Errorf("no backend selected built %T, want the battery client", battery.Client)
	}

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.PoolManager = config.PoolManager{
		Backend:    config.PoolBackendKubernetes,
		Kubernetes: &config.KubernetesPools{Kubeconfig: path, Context: "fleet", Namespace: "runners"},
	}
	k8s, err := newPoolClient(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = k8s.Close() }()
	if _, isKube := k8s.Client.(*kube.Backend); !isKube || k8s.setProfiles == nil {
		t.Errorf("backend kubernetes built %T, want the kubernetes pool backend with a profile setter", k8s.Client)
	}

	cfg.PoolManager.Kubernetes.Kubeconfig = filepath.Join(t.TempDir(), "missing")
	if _, err := newPoolClient(cfg, log); err == nil {
		t.Error("a missing kubeconfig was accepted")
	}
}

// TestKubeNamespace checks the order the Pools' namespace is taken in.
func TestKubeNamespace(t *testing.T) {
	t.Parallel()
	inCluster := func(string) ([]byte, error) { return []byte("runner-pod-ns\n"), nil }
	outside := func(string) ([]byte, error) { return nil, errors.New("no such file") }

	if got := kubeNamespace(config.KubernetesPools{Namespace: "configured"}, "ci", inCluster); got != "configured" {
		t.Errorf("namespace = %q, want the configured one", got)
	}
	if got := kubeNamespace(config.KubernetesPools{}, "ci", inCluster); got != "runner-pod-ns" {
		t.Errorf("namespace = %q, want the pod's own", got)
	}
	if got := kubeNamespace(config.KubernetesPools{}, "ci", outside); got != "ci" {
		t.Errorf("namespace = %q, want the runner namespace", got)
	}
}
