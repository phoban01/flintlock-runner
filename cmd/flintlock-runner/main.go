// Command flintlock-runner is the GitLab CI runner that runs every Job in its
// own MicroVM on a fleet of flintlock Hosts. It is a single Go program built
// from the gitlab-runner packages with the flintlock executor inserted at the
// executor boundary (docs/architecture.md).
//
// Subcommands:
//
//	run                 start the Runner (the gitlab-runner run loop with the flintlock executor)
//	config show         print the effective configuration with secrets redacted
//	fleet provision     turn discovered instances into Hosts and write the Inventory
//	fleet verify        check Hosts, Pools and Host Services
//	fleet drain         remove a Host from every Pool and wait for its Leases
//	fleet teardown      delete Pools, stop services, remove the Inventory
//	fleet emit-userdata print the launch-template user-data script
//
// Phase 0 registers every subcommand; each one exits with ErrNotImplemented
// until its work package lands (docs/PLAN.md).
package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urfave/cli"

	"gitlab.com/gitlab-org/gitlab-runner/commands"
	"gitlab.com/gitlab-org/gitlab-runner/common"
	"gitlab.com/gitlab-org/gitlab-runner/executors"
	"gitlab.com/gitlab-org/gitlab-runner/network"
	_ "gitlab.com/gitlab-org/gitlab-runner/shells" // registers the bash shell (GL-041)

	"github.com/phoban01/flintlock-runner/internal/config"
)

// ErrNotImplemented is returned by every subcommand whose work package has
// not landed yet.
var ErrNotImplemented = errors.New("flintlock-runner: not implemented yet")

// exitNotImplemented is the exit status of a not-yet-implemented subcommand.
const exitNotImplemented = 3

func main() {
	if err := newApp().Run(os.Args); err != nil {
		var exitErr cli.ExitCoder
		if errors.As(err, &exitErr) {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitErr.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

//= docs/requirements/01-gitlab-protocol.md#library-basis
//# The Runner SHALL be a single Go program that imports the
//# gitlab-runner `common`, `network`, `shells` and `executors` packages and
//# SHALL NOT execute a `gitlab-runner` binary.

// newApp builds the command-line application. The run loop is the
// gitlab-runner library's own run command, constructed in newRunLoopCommand;
// no gitlab-runner binary is involved.
func newApp() *cli.App {
	app := cli.NewApp()
	app.Name = "flintlock-runner"
	app.Usage = "GitLab CI runner that executes every job in its own flintlock microVM"
	app.Version = version
	// cli.App leaves ErrWriter nil and falls back to os.Stderr only in its
	// own printing; the subcommands log to it, so it is set explicitly.
	app.ErrWriter = os.Stderr
	app.Flags = []cli.Flag{
		cli.StringFlag{
			Name:   "config",
			Usage:  "path of the YAML configuration file (CF-001)",
			EnvVar: "FLINTLOCK_RUNNER_CONFIG",
			Value:  "/etc/flintlock-runner/config.yaml",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:   "run",
			Usage:  "start the Runner",
			Action: runRunner,
		},
		{
			Name:  "config",
			Usage: "inspect the configuration",
			Subcommands: []cli.Command{{
				Name:   "show",
				Usage:  "print the effective configuration with every secret redacted (CF-006)",
				Action: configShow,
			}},
		},
		{
			Name:        "fleet",
			Usage:       "provision and operate the Host fleet",
			Subcommands: fleetCommands(),
		},
	}
	return app
}

// fleetCommands registers the Fleet Controller subcommands (06-fleet.md).
func fleetCommands() []cli.Command {
	var cmds []cli.Command
	for _, c := range []struct{ name, usage string }{
		{"provision", "discover instances, provision them as Hosts and write the Inventory"},
		{"verify", "check every Host, Pool and Host Service (FL-070)"},
		{"drain", "remove a Host from every Pool and wait for its Leases (FL-080)"},
		{"teardown", "delete Pools, stop services and remove the Inventory (FL-081)"},
		{"emit-userdata", "print the launch-template user-data script (FL-090)"},
	} {
		name := c.name
		cmds = append(cmds, cli.Command{
			Name:  name,
			Usage: c.usage,
			Action: func(*cli.Context) error {
				return notImplemented("fleet "+name, "fleet")
			},
		})
	}
	return cmds
}

// exitInvalidConfig is the exit status when the configuration file cannot be
// read or fails validation (CF-004).
const exitInvalidConfig = 2

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL provide a `config show` command that prints the
//# effective configuration with every secret value redacted.

// configShow is the action of `config show`: it loads the file named by
// --config or FLINTLOCK_RUNNER_CONFIG and prints the effective configuration
// with every secret redacted (config.Show).
func configShow(c *cli.Context) error {
	cfg, err := loadConfig(c)
	if err != nil {
		return err
	}
	return config.Show(c.App.Writer, cfg)
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# When starting, the Runner SHALL validate the configuration and,
//# if it is invalid, SHALL exit with a non-zero status and a message naming
//# the first invalid field and the reason.

// loadConfig loads and validates the configuration for a subcommand. A
// failure becomes a non-zero exit whose message is config.Load's error,
// which names the first invalid field and the reason (and lists the rest).
// Startup warnings go to the application's error writer.
func loadConfig(c *cli.Context) (*config.Config, error) {
	logger := slog.New(slog.NewTextHandler(c.App.ErrWriter, nil))
	cfg, err := config.Load(c.GlobalString("config"), config.WithLogger(logger))
	if err != nil {
		return nil, cli.NewExitError(err.Error(), exitInvalidConfig)
	}
	return cfg, nil
}

// notImplemented is the action of every subcommand whose work package has
// not landed. It names the work package from docs/PLAN.md.
func notImplemented(command, workPackage string) error {
	return cli.NewExitError(
		fmt.Sprintf("%v: %q lands with the %q work package (docs/PLAN.md)", ErrNotImplemented, command, workPackage),
		exitNotImplemented,
	)
}

//= docs/requirements/01-gitlab-protocol.md#library-basis
//# The Runner SHALL drive job acquisition and execution through the
//# gitlab-runner run loop (`commands.NewRunCommand`) with a provider registry
//# that contains the Executor, so that graceful shutdown, token rotation and
//# the session server come from the library unchanged.

//= docs/requirements/01-gitlab-protocol.md#library-basis
//# The Runner SHALL perform every request to the GitLab API through
//# the `GitLabClient` type of the gitlab-runner `network` package.

// newRunLoopCommand builds the gitlab-runner run loop as a library command
// (GL-004): the network client from the gitlab-runner network package
// (GL-002), the API request metrics collector, and a provider registry
// holding the flintlock executor. The network client looks executors up in
// the same registry, so the info payload of every request, runners/verify
// included, carries the flintlock executor's features (GL-020, GL-022).
func newRunLoopCommand(providers map[string]common.ExecutorProvider) (cli.Command, common.Network) {
	collector := network.NewAPIRequestsCollector()
	registry := executors.NewProviderRegistry(providers)
	client := network.NewGitLabClient(
		network.WithAPIRequestsCollector(collector),
		network.WithExecutorProviderFunc(registry.GetByName),
	)
	return commands.NewRunCommand(client, prometheus.Collector(collector), registry), client
}

// version is set by the linker (-X main.version=...) in release builds.
var version = "dev"
