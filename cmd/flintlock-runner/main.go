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
			Name:  "run",
			Usage: "start the Runner",
			Action: func(*cli.Context) error {
				return notImplemented("run", "executor")
			},
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
	// urfave/cli leaves App.ErrWriter nil unless the caller sets it (it
	// falls back to os.Stderr internally), and a text handler over a nil
	// writer panics on the first startup line, such as the CF-083 notice
	// that a file without a distributed_cache section gets.
	errOut := c.App.ErrWriter
	if errOut == nil {
		errOut = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(errOut, nil))
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

// newRunLoopCommand builds the gitlab-runner run loop as a library command
// (GL-004): the network client from the gitlab-runner network package
// (GL-002), the API request metrics collector, and a provider registry that
// will hold the flintlock executor. The `run` subcommand wraps it once the
// configuration translation (CF-011) exists; until then it is constructed
// here so that the wiring is compiled and tested.
func newRunLoopCommand(providers map[string]common.ExecutorProvider) (cli.Command, common.Network) {
	collector := network.NewAPIRequestsCollector()
	client := network.NewGitLabClient(network.WithAPIRequestsCollector(collector))
	registry := executors.NewProviderRegistry(providers)
	return commands.NewRunCommand(client, prometheus.Collector(collector), registry), client
}

// version is set by the linker (-X main.version=...) in release builds.
var version = "dev"
