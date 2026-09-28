package main

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
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
	// kube is the Kubernetes API access of the Kubernetes pool backend, nil
	// for battery. The kube-exec Guest Transport and the Virtual Node
	// lookup share it, so that the Runner talks to one API server as one
	// identity (KF-128).
	kube *kubeAccess
}

// kubeAccess is how the Runner of a cluster fleet reaches the Kubernetes
// API: one client configuration, its clientset and the namespace of its
// Pools.
type kubeAccess struct {
	config    *rest.Config
	client    kubernetes.Interface
	namespace string
}

// newPoolClient builds the Pool backend pool_manager.backend selects: the
// battery gRPC client unless it names Kubernetes or claim. Everything above
// poolmgr.Client is the same for each (KF-052, KF-156).
func newPoolClient(cfg *config.Config, log *slog.Logger) (*poolClient, error) {
	pm := cfg.PoolManager
	if pm.IsClaim() {
		return newClaimClient(cfg, log)
	}
	if !pm.IsKubernetes() {
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

	k := config.KubernetesPools{}
	if pm.Kubernetes != nil {
		k = *pm.Kubernetes
	}
	restConfig, err := kubeRESTConfig(k.Kubeconfig, k.Context)
	if err != nil {
		return nil, fmt.Errorf("kubernetes pool backend: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("kubernetes pool backend: %w", err)
	}
	namespace := kubeNamespace(k.Namespace, cfg.Scheduler.Namespace, os.ReadFile)
	backend, err := kube.New(kube.Options{
		Client:              clientset,
		Namespace:           namespace,
		RunnerName:          cfg.GitLab.Name,
		Profiles:            cfg.Profiles,
		CloudInitConfigMaps: k.CloudInitConfigMaps,
		JobTimeout:          k.JobTimeout,
		CleanupMargin:       k.CleanupMargin,
		RolloutInterval:     k.RolloutInterval,
		Deadline:            pm.Deadline,
		Log:                 log,
	})
	if err != nil {
		return nil, fmt.Errorf("kubernetes pool backend: %w", err)
	}
	return &poolClient{
		Client:      backend,
		setProfiles: backend.SetProfiles,
		kube:        &kubeAccess{config: restConfig, client: clientset, namespace: namespace},
	}, nil
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
	return &poolClient{Client: backend, setProfiles: backend.SetProfiles}, nil
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

// kubeNamespace is the Kubernetes namespace the Runner's Pools live in: the
// configured one, else the namespace of the Runner's own pod, else the Runner
// namespace of the Scheduler section.
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
