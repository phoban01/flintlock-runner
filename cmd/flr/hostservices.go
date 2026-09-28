package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/hostservices"
)

// hostServicesCommand is `flr host-services`, the container of the Host
// Agent that publishes where the Host's Host Services listen on the Host's
// Node (docs/requirements/12-cluster-fleet.md, KF-194). It runs on a Host
// and has a configuration file of its own: the Runner's does not exist
// there.
func hostServicesCommand() cli.Command {
	return cli.Command{
		Name:  "host-services",
		Usage: "publish the addresses of this Host's Host Services on its Node",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "host-services-config",
				Usage:  "path of the YAML configuration file that render.sh makes for this Host",
				EnvVar: "FLINTLOCK_RUNNER_HOST_SERVICES_CONFIG",
				Value:  "/etc/flr/rendered/host-services.yaml",
			},
			cli.StringFlag{
				Name:   "host-node",
				Usage:  "name of this Host's Node; overrides host_node, and is what the Host Agent sets from the downward API",
				EnvVar: "FLINTLOCK_RUNNER_HOST_NODE",
			},
		},
		Action: runHostServices,
	}
}

// runHostServices loads the configuration, connects to the API server and
// keeps the annotations on the Host's Node until it is signalled. An
// invalid configuration exits with the status every invalid configuration
// does.
func runHostServices(c *cli.Context) error {
	logger := slog.New(slog.NewTextHandler(c.App.ErrWriter, nil))
	cfg, err := hostservices.LoadConfig(c.String("host-services-config"), func(cfg *hostservices.Config) {
		if name := c.String("host-node"); name != "" {
			cfg.HostNode = name
		}
	})
	if err != nil {
		return cli.NewExitError(err.Error(), exitInvalidConfig)
	}
	restConfig, err := providerRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("host-services: %v", err), 1)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("host-services: building the Kubernetes client: %v", err), 1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("Publishing the Host Services on the host's Node", "node", cfg.HostNode, "services", cfg.EnabledHostServices())
	return hostservices.Run(ctx, cfg, kube, logger)
}
