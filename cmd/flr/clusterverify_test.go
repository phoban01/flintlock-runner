package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urfave/cli"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/verify"
)

// clusterConfigYAML is a cluster fleet's Runner configuration: the
// Kubernetes pool backend and no fleet section.
const clusterConfigYAML = `gitlab:
  url: https://gitlab.example.com
  token: glrt-not-a-real-token
  concurrent: 2
pool_manager:
  backend: kubernetes
  kubernetes:
    namespace: runners
profiles:
  - name: default
    arch: amd64
    kernel:
      image: registry.example.com/kernel:6.1
    rootfs: registry.example.com/rootfs:ubuntu
    pool:
      size: 1
`

// stubClusterVerifier answers with a fixed report and records what it was
// built with.
type stubClusterVerifier struct {
	report  *fleet.VerifyReport
	err     error
	timeout time.Duration
	cfg     *config.Config
}

func (s *stubClusterVerifier) Verify(context.Context) (*fleet.VerifyReport, error) {
	return s.report, s.err
}

func (s *stubClusterVerifier) seams() fleetSeams {
	return fleetSeams{ClusterVerifier: func(cfg *config.Config, timeout time.Duration, _ io.Writer) (clusterVerifier, error) {
		s.cfg, s.timeout = cfg, timeout
		return s, nil
	}}
}

func writeClusterConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(clusterConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFleetVerifyRoutesAClusterFleet checks that `fleet verify` of a
// configuration that selects the Kubernetes pool backend, which has no fleet
// section and no Inventory, runs the cluster verification with the default
// verification timeout, or the one --timeout names, and passes when it
// finds nothing wrong.
func TestFleetVerifyRoutesAClusterFleet(t *testing.T) {
	t.Parallel()
	path := writeClusterConfig(t)
	stub := &stubClusterVerifier{report: &fleet.VerifyReport{Hosts: []fleet.HostVerification{
		{Host: "host-a", Exercised: true, ClaimToReady: 1500 * time.Millisecond},
	}}}
	out, err := runFleetCommand(t, stub.seams(), path, "verify")
	if err != nil {
		t.Fatalf("fleet verify = %v\n%s", err, out)
	}
	if stub.cfg == nil || stub.cfg.PoolManager.Kubernetes.Namespace != "runners" {
		t.Fatalf("the cluster verifier was built with %+v, want the loaded configuration", stub.cfg)
	}
	if stub.timeout != config.DefaultVerificationTimeout {
		t.Errorf("timeout = %v, want the default %v", stub.timeout, config.DefaultVerificationTimeout)
	}
	for _, want := range []string{"host-a: creation to ready 1.5s", "verification passed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	if _, err := runFleetCommand(t, stub.seams(), path, "verify", "--timeout", "42s"); err != nil {
		t.Fatal(err)
	}
	if stub.timeout != 42*time.Second {
		t.Errorf("timeout = %v, want the one --timeout names", stub.timeout)
	}
}

//= docs/requirements/12-cluster-fleet.md#cluster-verification
//= type=test
//# If a Virtual Node is not ready or its verification pod does not
//# become ready within the verification timeout, then the verification
//# command SHALL exit with a non-zero status naming the Host and the
//# reason.

func TestFleetVerifyOfAClusterFleetExitsNonZeroNamingTheHost(t *testing.T) {
	t.Parallel()
	path := writeClusterConfig(t)
	stub := &stubClusterVerifier{report: &fleet.VerifyReport{
		Hosts: []fleet.HostVerification{{Host: "host-a"}, {Host: "host-b"}},
		Failures: []fleet.Failure{
			{Host: "host-a", Step: verify.StepVirtualNode, Err: errors.New("virtual node host-a-microvms is not ready: False (HostServiceDown)")},
			{Host: "host-b", Step: verify.StepPodReady, Err: errors.New("the verification pod was not ready within 1m0s")},
		},
	}}
	out, err := runFleetCommand(t, stub.seams(), path, "verify")
	var exit cli.ExitCoder
	if !errors.As(err, &exit) || exit.ExitCode() != exitFleetFailure {
		t.Fatalf("fleet verify = %v, want exit status %d\n%s", err, exitFleetFailure, out)
	}
	for _, want := range []string{
		"host-a: step virtual_node: virtual node host-a-microvms is not ready: False (HostServiceDown)",
		"host-b: step pod_ready: the verification pod was not ready within 1m0s",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("exit message does not name %q:\n%v", want, err)
		}
	}

	// A verification that cannot run at all exits non-zero too.
	stub = &stubClusterVerifier{err: errors.New("verify: no Virtual Node matches")}
	if _, err := runFleetCommand(t, stub.seams(), path, "verify"); !errors.As(err, &exit) || !strings.Contains(err.Error(), "no Virtual Node") {
		t.Errorf("fleet verify = %v, want a non-zero exit saying why", err)
	}
}

// TestNewClusterVerifierUsesTheRunnersKubeconfig checks that the production
// cluster verifier is built from the Runner's own Kubernetes client
// configuration, and that a verification that cannot reach the API server
// fails rather than passes.
func TestNewClusterVerifierUsesTheRunnersKubeconfig(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PoolManager: config.PoolManager{
			Backend:    config.PoolBackendKubernetes,
			Kubernetes: &config.KubernetesPools{Kubeconfig: path, Context: "fleet", Namespace: "runners"},
		},
		Profiles: []config.Profile{{Name: "default"}},
	}
	v, err := newClusterVerifier(cfg, time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "listing the Virtual Nodes") {
		t.Errorf("Verify against no API server = %v, want the listing failure", err)
	}

	cfg.PoolManager.Kubernetes.Kubeconfig = filepath.Join(t.TempDir(), "missing")
	if _, err := newClusterVerifier(cfg, time.Second, io.Discard); err == nil {
		t.Error("a missing kubeconfig was accepted")
	}
}
