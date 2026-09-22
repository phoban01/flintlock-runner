package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/phoban01/flintlock-runner/internal/kubelet"
)

// kubeletCommand is `flr kubelet`, the Pod Provider of a cluster fleet
// (docs/requirements/12-cluster-fleet.md). It runs on a Host, in the Host
// Agent, and has a configuration file of its own: the Runner's does not
// exist there.
func kubeletCommand() cli.Command {
	return cli.Command{
		Name:  "kubelet",
		Usage: "run the Pod Provider: register this Host's Virtual Node and run its pods as flintlock microVMs",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "kubelet-config",
				Usage:  "path of the Pod Provider's YAML configuration file",
				EnvVar: "FLINTLOCK_RUNNER_KUBELET_CONFIG",
				Value:  "/etc/flintlock-runner/kubelet.yaml",
			},
			cli.StringFlag{
				Name:   "host-node",
				Usage:  "name of this Host's Node; overrides host_node, and is what the Host Agent sets from the downward API",
				EnvVar: "FLINTLOCK_RUNNER_HOST_NODE",
			},
		},
		Action: runKubelet,
	}
}

// runKubelet loads the configuration, connects to the API server and to the
// local flintlockd, and runs the provider until it is signalled. An invalid
// configuration, a flintlockd endpoint that is not local among them
// (KF-018), exits with the status every invalid configuration does.
func runKubelet(c *cli.Context) error {
	logger := slog.New(slog.NewTextHandler(c.App.ErrWriter, nil))

	cfg, err := kubelet.LoadConfigWith(c.String("kubelet-config"), func(cfg *kubelet.Config) {
		if name := c.String("host-node"); name != "" {
			cfg.HostNode = name
		}
	})
	if err != nil {
		return cli.NewExitError(err.Error(), exitInvalidConfig)
	}

	restConfig, err := providerRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("kubelet: %v", err), 1)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("kubelet: building the Kubernetes client: %v", err), 1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	host, err := kubelet.DialLocal(ctx, cfg.HostNode, cfg.Flintlockd)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}
	defer func() { _ = host.Close() }()

	if err := kubelet.Run(ctx, kubelet.Options{Config: cfg, Kube: kube, Host: host, Logger: logger, Version: version}); err != nil && ctx.Err() == nil {
		return cli.NewExitError(err.Error(), 1)
	}
	return nil
}

// providerRESTConfig is the Pod Provider's client configuration: the in-cluster
// one, or a kubeconfig's.
func providerRESTConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig == "" {
		return rest.InClusterConfig()
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}
