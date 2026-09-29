package main

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
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
// configuration that names none, and that pool_manager.backend: claim
// builds the claim pool backend from the named kubeconfig, never battery in
// its place, without needing the API server to be up, as the battery client
// needs no battery.
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
	if _, isClaim := battery.Client.(*claim.Backend); isClaim || battery.setProfiles != nil || battery.claim != nil {
		t.Errorf("no backend selected built %T, want the battery client", battery.Client)
	}

	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.PoolManager = config.PoolManager{
		Backend:  config.PoolBackendClaim,
		Endpoint: "127.0.0.1:1",
		TLS:      config.ClientTLS{Insecure: true},
		Claim:    &config.ClaimPools{Kubeconfig: path, Context: "fleet", HolderServiceAccount: "runner"},
	}
	claims, err := newPoolClient(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claims.Close() }()
	if _, isClaim := claims.Client.(*claim.Backend); !isClaim || claims.setProfiles == nil {
		t.Errorf("backend claim built %T, want the claim pool backend with a profile setter", claims.Client)
	}
	cfg.PoolManager.Claim.Kubeconfig = filepath.Join(t.TempDir(), "missing")
	if _, err := newPoolClient(cfg, log); err == nil {
		t.Error("a missing kubeconfig was accepted for the claim backend")
	}
}

// TestKubeNamespace checks the order the Pools' namespace is taken in.
func TestKubeNamespace(t *testing.T) {
	t.Parallel()
	inCluster := func(string) ([]byte, error) { return []byte("runner-pod-ns\n"), nil }
	outside := func(string) ([]byte, error) { return nil, errors.New("no such file") }

	if got := kubeNamespace("configured", "ci", inCluster); got != "configured" {
		t.Errorf("namespace = %q, want the configured one", got)
	}
	if got := kubeNamespace("", "ci", inCluster); got != "runner-pod-ns" {
		t.Errorf("namespace = %q, want the pod's own", got)
	}
	if got := kubeNamespace("", "ci", outside); got != "ci" {
		t.Errorf("namespace = %q, want the runner namespace", got)
	}
}
