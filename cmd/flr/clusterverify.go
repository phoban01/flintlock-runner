package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/urfave/cli"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/verify"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// clusterVerifier is `fleet verify` for a cluster fleet (KF-100 to
// KF-102); verify.Cluster is the production one.
type clusterVerifier interface {
	Verify(ctx context.Context) (*fleet.VerifyReport, error)
}

// selectsKubernetes reports whether the configuration at path selects the
// Kubernetes pool backend. It reads that one key and nothing else, because
// a cluster fleet's configuration has no fleet section and the fleet
// subcommands' own loading would refuse it before verification could tell
// the two fleets apart.
func selectsKubernetes(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, cli.NewExitError(fmt.Sprintf("config: read %s: %v", path, err), exitInvalidConfig)
	}
	var doc struct {
		PoolManager struct {
			Backend config.PoolBackend `yaml:"backend"`
		} `yaml:"pool_manager"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, cli.NewExitError(fmt.Sprintf("config: parse %s: %v", path, err), exitInvalidConfig)
	}
	return doc.PoolManager.Backend == config.PoolBackendKubernetes, nil
}

// verifyCluster is `fleet verify` where the Kubernetes pool backend is
// configured: the Runner's configuration, loaded as the Runner loads it,
// and the verification of every Virtual Node with the Runner's own
// Kubernetes identity and namespace. timeout overrides the verification
// timeout when positive.
func verifyCluster(ctx context.Context, c *cli.Context, s fleetSeams, timeout time.Duration) error {
	out := c.App.Writer
	cfg, err := loadConfig(c)
	if err != nil {
		return err
	}
	if c.Bool("declare") {
		fmt.Fprintln(out, "note: --declare has no effect on a cluster fleet: verification pods are not taken from the Pools")
	}
	if timeout <= 0 {
		timeout = config.DefaultVerificationTimeout
		if cfg.Fleet != nil && cfg.Fleet.VerificationTimeout > 0 {
			timeout = cfg.Fleet.VerificationTimeout
		}
	}
	newVerifier := s.ClusterVerifier
	if newVerifier == nil {
		newVerifier = newClusterVerifier
	}
	v, err := newVerifier(cfg, timeout, out)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet verify: %v", err), exitFleetFailure)
	}
	report, err := v.Verify(ctx)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet verify: %v", err), exitFleetFailure)
	}
	for _, h := range report.Hosts {
		if h.Exercised {
			fmt.Fprintf(out, "%s: creation to ready %s\n", h.Host, h.ClaimToReady.Round(time.Millisecond))
		}
	}
	//= docs/requirements/12-cluster-fleet.md#cluster-verification
	//# If a Virtual Node is not ready or its verification pod does not
	//# become ready within the verification timeout, then the verification
	//# command SHALL exit with a non-zero status naming the Host and the
	//# reason.
	if err := verify.Summary(report); err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintln(out, "verification passed")
	return nil
}

// newClusterVerifier builds the cluster verifier the way the Runner builds
// its Kubernetes pool backend and kube-exec transport: the same client
// configuration, identity and namespace (KF-128), so that verification
// needs no permission the Runner lacks.
func newClusterVerifier(cfg *config.Config, timeout time.Duration, out io.Writer) (clusterVerifier, error) {
	k := config.KubernetesPools{}
	if cfg.PoolManager.Kubernetes != nil {
		k = *cfg.PoolManager.Kubernetes
	}
	restConfig, err := kubeRESTConfig(k.Kubeconfig, k.Context)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	namespace := kubeNamespace(k, cfg.Scheduler.Namespace, os.ReadFile)
	return verify.NewCluster(verify.ClusterConfig{
		Client:              client,
		Namespace:           namespace,
		Transports:          transport.NewFactory(transport.WithKubeExec(restConfig, namespace)),
		Profiles:            cfg.Profiles,
		CloudInitConfigMaps: k.CloudInitConfigMaps,
		HostServices:        cfg.HostServices,
		Timeout:             timeout,
		Out:                 out,
	})
}
