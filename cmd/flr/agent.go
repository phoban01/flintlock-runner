package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/phoban01/flintlock-runner/internal/agent"
)

// claimResync is the full resync period of the Exec Agent's claim cache.
const claimResync = 10 * time.Minute

// agentCommand is `flr agent`, the Exec Agent of a cluster fleet
// (docs/requirements/12-cluster-fleet.md#exec-agent). It runs on a Host, in
// the Host Agent, and has a configuration file of its own: the Runner's
// does not exist there.
func agentCommand() cli.Command {
	return cli.Command{
		Name:  "agent",
		Usage: "run the Exec Agent: relay exec requests of claim holders to this Host's flintlockd and report the Host's readiness",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   "agent-config",
				Usage:  "path of the Exec Agent's YAML configuration file",
				EnvVar: "FLINTLOCK_RUNNER_AGENT_CONFIG",
				Value:  "/etc/flintlock-runner/agent/agent.yaml",
			},
			cli.StringFlag{
				Name:   "host-node",
				Usage:  "name of this Host's Node; overrides host_node, and is what the Host Agent sets from the downward API",
				EnvVar: "FLINTLOCK_RUNNER_HOST_NODE",
			},
		},
		Action: runAgent,
	}
}

// runAgent loads the configuration, checks who the agent is, connects to
// the API server and to the local flintlockd, and runs the agent until it
// is signalled. An invalid configuration, a flintlockd endpoint that is not
// local among them (KF-171), exits with the status every invalid
// configuration does.
func runAgent(c *cli.Context) error {
	logger := slog.New(slog.NewTextHandler(c.App.ErrWriter, nil))

	cfg, err := agent.LoadConfig(c.String("agent-config"), func(cfg *agent.Config) {
		if name := c.String("host-node"); name != "" {
			cfg.HostNode = name
		}
	})
	if err != nil {
		return cli.NewExitError(err.Error(), exitInvalidConfig)
	}
	if err := checkAgentUser(cfg.FlintlockdUserID, os.Getuid()); err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	restConfig, err := providerRESTConfig(cfg.Kubeconfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("agent: %v", err), 1)
	}
	kube, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("agent: building the Kubernetes client: %v", err), 1)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("agent: building the Kubernetes client: %v", err), 1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The admission policy tells Hosts apart by the node in the agent's
	// identity (KF-180); without it every change to the Host's Node would be
	// refused, so an agent that does not carry its Host does not start.
	if err := agent.CheckIdentity(ctx, kube, cfg.HostNode); err != nil {
		return cli.NewExitError(err.Error(), 1)
	}

	fl, err := agent.DialFlintlockd(cfg.Flintlockd)
	if err != nil {
		return cli.NewExitError(err.Error(), 1)
	}
	defer func() { _ = fl.Close() }()

	res := agent.ProvisionalClaimResource
	res.Group, res.Version, res.Resource = cfg.Claims.Group, cfg.Claims.Version, cfg.Claims.Resource
	claims := agent.NewDynamicClaims(dyn, res, claimResync)
	go claims.Run(ctx)

	if err := agent.Run(ctx, agent.Options{Config: cfg, Kube: kube, Claims: claims, Flintlockd: fl, Logger: logger}); err != nil && ctx.Err() == nil {
		return cli.NewExitError(err.Error(), 1)
	}
	return nil
}

//= docs/requirements/12-cluster-fleet.md#exec-agent
//# The Exec Agent SHALL reach `flintlockd` only through the local
//# endpoint of HI-042, and the Fleet Manifests SHALL run it as the user id
//# that HI-063 admits there and run no other container as that user id.

// checkAgentUser refuses to start the agent as any user but the one the
// Host Image admits to flintlockd (HI-063): as any other, every call to
// flintlockd would be reset by the Host's firewall, and the Host would
// report not ready for a reason nobody would guess. A negative user id
// skips the check, for running the agent off a Host.
func checkAgentUser(want, uid int) error {
	if want < 0 || uid == want {
		return nil
	}
	return fmt.Errorf("agent: running as user id %d, but flintlockd admits only user id %d (flintlockd_user_id, HI-063); "+
		"run the agent as that user id", uid, want)
}
