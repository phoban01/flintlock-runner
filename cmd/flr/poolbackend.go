package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"google.golang.org/grpc"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/executor"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
)

// inClusterNamespaceFile is where a pod finds its own namespace.
const inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// poolClient is the configured Pool backend and, for a backend that needs to
// be told, the way to give it the Profiles of a configuration reload.
type poolClient struct {
	poolmgr.Client
	// setProfiles is nil for battery, which learns everything it needs from
	// the PoolSpec.
	setProfiles func([]config.Profile)
	// claim is what the claim pool backend gives the agent-exec Guest
	// Transport and the Host Service lookup, nil for battery.
	claim *claimAccess
}

// claimAccess is what the Runner of the claim design reaches its guests
// with: the claims the claim pool backend holds, each of which dials its
// own Exec Agent, and the Nodes of the same API server, read as the same
// identity (KF-189).
type claimAccess struct {
	backend *claim.Backend
	nodes   executor.NodeGetter
}

// dialAgent dials the Exec Agent of the claim that the backend holds under
// leaseID, with that claim's own connection. battery-operator's Client
// Library makes it: it verifies the agent against the Operator's serving
// CA and sends the claim's current token with every call (KF-186). A
// lease that the backend does not hold has no connection.
func (c *claimAccess) dialAgent(ctx context.Context, leaseID string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	held, ok := c.backend.Held(leaseID)
	if !ok {
		return nil, fmt.Errorf("claim pool backend: no claim is held under lease %s", leaseID)
	}
	return held.Dial(ctx, opts...)
}

// newPoolClient builds the Pool backend pool_manager.backend selects: the
// battery gRPC client unless it names claim. Everything above
// poolmgr.Client is the same for each (KF-156).
func newPoolClient(cfg *config.Config, log *slog.Logger) (*poolClient, error) {
	pm := cfg.PoolManager
	if pm.IsClaim() {
		return newClaimClient(cfg, log)
	}
	client, err := poolmgr.NewClient(poolmgr.ClientConfig{
		Endpoint: pm.Endpoint,
		TLS:      pm.TLS,
		Deadline: pm.Deadline,
	})
	if err != nil {
		return nil, fmt.Errorf("pool manager client: %w", err)
	}
	return &poolClient{Client: client}, nil
}

// newClaimClient builds the claim pool backend: a Lease is a
// battery-operator MicroVMClaim, made for the configured Holder, and a Pool
// is a Pool resource. It reads the Profiles for the Pools' node selectors,
// and the serving CA from its ConfigMap on the first claim, so that it does
// not need the API server to be up, as the battery client needs no battery.
func newClaimClient(cfg *config.Config, log *slog.Logger) (*poolClient, error) {
	pm := cfg.PoolManager
	c := config.ClaimPools{}
	if pm.Claim != nil {
		c = *pm.Claim
	}
	restConfig, err := kubeRESTConfig(c.Kubeconfig, c.Context)
	if err != nil {
		return nil, fmt.Errorf("claim pool backend: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("claim pool backend: %w", err)
	}
	backend, err := claim.New(claim.Options{
		Config:          restConfig,
		Namespace:       kubeNamespace(c.Namespace, cfg.Scheduler.Namespace, os.ReadFile),
		RunnerName:      cfg.GitLab.Name,
		RunnerNamespace: cfg.Scheduler.Namespace,
		Holder:          c.HolderServiceAccount,
		ServingCA:       c.ServingCA,
		Profiles:        cfg.Profiles,
		Deadline:        pm.Deadline,
		Log:             log,
	})
	if err != nil {
		return nil, fmt.Errorf("claim pool backend: %w", err)
	}
	return &poolClient{
		Client:      backend,
		setProfiles: backend.SetProfiles,
		claim:       &claimAccess{backend: backend, nodes: clientset.CoreV1().Nodes()},
	}, nil
}

// kubeRESTConfig is the client configuration: the named kubeconfig file and
// context, or the Runner's own pod (KF-080).
func kubeRESTConfig(kubeconfig, kubeContext string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: kubeContext},
	).ClientConfig()
}

// kubeNamespace is the Kubernetes namespace of the Runner's claims and
// Pools: the configured one, else the namespace of the Runner's own pod,
// else the Runner namespace of the Scheduler section.
func kubeNamespace(configured, runnerNamespace string, readFile func(string) ([]byte, error)) string {
	if configured != "" {
		return configured
	}
	if data, err := readFile(inClusterNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return runnerNamespace
}
