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
// battery gRPC client unless it names Kubernetes. Everything above
// poolmgr.Client is the same for both (KF-052).
func newPoolClient(cfg *config.Config, log *slog.Logger) (*poolClient, error) {
	pm := cfg.PoolManager
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
	namespace := kubeNamespace(k, cfg.Scheduler.Namespace, os.ReadFile)
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

// kubeRESTConfig is a Kubernetes client configuration: the named kubeconfig
// file, in the named context or its current one, or the in-cluster
// configuration of the pod the process runs in when no file is named. Both
// the Runner (KF-080) and the Pod Provider use it.
func kubeRESTConfig(kubeconfig, context string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig},
		&clientcmd.ConfigOverrides{CurrentContext: context},
	).ClientConfig()
}

// kubeNamespace is the Kubernetes namespace the Runner's Pools live in: the
// configured one, else the namespace of the Runner's own pod, else the Runner
// namespace of the Scheduler section.
func kubeNamespace(k config.KubernetesPools, runnerNamespace string, readFile func(string) ([]byte, error)) string {
	if k.Namespace != "" {
		return k.Namespace
	}
	if data, err := readFile(inClusterNamespaceFile); err == nil {
		if ns := strings.TrimSpace(string(data)); ns != "" {
			return ns
		}
	}
	return runnerNamespace
}
