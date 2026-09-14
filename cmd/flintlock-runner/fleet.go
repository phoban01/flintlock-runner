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
	"sync"
	"time"

	"github.com/urfave/cli"
	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/ca"
	"github.com/phoban01/flintlock-runner/internal/fleet/drain"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/verify"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// exitFleetFailure is the exit status of a fleet subcommand that ran and
// found failures (FL-013, FL-071).
const exitFleetFailure = 1

// fleetSeams are the constructors of every dependency the fleet subcommands
// take. The concrete Discovery, Remote, Scripts, Provisioner, EC2 and
// Control Node installer are built by the access and provisioning work
// packages; until they land their constructors report which package brings
// them. Tests replace every seam with a fake.
type fleetSeams struct {
	// Discovery finds candidate instances (FL-001, FL-002, TD-044).
	Discovery func(cfg *config.Config) (fleet.Discovery, error)
	// Remote runs scripts on instances (FL-010, FL-011); out receives the
	// streamed output.
	Remote func(cfg *config.Config, out io.Writer) (fleet.Remote, error)
	// Scripts renders the provisioning, drain and teardown scripts.
	Scripts func(cfg *config.Config) (fleet.Scripts, error)
	// Provisioner provisions one instance end to end with deps.
	Provisioner func(cfg *config.Config, deps fleet.Deps) (fleet.Provisioner, error)
	// EC2 is only needed by teardown --terminate.
	EC2 func(cfg *config.Config) (fleet.EC2, error)
	// Control installs and reloads the Pool Manager daemon (FL-051,
	// FL-055). A nil ControlNode skips the reload.
	Control func(cfg *config.Config, deps fleet.Deps) (fleet.ControlNode, error)

	// PoolManager, Hosts and Transports are the Runner's own clients.
	PoolManager func(cfg *config.Config) (poolmgr.Client, error)
	Hosts       func() flintlock.Dialer
	Transports  func() transport.Factory
	// Leases counts leased MicroVMs on a drained Host.
	Leases func(pm poolmgr.Client) drain.LeaseReporter
	// Now is the clock for the Inventory's generated_at.
	Now func() time.Time
}

// errNotWired is what a seam returns until its work package lands.
func errNotWired(what, workPackage string) error {
	return fmt.Errorf("%w: the %s lands with the %q work package (docs/PLAN.md)", ErrNotImplemented, what, workPackage)
}

// productionSeams are the seams of the released binary.
//
// Wiring left for the lead when wp/fleet-access and wp/fleet-provision
// merge: Discovery, Remote and EC2 come from internal/fleet/discovery,
// internal/fleet/remote and internal/fleet/awsclient; Scripts, Provisioner
// and Control from internal/fleet/scripts and internal/fleet/provision.
func productionSeams() fleetSeams {
	return fleetSeams{
		Discovery: func(*config.Config) (fleet.Discovery, error) {
			return nil, errNotWired("instance discovery", "fleet-access")
		},
		Remote: func(*config.Config, io.Writer) (fleet.Remote, error) {
			return nil, errNotWired("remote execution", "fleet-access")
		},
		Scripts: func(*config.Config) (fleet.Scripts, error) {
			return nil, errNotWired("provisioning scripts", "fleet-provision")
		},
		Provisioner: func(*config.Config, fleet.Deps) (fleet.Provisioner, error) {
			return nil, errNotWired("host provisioner", "fleet-provision")
		},
		EC2: func(*config.Config) (fleet.EC2, error) {
			return nil, errNotWired("EC2 client", "fleet-access")
		},
		Control: func(*config.Config, fleet.Deps) (fleet.ControlNode, error) {
			return nil, nil
		},
		PoolManager: func(cfg *config.Config) (poolmgr.Client, error) {
			return poolmgr.NewClient(poolmgr.ClientConfig{
				Endpoint: cfg.PoolManager.Endpoint,
				TLS:      cfg.PoolManager.TLS,
				Deadline: cfg.PoolManager.Deadline,
			})
		},
		Hosts:      func() flintlock.Dialer { return flintlock.NewDialer() },
		Transports: func() transport.Factory { return transport.NewFactory() },
		Leases:     func(pm poolmgr.Client) drain.LeaseReporter { return drain.PoolLeases{Pools: pm} },
		Now:        time.Now,
	}
}

// fleetCommands registers the Fleet Controller subcommands (06-fleet.md).
func fleetCommands(s fleetSeams) []cli.Command {
	return []cli.Command{
		{
			Name:   "provision",
			Usage:  "discover instances, provision the new ones as Hosts and write the Inventory and Runner configuration",
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
			Name:  "emit-userdata",
			Usage: "print the launch-template user-data script (FL-090)",
			Action: func(*cli.Context) error {
				return notImplemented("fleet emit-userdata", "fleet-provision")
			},
		},
	}
}

// seamErr turns a seam's error into the command's exit error.
func seamErr(err error) error {
	if errors.Is(err, ErrNotImplemented) {
		return cli.NewExitError(err.Error(), exitNotImplemented)
	}
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
	path := c.GlobalString("config")
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

// isMetal reports whether an instance can run KVM. Static hosts are
// whatever the operator listed. This is the minimum FL-003 filter the
// provisioning flow needs; the discovery package may replace it.
func isMetal(inst fleet.Instance) bool {
	return inst.Type == "static" || inst.Type == "" || strings.HasSuffix(inst.Type, ".metal")
}

// fleetProvision is `fleet provision`: discover, provision only the new
// instances, merge them into the Inventory, drop the vanished ones, and
// write the Inventory and the Runner configuration.
func fleetProvision(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	out := c.App.Writer
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	store := inventory.Store{}
	invPath := fleetInventoryPath(cfg)
	current, err := store.Load(ctx, invPath)
	if err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}

	disc, err := s.Discovery(cfg)
	if err != nil {
		return seamErr(err)
	}
	discovered, err := disc.Discover(ctx)
	if err != nil {
		// Without a complete discovery nothing can be said to have gone;
		// the Inventory is left as it is.
		return cli.NewExitError(fmt.Sprintf("fleet provision: discovery: %v", err), exitFleetFailure)
	}
	plan := inventory.Diff(current, discovered)
	//= docs/requirements/06-fleet.md#inventory-and-runner-configuration
	//# When run again after an instance in the Inventory no longer
	//# exists, the Fleet Controller SHALL remove it from the Inventory and report
	//# the removal.
	for _, h := range plan.Removed {
		fmt.Fprintf(out, "removed %s (instance %s) from the Inventory: the instance no longer exists\n", h.Name, inventory.InstanceID(&h))
	}
	var candidates []fleet.Instance
	for _, inst := range plan.New {
		if !isMetal(inst) {
			fmt.Fprintf(out, "excluded %s: instance type %s is not bare metal, so KVM is unavailable\n", inst.ID, inst.Type)
			continue
		}
		candidates = append(candidates, inst)
	}
	for _, inst := range plan.Known {
		fmt.Fprintf(out, "%s is already in the Inventory; not provisioned again\n", inst.ID)
	}

	var failures []fleet.Failure
	var added []config.HostEntry
	if len(candidates) > 0 {
		results, err := provisionNew(ctx, c, s, cfg, candidates)
		if err != nil {
			return err
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
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintf(out, "wrote the Inventory %s with %d Host(s)\n", invPath, len(merged.Hosts))
	if err := writeRunnerConfig(ctx, c, cfg, merged); err != nil {
		failures = append(failures, fleet.Failure{Host: "runner configuration", Err: err})
	}
	if err := reloadPoolManager(ctx, s, cfg, merged); err != nil {
		failures = append(failures, fleet.Failure{Host: "control node", Step: fleet.StepControlNode, Err: err})
	}
	if len(failures) > 0 {
		return cli.NewExitError(summarise("fleet provision", failures), exitFleetFailure)
	}
	return nil
}

// provisionNew runs the Provisioner over instances with the configured
// parallelism, collecting every result in the order of instances.
func provisionNew(ctx context.Context, c *cli.Context, s fleetSeams, cfg *config.Config, instances []fleet.Instance) ([]fleet.HostResult, error) {
	authority := certificateAuthority(cfg)
	if !cfg.Fleet.Flintlockd.Insecure {
		if _, err := authority.Ensure(ctx, tlsDir(cfg), instances); err != nil {
			return nil, cli.NewExitError(fmt.Sprintf("fleet provision: TLS material: %v", err), exitFleetFailure)
		}
	}
	remote, err := s.Remote(cfg, c.App.Writer)
	if err != nil {
		return nil, seamErr(err)
	}
	scripts, err := s.Scripts(cfg)
	if err != nil {
		return nil, seamErr(err)
	}
	prov, err := s.Provisioner(cfg, fleet.Deps{
		Remote:    remote,
		Scripts:   scripts,
		Inventory: inventory.Store{},
		Certs:     authority,
		Out:       c.App.Writer,
	})
	if err != nil {
		return nil, seamErr(err)
	}
	results := make([]fleet.HostResult, len(instances))
	limit := max(cfg.Fleet.Parallelism, 1)
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, inst := range instances {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = prov.Provision(ctx, inst)
			if results[i].Instance.ID == "" {
				results[i].Instance = inst
			}
		}()
	}
	wg.Wait()
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
	return entry, nil
}

// writeRunnerConfig writes the Runner configuration (FL-062). It is
// generated from the configuration as written, without environment
// overrides, and validated against the merged Inventory.
func writeRunnerConfig(ctx context.Context, c *cli.Context, cfg *config.Config, merged *fleet.Inventory) error {
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
		fmt.Fprintf(c.App.Writer, "the Runner configuration %s already references the Inventory\n", path)
		return nil
	}
	if err := (inventory.Store{}).SaveRunnerConfig(ctx, path, inventory.RunnerConfig(input, fleetInventoryPath(cfg))); err != nil {
		return err
	}
	fmt.Fprintf(c.App.Writer, "wrote the Runner configuration %s\n", path)
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

// reloadPoolManager regenerates the Pool Manager's host list when a Control
// Node installer is wired.
func reloadPoolManager(ctx context.Context, s fleetSeams, cfg *config.Config, inv *fleet.Inventory) error {
	if s.Control == nil {
		return nil
	}
	ctl, err := s.Control(cfg, fleet.Deps{Inventory: inventory.Store{}})
	if err != nil || ctl == nil {
		return err
	}
	return ctl.ReloadPoolManager(ctx, inv)
}

// hostFailure is the failure of one provisioning result, naming the step
// that failed when the Provisioner reports one.
func hostFailure(res fleet.HostResult) fleet.Failure {
	f := fleet.Failure{Host: res.Instance.ID, Err: res.Err}
	for _, st := range res.Steps {
		if st.Err != nil {
			f.Step = st.Step
			if f.Err == nil {
				f.Err = st.Err
			}
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
	ctx := context.Background()
	cfg, err := loadFleetConfig(c, nil, true)
	if err != nil {
		return err
	}
	inv := fleetInventory(cfg)
	if len(inv.Hosts) == 0 {
		return cli.NewExitError(fmt.Sprintf("fleet verify: the Inventory %s lists no Host; run fleet provision first", fleetInventoryPath(cfg)), exitFleetFailure)
	}
	pm, err := s.PoolManager(cfg)
	if err != nil {
		return seamErr(err)
	}
	defer func() { _ = pm.Close() }()

	if c.Bool("declare") {
		if err := declarePools(ctx, c.App.Writer, pm, cfg); err != nil {
			return cli.NewExitError(err.Error(), exitFleetFailure)
		}
	}
	timeout := cfg.Fleet.VerificationTimeout
	if d := c.Duration("timeout"); d > 0 {
		timeout = d
	}
	v, err := verify.New(verify.Config{
		PoolManager:       pm,
		Hosts:             s.Hosts(),
		Transports:        s.Transports(),
		Profiles:          cfg.Profiles,
		Timeout:           timeout,
		TransportDeadline: cfg.Executor.TransportDeadline,
		Out:               c.App.Writer,
	})
	if err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	report, err := v.Verify(ctx, inv)
	if err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	for _, h := range report.Hosts {
		if h.Exercised {
			fmt.Fprintf(c.App.Writer, "%s: claim to ready %s\n", h.Host, h.ClaimToReady.Round(time.Millisecond))
		}
	}
	printHostServiceNotes(c.App.Writer, cfg.HostServices)
	//= docs/requirements/06-fleet.md#verification
	//# If verification fails on any Host or service, then the Fleet
	//# Controller SHALL exit with a non-zero status naming each failure and the
	//# step that failed.
	if err := verify.Summary(report); err != nil {
		return cli.NewExitError(err.Error(), exitFleetFailure)
	}
	fmt.Fprintln(c.App.Writer, "verification passed")
	return nil
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
		remote, err := s.Remote(cfg, c.App.Writer)
		if err != nil {
			return seamErr(err)
		}
		scripts, err := s.Scripts(cfg)
		if err != nil {
			return seamErr(err)
		}
		// Stop runs only once the Drainer has seen no leased MicroVM on the
		// Host, and only then is the script told it may stop flintlockd.
		dcfg.Stop = func(ctx context.Context, _ string) error {
			return runStep(ctx, remote, scripts, cfg, inv, entry, fleet.StepDrain,
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
	remote, err := s.Remote(cfg, c.App.Writer)
	if err != nil {
		return seamErr(err)
	}
	scripts, err := s.Scripts(cfg)
	if err != nil {
		return seamErr(err)
	}
	var ec2 fleet.EC2
	if opts.Terminate {
		if ec2, err = s.EC2(cfg); err != nil {
			return seamErr(err)
		}
	}
	td, err := drain.NewTeardown(drain.TeardownConfig{
		Pools:         pm,
		Profiles:      cfg.Profiles,
		Hosts:         s.Hosts(),
		Scripts:       scripts,
		Remote:        remote,
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
