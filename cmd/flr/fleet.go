package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli"
	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/ca"
	"github.com/phoban01/flintlock-runner/internal/fleet/discovery"
	"github.com/phoban01/flintlock-runner/internal/fleet/drain"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/launchtemplate"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote"
	"github.com/phoban01/flintlock-runner/internal/fleet/verify"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// exitFleetFailure is the exit status of a fleet subcommand that ran and
// found failures (FL-013, FL-071).
const exitFleetFailure = 1

// fleetCommands registers the Fleet Controller subcommands (06-fleet.md).
func fleetCommands(s fleetSeams) []cli.Command {
	return []cli.Command{
		{
			Name:  "provision",
			Usage: "discover instances, provision the new ones as Hosts and write the Inventory and Runner configuration",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "dry-run", Usage: "print what would be provisioned, left alone and removed, and change nothing (FL-118)"},
			},
			Action: func(c *cli.Context) error { return fleetProvision(c, s) },
		},
		{
			Name:  "verify",
			Usage: "check every Host, Pool and Host Service (FL-070)",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "declare", Usage: "declare the Profiles' Pools first, as the Runner does, so that a fleet can be verified before the Runner starts"},
				cli.DurationFlag{Name: "timeout", Usage: "verification timeout; default fleet.verification_timeout"},
			},
			Action: func(c *cli.Context) error { return fleetVerify(c, s) },
		},
		{
			Name:  "up",
			Usage: "provision the fleet, then verify it with the Profiles' Pools declared, and print one summary (FL-119)",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "dry-run", Usage: "print the provision plan and what up would do next, and change nothing (FL-124)"},
				cli.BoolFlag{Name: "install-runner", Usage: "also run the Runner as the systemd service flr on the Control Node (FL-122)"},
				cli.DurationFlag{Name: "timeout", Usage: "verification timeout; default fleet.verification_timeout"},
			},
			Action: func(c *cli.Context) error { return fleetUp(c, s) },
		},
		{
			Name:      "drain",
			Usage:     "remove a Host from every Pool and wait for its Leases (FL-080)",
			ArgsUsage: "HOST",
			Flags: []cli.Flag{
				cli.DurationFlag{Name: "timeout", Usage: "drain timeout; default fleet.drain_timeout"},
				cli.BoolFlag{Name: "stop", Usage: "run the drain script on the Host once no Lease remains on it"},
			},
			Action: func(c *cli.Context) error { return fleetDrain(c, s) },
		},
		{
			Name:  "teardown",
			Usage: "delete Pools, stop services and remove the Inventory (FL-081)",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "terminate", Usage: "also terminate the EC2 instances (FL-082)"},
				cli.BoolFlag{Name: "purge", Usage: "also remove the thin pool and wipe its device (FL-083)"},
			},
			Action: func(c *cli.Context) error { return fleetTeardown(c, s) },
		},
		{
			Name: "emit-userdata",
			Usage: "write the launch-template user-data to standard output, gzip-compressed as cloud-init " +
				"accepts it and within EC2's 16 KB limit (FL-090)",
			Flags: []cli.Flag{
				cli.BoolFlag{Name: "plain", Usage: "write the uncompressed script instead, for inspection"},
			},
			Action: func(c *cli.Context) error { return fleetEmitUserData(c, s) },
		},
	}
}

// seamErr turns the error of building a dependency into the command's exit
// error.
func seamErr(err error) error {
	return cli.NewExitError(err.Error(), exitFleetFailure)
}

// loadFleetConfig loads the configuration for a fleet subcommand. The fleet
// input is a whole Runner configuration with a fleet section, but on the
// first `fleet provision` there is no Inventory yet, so the Inventory
// section is replaced before validation by hosts, or, when hosts is nil,
// by the Hosts of the Inventory file the configuration references or lists
// inline. When there are none yet, validation runs against one placeholder
// Host per Profile that the Profile's selector accepts, and the returned
// configuration has no Hosts. withEnv controls the environment overrides of
// secrets; the Runner configuration is generated from a load without them,
// so that a secret the operator supplied through the environment is not
// written to disk.
func loadFleetConfig(c *cli.Context, hosts []config.HostEntry, withEnv bool) (*config.Config, error) {
	return loadFleetConfigAt(c, c.GlobalString("config"), hosts, withEnv)
}

// loadFleetConfigAt is loadFleetConfig for the configuration at path rather
// than the one --config names; fleet up verifies the Runner configuration
// provisioning wrote with it.
func loadFleetConfigAt(c *cli.Context, path string, hosts []config.HostEntry, withEnv bool) (*config.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("config: read %s: %v", path, err), exitInvalidConfig)
	}
	dir := filepath.Dir(path)
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("config: parse %s: %v", path, err), exitInvalidConfig)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	if hosts == nil {
		hosts, err = referencedHosts(data, dir)
		if err != nil {
			return nil, cli.NewExitError(err.Error(), exitInvalidConfig)
		}
	}
	checked := hosts
	if len(checked) == 0 {
		if checked, err = placeholderHosts(data); err != nil {
			return nil, cli.NewExitError(fmt.Sprintf("config: parse %s: %v", path, err), exitInvalidConfig)
		}
	}
	doc["inventory"] = map[string]any{"hosts": checked}
	rewritten, err := yaml.Marshal(doc)
	if err != nil {
		return nil, cli.NewExitError(fmt.Sprintf("config: %s: %v", path, err), exitInvalidConfig)
	}
	opts := []config.Option{config.WithLogger(slog.New(slog.NewTextHandler(c.App.ErrWriter, nil)))}
	if !withEnv {
		opts = append(opts, config.WithEnv(func(string) (string, bool) { return "", false }))
	}
	cfg, err := config.Parse(rewritten, dir, opts...)
	if err != nil {
		return nil, cli.NewExitError(strings.Replace(err.Error(), "<memory>", path, 1), exitInvalidConfig)
	}
	if cfg.Fleet == nil {
		return nil, cli.NewExitError(fmt.Sprintf("config: %s has no fleet section; the fleet subcommands need one (CF-060)", path), exitInvalidConfig)
	}
	cfg.Inventory = config.Inventory{Hosts: hosts}
	return cfg, nil
}

// referencedHosts reads the Hosts the configuration names: the inline list,
// or the Inventory file it references, which may not exist yet.
func referencedHosts(data []byte, dir string) ([]config.HostEntry, error) {
	var head struct {
		Inventory config.Inventory `yaml:"inventory"`
		Fleet     *struct {
			InventoryPath string `yaml:"inventory_path"`
		} `yaml:"fleet"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(head.Inventory.Hosts) > 0 {
		return head.Inventory.Hosts, nil
	}
	file := head.Inventory.File
	if file == "" && head.Fleet != nil {
		file = head.Fleet.InventoryPath
	}
	if file == "" {
		file = config.DefaultInventoryPath
	}
	if !filepath.IsAbs(file) {
		file = filepath.Join(dir, file)
	}
	inv, err := (inventory.Store{}).Load(context.Background(), file)
	if err != nil {
		return nil, err
	}
	return inv.Hosts, nil
}

// placeholderHosts is one Host per Profile that the Profile's architecture
// and selector accept, so that a configuration can be validated before any
// Host exists. They are never written anywhere.
func placeholderHosts(data []byte) ([]config.HostEntry, error) {
	var head struct {
		Profiles []config.Profile `yaml:"profiles"`
	}
	if err := yaml.Unmarshal(data, &head); err != nil {
		return nil, err
	}
	if len(head.Profiles) == 0 {
		head.Profiles = []config.Profile{{}}
	}
	out := make([]config.HostEntry, 0, len(head.Profiles))
	for i, p := range head.Profiles {
		arch := p.Arch
		if arch == "" {
			arch = config.Architecture(runtime.GOARCH)
		}
		out = append(out, config.HostEntry{
			Name:     fmt.Sprintf("placeholder-%d", i),
			Endpoint: fmt.Sprintf("placeholder-%d.invalid:9090", i),
			Arch:     arch,
			VCPU:     1,
			MemoryMB: 1,
			Labels:   p.HostSelector,
			TLS:      config.ClientTLS{Insecure: true},
		})
	}
	return out, nil
}

// fleetInventoryPath is where the Inventory file lives.
func fleetInventoryPath(cfg *config.Config) string { return cfg.Fleet.InventoryPath }

// tlsDir is where generated TLS material is kept: beside the Inventory.
func tlsDir(cfg *config.Config) string {
	return filepath.Join(filepath.Dir(fleetInventoryPath(cfg)), "tls")
}

// certificateAuthority is the CA built from the fleet section (SE-023).
func certificateAuthority(cfg *config.Config) ca.Authority {
	return ca.Authority{Supplied: cfg.Fleet.Flintlockd.TLS, Overrides: cfg.Fleet.EndpointOverrides}
}

// fleetProvision is `fleet provision`: discover, provision only the new
// instances, merge them into the Inventory, drop the vanished ones, write
// the Inventory and the Runner configuration, and install or reload the
// Pool Manager daemon with the new host list. With --dry-run it stops once
// the plan is computed and reports it.
func fleetProvision(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	// Every instance's output streams here at once (FL-014).
	out := remote.SyncWriter(c.App.Writer)
	p, err := planProvision(ctx, c, s, out)
	if err != nil {
		return err
	}
	if c.Bool("dry-run") {
		p.report(out)
		return nil
	}
	_, err = p.apply(ctx, c, s, out)
	return err
}

// provisionPlan is what fleet provision has found before it changes
// anything: the configuration, the Inventory as it stands, and what the
// run has to do to it.
type provisionPlan struct {
	cfg     *config.Config
	current *fleet.Inventory
	plan    inventory.Plan
}

// planProvision loads the configuration and the Inventory, discovers the
// instances and computes the plan with inventory.Diff. It reaches no Host,
// writes nothing and does not touch the Pool Manager: a dry run stops after
// it and a real run continues from it, so the two cannot plan differently.
func planProvision(ctx context.Context, c *cli.Context, s fleetSeams, out io.Writer) (*provisionPlan, error) {
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return nil, err
	}
	if err := checkSecretsOverSSM(cfg.Fleet); err != nil {
		return nil, cli.NewExitError(err.Error(), exitInvalidConfig)
	}
	current, err := (inventory.Store{}).Load(ctx, fleetInventoryPath(cfg))
	if err != nil {
		return nil, cli.NewExitError(err.Error(), exitFleetFailure)
	}
	disc, err := s.discovery(ctx, cfg, func(u discovery.Unsupported) {
		fmt.Fprintf(out, "excluded %s: %s\n", u.Instance.ID, u.Reason)
	})
	if err != nil {
		return nil, seamErr(err)
	}
	discovered, err := disc.Discover(ctx)
	if err != nil {
		// Without a complete discovery nothing can be said to have gone;
		// the Inventory is left as it is.
		return nil, cli.NewExitError(fmt.Sprintf("fleet provision: discovery: %v", err), exitFleetFailure)
	}
	return &provisionPlan{cfg: cfg, current: current, plan: inventory.Diff(current, discovered)}, nil
}

// provisionOutcome is what a provisioning run did.
type provisionOutcome struct {
	// provisioned is the number of instances provisioned by this run.
	provisioned int
	// inventory is the Inventory it wrote, nil when it wrote none.
	inventory *fleet.Inventory
}

// apply carries out the plan: it provisions the new instances, writes the
// Inventory and the Runner configuration, and installs or reloads the Pool
// Manager daemon. The error is the command's exit error, summarising every
// failure (FL-013).
func (p *provisionPlan) apply(ctx context.Context, c *cli.Context, s fleetSeams, out io.Writer) (*provisionOutcome, error) {
	cfg, current, plan := p.cfg, p.current, p.plan
	store := inventory.Store{}
	invPath := fleetInventoryPath(cfg)
	//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
	//# When run again after an instance in the Inventory no longer
	//# exists, the Fleet Controller SHALL remove it from the Inventory and report
	//# the removal.
	for _, h := range plan.Removed {
		fmt.Fprintf(out, "removed %s (instance %s) from the Inventory: the instance no longer exists\n", h.Name, inventory.InstanceID(&h))
	}
	for _, inst := range plan.Known {
		fmt.Fprintf(out, "%s is already in the Inventory; not provisioned again\n", inst.ID)
	}

	scr, err := s.scripts(cfg)
	if err != nil {
		return nil, seamErr(err)
	}
	authority := certificateAuthority(cfg)
	var failures []fleet.Failure
	var added []config.HostEntry
	if len(plan.New) > 0 {
		results, err := provisionNew(ctx, s, cfg, out, scr, authority, peerInventory(cfg, current, plan), plan.New)
		if err != nil {
			return nil, err
		}
		for _, res := range results {
			if res.Err != nil || res.Entry == nil {
				failures = append(failures, hostFailure(res))
				continue
			}
			entry, err := completeEntry(cfg, res)
			if err != nil {
				failures = append(failures, fleet.Failure{Host: res.Instance.ID, Err: err})
				continue
			}
			added = append(added, entry)
			fmt.Fprintf(out, "provisioned %s as %s at %s\n", res.Instance.ID, entry.Name, entry.Endpoint)
		}
	}

	merged := inventory.Merge(current, plan, added, s.Now().UTC())
	if err := store.Save(ctx, invPath, merged); err != nil {
		return nil, cli.NewExitError(err.Error(), exitFleetFailure)
	}
	outcome := &provisionOutcome{provisioned: len(added), inventory: merged}
	fmt.Fprintf(out, "wrote the Inventory %s with %d Host(s)\n", invPath, len(merged.Hosts))
	if err := writeRunnerConfig(ctx, c, out, cfg, merged); err != nil {
		failures = append(failures, fleet.Failure{Host: "runner configuration", Err: err})
	}
	if len(merged.Hosts) > 0 || len(current.Hosts) > 0 {
		if err := applyPoolManager(ctx, s, cfg, out, scr, authority, len(current.Hosts) == 0, merged); err != nil {
			failures = append(failures, fleet.Failure{Host: controlNodeName, Step: fleet.StepControlNode, Err: err})
		}
	}
	//= docs/requirements/06-fleet.md#remote-execution
	//# If provisioning fails on one instance, then the Fleet Controller
	//# SHALL continue provisioning the others and SHALL exit with a non-zero
	//# status that summarises every failure.
	//
	// Every instance ran to its end in provisionNew whatever the others
	// did, the ones that succeeded are in the Inventory, and every failure
	// is one line of the exit message.
	if len(failures) > 0 {
		return outcome, cli.NewExitError(summarise("fleet provision", failures), exitFleetFailure)
	}
	return outcome, nil
}

// peerInventory is the Inventory the new instances' guest firewalls take
// their peers from (FL-046): the Hosts that stay, and the instances being
// provisioned alongside, at their private addresses.
func peerInventory(cfg *config.Config, current *fleet.Inventory, plan inventory.Plan) fleet.Inventory {
	peers := inventory.Merge(current, plan, nil, time.Time{})
	for _, inst := range plan.New {
		peers.Hosts = append(peers.Hosts, config.HostEntry{Name: inst.ID, Endpoint: discovery.Endpoint(fleet.Instance{ID: inst.ID, PrivateIP: inst.PrivateIP}, cfg.Fleet)})
	}
	return *peers
}

// provisionNew runs the Provisioner over instances with the configured
// parallelism (FL-012) and returns every result in the order of instances.
// One instance failing does not stop the others (FL-013).
func provisionNew(ctx context.Context, s fleetSeams, cfg *config.Config, out io.Writer, scr fleet.Scripts, authority ca.Authority, peers fleet.Inventory, instances []fleet.Instance) ([]fleet.HostResult, error) {
	var bundle *fleet.CertBundle
	if !cfg.Fleet.Flintlockd.Insecure {
		var err error
		if bundle, err = authority.Ensure(ctx, tlsDir(cfg), instances); err != nil {
			return nil, cli.NewExitError(fmt.Sprintf("fleet provision: TLS material: %v", err), exitFleetFailure)
		}
	}
	rem, err := s.remote(ctx, cfg)
	if err != nil {
		return nil, seamErr(err)
	}
	var params fleet.Parameters
	if overSSM(cfg.Fleet) {
		// Run Command has no standard input: the scripts read the token,
		// the TLS material and the Host Service credentials from their
		// Systems Manager parameters on the Host instead (SE-015).
		rem = parameterSecrets{rem}
	} else if params, err = s.parameters(ctx, cfg); err != nil {
		return nil, seamErr(err)
	}
	prov, err := s.provisioner(cfg, fleet.Deps{
		Remote:     rem,
		Scripts:    scr,
		Parameters: params,
		Inventory:  inventory.Store{},
		Certs:      authority,
		Out:        out,
	}, bundle, func() fleet.Inventory { return peers })
	if err != nil {
		return nil, seamErr(err)
	}
	results, err := remote.Fan(ctx, instances, cfg.Fleet.Parallelism, func(ctx context.Context, inst fleet.Instance) (fleet.HostResult, error) {
		res := prov.Provision(ctx, inst)
		if res.Instance.ID == "" {
			res.Instance = inst
		}
		if res.Err == nil && res.Entry == nil {
			res.Err = errors.New("the provisioner returned no Inventory entry")
		}
		return res, res.Err
	})
	// An instance Fan never started, because ctx ended, has no result of
	// its own; its failure is Fan's.
	var failed remote.Failures
	if errors.As(err, &failed) {
		for _, f := range failed {
			for i := range results {
				if results[i].Instance.ID == "" && instances[i].ID == f.Instance.ID {
					results[i] = fleet.HostResult{Instance: f.Instance, Err: f.Err}
				}
			}
		}
	}
	return results, nil
}

// completeEntry finishes a provisioned Host's Inventory entry: capacity and
// defaults (inventory.Complete), the TLS material the Runner verifies it
// with, and the fleet's flintlockd token when the Provisioner recorded none.
func completeEntry(cfg *config.Config, res fleet.HostResult) (config.HostEntry, error) {
	entry, err := inventory.Complete(*res.Entry, res.Instance, cfg.Fleet)
	if err != nil {
		return entry, err
	}
	return withFleetAccess(cfg, entry), nil
}

// withFleetAccess gives an entry the TLS material the Runner verifies the
// Host with and the fleet's flintlockd token, where it has none.
func withFleetAccess(cfg *config.Config, entry config.HostEntry) config.HostEntry {
	switch {
	case cfg.Fleet.Flintlockd.Insecure:
		entry.TLS = config.ClientTLS{Insecure: true}
	case entry.TLS.CAFile == "":
		if cfg.Fleet.Flintlockd.TLS.CAFile != "" {
			entry.TLS.CAFile = cfg.Fleet.Flintlockd.TLS.CAFile
		} else {
			entry.TLS.CAFile = filepath.Join(tlsDir(cfg), "ca.pem")
		}
	}
	if entry.Token == "" {
		entry.Token = cfg.Fleet.Flintlockd.Token
	}
	return entry
}

// writeRunnerConfig writes the Runner configuration (FL-062). It is
// generated from the configuration as written, without environment
// overrides, and validated against the merged Inventory.
func writeRunnerConfig(ctx context.Context, c *cli.Context, out io.Writer, cfg *config.Config, merged *fleet.Inventory) error {
	path := cfg.Fleet.RunnerConfigPath
	if len(merged.Hosts) == 0 {
		return errors.New("no Host is provisioned yet, so there is no Runner configuration to write")
	}
	input, err := loadFleetConfig(c, merged.Hosts, false)
	if err != nil {
		return err
	}
	if inputReferences(c.GlobalString("config"), path, fleetInventoryPath(cfg)) {
		// The input is the Runner configuration and already points at the
		// Inventory: rewriting it would only expand its defaults and drop
		// its comments.
		fmt.Fprintf(out, "the Runner configuration %s already references the Inventory\n", path)
		return nil
	}
	if err := (inventory.Store{}).SaveRunnerConfig(ctx, path, inventory.RunnerConfig(input, fleetInventoryPath(cfg))); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote the Runner configuration %s\n", path)
	return nil
}

// inputReferences reports whether the Runner configuration path is the
// input file itself and that file already references the Inventory file.
func inputReferences(inputPath, runnerPath, inventoryPath string) bool {
	in, err1 := filepath.Abs(inputPath)
	out, err2 := filepath.Abs(runnerPath)
	if err1 != nil || err2 != nil || in != out {
		return false
	}
	data, err := os.ReadFile(inputPath)
	if err != nil {
		return false
	}
	var head struct {
		Inventory config.Inventory `yaml:"inventory"`
	}
	if yaml.Unmarshal(data, &head) != nil || head.Inventory.File == "" || len(head.Inventory.Hosts) > 0 {
		return false
	}
	file := head.Inventory.File
	if !filepath.IsAbs(file) {
		file = filepath.Join(filepath.Dir(inputPath), file)
	}
	return filepath.Clean(file) == filepath.Clean(inventoryPath)
}

// overSSM reports whether scripts reach the Hosts through Systems Manager
// Run Command, the default remote mode (FL-010).
func overSSM(f *config.Fleet) bool { return f.Remote.Mode != config.RemoteSSH }

// checkSecretsOverSSM refuses a fleet provisioned over Systems Manager
// whose Hosts could not get their secrets. Run Command has no standard
// input, so the Hosts read the flintlockd token and TLS material from the
// Systems Manager parameters named in fleet.launch_template.parameters
// (SE-015), and every Host gets the same certificate from them: the fleet
// has to supply that material in fleet.flintlockd.tls too, so that the
// Inventory and the Pool Manager trust the CA the Hosts serve with. A CA
// generated on the Control Node could only reach the Hosts over SSH.
func checkSecretsOverSSM(f *config.Fleet) error {
	if !overSSM(f) {
		return nil
	}
	if f.LaunchTemplate == nil || f.LaunchTemplate.Parameters.HostToken == "" {
		return errors.New("fleet provision: over Systems Manager the Hosts read the flintlockd token and TLS material " +
			"from Systems Manager parameters; name them in fleet.launch_template.parameters, or provision over SSH")
	}
	tls := f.Flintlockd.TLS
	if !f.Flintlockd.Insecure && (tls.CAFile == "" || tls.CertFile == "" || tls.KeyFile == "") {
		return errors.New("fleet provision: over Systems Manager every Host serves the certificate in the " +
			"fleet.launch_template.parameters TLS parameters; set fleet.flintlockd.tls to the same CA, certificate " +
			"and key so that the Runner and the Pool Manager trust it, or provision over SSH to generate per-Host certificates")
	}
	return nil
}

// parameterSecrets is a Remote over Systems Manager for the Provisioner,
// which hands every step its secrets on standard input. Run Command has
// none, so the bundle is left behind and the scripts read each secret from
// the Systems Manager parameter named for it on the Host (SE-015).
type parameterSecrets struct{ fleet.Remote }

// Run implements fleet.Remote.
func (r parameterSecrets) Run(ctx context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	s.Stdin = nil
	return r.Remote.Run(ctx, inst, s, out)
}

// applyPoolManager installs the Pool Manager daemon on the Control Node
// with a host list generated from inv on the first run (FL-051), and
// regenerates the host list and reloads the daemon on every later one
// (FL-055); the script restarts the daemon only when the list changed.
func applyPoolManager(ctx context.Context, s fleetSeams, cfg *config.Config, out io.Writer, scr fleet.Scripts, authority ca.Authority, first bool, inv *fleet.Inventory) error {
	pm, err := s.PoolManager(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = pm.Close() }()
	ctl, err := s.control(ctx, cfg, fleet.Deps{
		Scripts:     scr,
		Inventory:   inventory.Store{},
		Certs:       authority,
		PoolManager: pm,
		Out:         out,
	})
	if err != nil {
		return err
	}
	if first {
		err = ctl.InstallPoolManager(ctx, inv)
	} else {
		err = ctl.ReloadPoolManager(ctx, inv)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "the Pool Manager daemon lists %d Host(s)\n", len(inv.Hosts))
	return nil
}

// hostFailure is the failure of one provisioning result, naming the step
// that failed when the Provisioner reports one.
func hostFailure(res fleet.HostResult) fleet.Failure {
	f := fleet.Failure{Host: res.Instance.ID, Err: res.Err}
	for _, st := range res.Steps {
		if st.Err != nil {
			// The step's own error: the summary names the Host and the
			// step already, which the Provisioner's error repeats.
			f.Step, f.Err = st.Step, st.Err
		}
	}
	if f.Err == nil {
		f.Err = errors.New("the provisioner returned no Inventory entry")
	}
	return f
}

// summarise renders failures one per line, sorted.
func summarise(command string, failures []fleet.Failure) string {
	lines := make([]string, 0, len(failures))
	for _, f := range failures {
		if f.Step != "" {
			lines = append(lines, fmt.Sprintf("  %s: step %s: %v", f.Host, f.Step, f.Err))
		} else {
			lines = append(lines, fmt.Sprintf("  %s: %v", f.Host, f.Err))
		}
	}
	sort.Strings(lines)
	return fmt.Sprintf("%s: %d failure(s):\n%s", command, len(lines), strings.Join(lines, "\n"))
}

// fleetInventory is the Inventory the verify, drain and teardown commands
// act on: the one loadFleetConfig resolved.
func fleetInventory(cfg *config.Config) *fleet.Inventory {
	return &fleet.Inventory{Hosts: cfg.Inventory.Hosts}
}

// fleetVerify is `fleet verify` (FL-070 to FL-073).
func fleetVerify(c *cli.Context, s fleetSeams) error {
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	_, err = verifyFleet(context.Background(), c.App.Writer, s, cfg, c.Bool("declare"), c.Duration("timeout"))
	return err
}

// verifyFleet verifies the Hosts of cfg's Inventory, first declaring the
// Profiles' Pools when declare is set; timeout overrides
// fleet.verification_timeout when positive. The error is the command's
// exit error.
func verifyFleet(ctx context.Context, out io.Writer, s fleetSeams, cfg *config.Config, declare bool, timeout time.Duration) (*fleet.VerifyReport, error) {
	inv := fleetInventory(cfg)
	if len(inv.Hosts) == 0 {
		return nil, cli.NewExitError(fmt.Sprintf("fleet verify: the Inventory %s lists no Host; run fleet provision first", fleetInventoryPath(cfg)), exitFleetFailure)
	}
	pm, err := s.PoolManager(cfg)
	if err != nil {
		return nil, seamErr(err)
	}
	defer func() { _ = pm.Close() }()

	if declare {
		if err := declarePools(ctx, out, pm, cfg); err != nil {
			return nil, cli.NewExitError(err.Error(), exitFleetFailure)
		}
	}
	if timeout <= 0 {
		timeout = cfg.Fleet.VerificationTimeout
	}
	v, err := verify.New(verify.Config{
		PoolManager:       pm,
		Hosts:             s.Hosts(),
		Transports:        s.Transports(),
		Profiles:          cfg.Profiles,
		Timeout:           timeout,
		TransportDeadline: cfg.Executor.TransportDeadline,
		Out:               out,
	})
	if err != nil {
		return nil, cli.NewExitError(err.Error(), exitFleetFailure)
	}
	report, err := v.Verify(ctx, inv)
	if err != nil {
		return nil, cli.NewExitError(err.Error(), exitFleetFailure)
	}
	for _, h := range report.Hosts {
		if h.Exercised {
			fmt.Fprintf(out, "%s: claim to ready %s\n", h.Host, h.ClaimToReady.Round(time.Millisecond))
		}
	}
	printHostServiceNotes(out, cfg.HostServices)
	//= docs/requirements/06-fleet.md#verification
	//# If verification fails on any Host or service, then the Fleet
	//# Controller SHALL exit with a non-zero status naming each failure and the
	//# step that failed.
	if err := verify.Summary(report); err != nil {
		return report, cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintln(out, "verification passed")
	return report, nil
}

// declarePools declares every Profile's Pool as the Runner would.
func declarePools(ctx context.Context, out io.Writer, pm poolmgr.PoolAdmin, cfg *config.Config) error {
	d := poolmgr.NewDeclarer(pm)
	for i := range cfg.Profiles {
		p := &cfg.Profiles[i]
		spec, err := poolmgr.NewSpecBuilder().Build(*p, poolmgr.SpecInput{
			RunnerName: cfg.GitLab.Name,
			Namespace:  cfg.Scheduler.Namespace,
			Hosts:      config.SelectHosts(p, cfg.Inventory.Hosts),
		})
		if err != nil {
			return fmt.Errorf("fleet verify: pool spec for profile %s: %w", p.Name, err)
		}
		if _, err := d.Declare(ctx, spec); err != nil {
			return fmt.Errorf("fleet verify: declaring pool %s: %w", spec.Ref, err)
		}
		fmt.Fprintf(out, "declared pool %s on %v\n", spec.Ref, spec.FlintlockHosts)
	}
	return nil
}

// fleetDrain is `fleet drain HOST` (FL-080, FL-084).
func fleetDrain(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	host := c.Args().First()
	if host == "" || c.NArg() != 1 {
		return cli.NewExitError("fleet drain: name exactly one Host", exitFleetFailure)
	}
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	inv := fleetInventory(cfg)
	entry, ok := config.HostByName(inv.Hosts, host)
	if !ok {
		return cli.NewExitError(fmt.Sprintf("fleet drain: %s is not in the Inventory %s", host, fleetInventoryPath(cfg)), exitFleetFailure)
	}
	pm, err := s.PoolManager(cfg)
	if err != nil {
		return seamErr(err)
	}
	defer func() { _ = pm.Close() }()

	dcfg := drain.Config{Pools: pm, Leases: s.Leases(pm), Out: c.App.Writer}
	if c.Bool("stop") {
		rem, err := s.remote(ctx, cfg)
		if err != nil {
			return seamErr(err)
		}
		scr, err := s.scripts(cfg)
		if err != nil {
			return seamErr(err)
		}
		// Stop runs only once the Drainer has seen no leased MicroVM on the
		// Host, and only then is the script told it may stop flintlockd.
		dcfg.Stop = func(ctx context.Context, _ string) error {
			return runStep(ctx, rem, scr, cfg, inv, entry, fleet.StepDrain,
				map[string]string{drain.OptionStopFlintlockd: "true"}, c.App.Writer)
		}
	}
	d, err := drain.NewDrainer(dcfg)
	if err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	timeout := cfg.Fleet.DrainTimeout
	if t := c.Duration("timeout"); t > 0 {
		timeout = t
	}
	if err := d.Drain(ctx, host, timeout); err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintf(c.App.Writer, "drained %s\n", host)
	return nil
}

// runStep renders one step's script for a Host and runs it.
func runStep(ctx context.Context, remote fleet.Remote, scripts fleet.Scripts, cfg *config.Config, inv *fleet.Inventory, h *config.HostEntry, step fleet.Step, options map[string]string, out io.Writer) error {
	in := fleet.RenderInput{Fleet: *cfg.Fleet, HostServices: cfg.HostServices, Profiles: cfg.Profiles, Instance: inventory.InstanceOf(h), Inventory: *inv, Options: options}
	script, err := scripts.Render(step, in)
	if err != nil {
		return err
	}
	res, err := remote.Run(ctx, in.Instance, script, out)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s script exited %d: %s", step, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// fleetTeardown is `fleet teardown` (FL-081 to FL-083).
func fleetTeardown(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	inv := fleetInventory(cfg)
	opts := fleet.TeardownOptions{Terminate: c.Bool("terminate"), Purge: c.Bool("purge")}
	pm, err := s.PoolManager(cfg)
	if err != nil {
		return seamErr(err)
	}
	defer func() { _ = pm.Close() }()
	rem, err := s.remote(ctx, cfg)
	if err != nil {
		return seamErr(err)
	}
	scr, err := s.scripts(cfg)
	if err != nil {
		return seamErr(err)
	}
	var ec2 fleet.EC2
	if opts.Terminate {
		if ec2, err = s.ec2(ctx, cfg); err != nil {
			return seamErr(err)
		}
	}
	// The Runner goes first, so that it neither claims from nor declares
	// the Pools being deleted.
	if err := removeRunner(ctx, s, cfg, c.App.Writer); err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet teardown: stopping the Runner on the Control Node: %v", err), exitFleetFailure)
	}
	td, err := drain.NewTeardown(drain.TeardownConfig{
		Pools:         pm,
		Profiles:      cfg.Profiles,
		Hosts:         s.Hosts(),
		Scripts:       scr,
		Remote:        rem,
		Render:        fleet.RenderInput{Fleet: *cfg.Fleet, HostServices: cfg.HostServices, Profiles: cfg.Profiles},
		EC2:           ec2,
		InventoryPath: fleetInventoryPath(cfg),
		Timeout:       cfg.Fleet.DrainTimeout,
		Out:           c.App.Writer,
	})
	if err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	if err := td.Teardown(ctx, inv, opts); err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintln(c.App.Writer, "teardown complete")
	return nil
}

// fleetEmitUserData is `fleet emit-userdata` (FL-090, FL-091): the
// launch-template user-data, gzip-compressed so that it fits EC2's limit,
// or with --plain the script itself.
func fleetEmitUserData(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	scr, err := s.scripts(cfg)
	if err != nil {
		return seamErr(err)
	}
	// With parameters the emitter checks that every one exists and that
	// no value read from them ends up in the user-data.
	params, err := s.parameters(ctx, cfg)
	if err != nil {
		return seamErr(err)
	}
	userData, err := launchtemplate.NewEmitter(cfg, scr, params).Emit(ctx)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet emit-userdata: %v", err), exitFleetFailure)
	}
	if !c.Bool("plain") {
		if userData, err = launchtemplate.Compress(userData); err != nil {
			return cli.NewExitError(fmt.Sprintf("fleet emit-userdata: %v", err), exitFleetFailure)
		}
	}
	if _, err := c.App.Writer.Write(userData); err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet emit-userdata: %v", err), exitFleetFailure)
	}
	return nil
}
