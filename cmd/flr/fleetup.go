package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/urfave/cli"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/fleet/remote"
	"github.com/phoban01/flintlock-runner/internal/fleet/scripts"
)

//= docs/requirements/06-fleet.md#one-shot-operation
//# Where the dry-run flag is passed, the provision command SHALL
//# report each instance it would provision, each Inventory Host it would leave
//# unchanged and each Host it would remove from the Inventory, and SHALL NOT
//# contact any Host, write the Inventory or the Runner configuration, or change
//# the Pool Manager.

// report prints the plan of a dry run. planProvision has contacted no Host,
// written nothing and not touched the Pool Manager, and the caller stops
// here.
func (p *provisionPlan) report(out io.Writer) {
	fmt.Fprintln(out, "dry run: no Host or Pool Manager is contacted and nothing is written")
	for _, inst := range p.plan.New {
		fmt.Fprintf(out, "would provision %s\n", describeInstance(inst))
	}
	for _, inst := range p.plan.Known {
		fmt.Fprintf(out, "would leave %s alone: it is already in the Inventory\n", inst.ID)
	}
	for _, h := range p.plan.Removed {
		fmt.Fprintf(out, "would remove %s (instance %s) from the Inventory: the instance no longer exists\n", h.Name, inventory.InstanceID(&h))
	}
	fmt.Fprintf(out, "plan: %d to provision, %d already in the Inventory, %d to remove\n",
		len(p.plan.New), len(p.plan.Known), len(p.plan.Removed))
	if len(p.plan.New) > 0 {
		fmt.Fprintln(out, "KVM is checked on each instance while it is provisioned, so a dry run cannot tell "+
			"which of the instances to provision would be excluded for want of it")
	}
}

// describeInstance names an instance with what discovery knows of it.
func describeInstance(inst fleet.Instance) string {
	var about []string
	for _, v := range []string{inst.Type, string(inst.Arch), inst.PrivateIP} {
		if v != "" {
			about = append(about, v)
		}
	}
	if len(about) == 0 {
		return inst.ID
	}
	return fmt.Sprintf("%s (%s)", inst.ID, strings.Join(about, ", "))
}

// runCommand is the command that starts the Runner with the Runner
// configuration at path.
func runCommand(path string) string {
	return "flr --config " + path + " run"
}

// upSummary is the one summary fleet up prints at the end: a line per
// step, with the continuation lines of a multi-line outcome indented.
type upSummary struct {
	lines []string
}

func (u *upSummary) add(step, format string, args ...any) {
	text := fmt.Sprintf(format, args...)
	text = strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n             ")
	u.lines = append(u.lines, fmt.Sprintf("  %-10s %s", step, text))
}

func (u *upSummary) String() string {
	return "fleet up summary:\n" + strings.Join(u.lines, "\n")
}

// failed is the exit error of a fleet up whose step failed with err: the
// summary, with the exit status of the step's own error.
func (u *upSummary) failed(err error) error {
	code := exitFleetFailure
	var ec cli.ExitCoder
	if errors.As(err, &ec) && ec.ExitCode() != 0 {
		code = ec.ExitCode()
	}
	return cli.NewExitError(u.String(), code)
}

//= docs/requirements/06-fleet.md#one-shot-operation
//# The Fleet Controller SHALL provide an up command that runs the
//# provision command, then runs the verification command with the Profiles'
//# Pools declared against the Runner configuration that provisioning wrote,
//# and reports the outcome of both in one summary.

// fleetUp is `fleet up`: fleet provision, then fleet verify --declare
// against the Runner configuration provisioning wrote, then, with
// --install-runner, the Runner's service on the Control Node. It prints one
// summary at the end; when a step failed the summary is the exit message.
func fleetUp(c *cli.Context, s fleetSeams) error {
	ctx := context.Background()
	out := remote.SyncWriter(c.App.Writer)
	install := c.Bool("install-runner")
	var sum upSummary

	p, err := planProvision(ctx, c, s, out)
	if err != nil {
		sum.add("provision", "failed: %v", err)
		sum.add("verify", "not run, because provisioning failed")
		return sum.failed(err)
	}
	runnerPath, err := filepath.Abs(p.cfg.Fleet.RunnerConfigPath)
	if err != nil {
		return cli.NewExitError(fmt.Sprintf("fleet up: %v", err), exitFleetFailure)
	}
	if c.Bool("dry-run") {
		p.reportUp(out, runnerPath, install)
		return nil
	}

	outcome, err := p.apply(ctx, c, s, out)
	//= docs/requirements/06-fleet.md#one-shot-operation
	//# If provisioning fails, then the up command SHALL NOT run
	//# verification and SHALL exit with a non-zero status.
	if err != nil {
		sum.add("provision", "failed: %v", err)
		sum.add("verify", "not run, because provisioning failed")
		if install {
			sum.add("runner", "not installed, because provisioning failed")
		}
		return sum.failed(err)
	}
	sum.add("provision", "ok: the Inventory %s lists %d Host(s); %d provisioned, %d already there, %d removed",
		fleetInventoryPath(p.cfg), len(outcome.inventory.Hosts), outcome.provisioned, len(p.plan.Known), len(p.plan.Removed))

	fmt.Fprintf(out, "verifying the fleet against %s\n", runnerPath)
	vcfg, err := loadFleetConfigAt(c, runnerPath, nil, true)
	var report *fleet.VerifyReport
	if err == nil {
		report, err = verifyFleet(ctx, out, s, vcfg, true, c.Duration("timeout"))
	}
	//= docs/requirements/06-fleet.md#one-shot-operation
	//# If verification fails, then the up command SHALL exit with a
	//# non-zero status.
	if err != nil {
		sum.add("verify", "failed: %v", err)
		if install {
			sum.add("runner", "not installed, because verification failed")
		} else {
			sum.add("runner", "not started; once verification passes, start it with: %s", runCommand(runnerPath))
		}
		return sum.failed(err)
	}
	sum.add("verify", "ok: %d Host(s) and %d Pool(s) passed", len(report.Hosts), len(report.Pools))

	if install {
		if err := installRunner(ctx, c, s, p.cfg, runnerPath, out); err != nil {
			sum.add("runner", "failed: %v", err)
			return sum.failed(err)
		}
		sum.add("runner", "ok: the systemd service %s is active, running %s", scripts.RunnerUnit, runCommand(runnerPath))
	} else {
		//= docs/requirements/06-fleet.md#one-shot-operation
		//# Where the install-runner flag is not passed, the up command
		//# SHALL print the command that starts the Runner with the generated Runner
		//# configuration.
		sum.add("runner", "not installed; start it with: %s", runCommand(runnerPath))
	}
	fmt.Fprintln(out, sum.String())
	return nil
}

//= docs/requirements/06-fleet.md#one-shot-operation
//# Where the dry-run flag is passed, the up command SHALL report
//# the provision plan and the steps it would run after provisioning, and
//# SHALL NOT provision, verify or install anything.

// reportUp prints the plan of fleet up --dry-run: the provision plan and
// the steps after it. Nothing has been changed and nothing is.
func (p *provisionPlan) reportUp(out io.Writer, runnerPath string, install bool) {
	p.report(out)
	fmt.Fprintln(out, "then fleet up would:")
	fmt.Fprintf(out, "  write the Inventory %s and the Runner configuration %s\n", fleetInventoryPath(p.cfg), runnerPath)
	if len(p.current.Hosts) == 0 {
		fmt.Fprintln(out, "  install the Pool Manager daemon on the Control Node with the provisioned Hosts")
	} else {
		fmt.Fprintln(out, "  reload the Pool Manager daemon on the Control Node with the new host list")
	}
	fmt.Fprintf(out, "  run fleet verify --declare against %s\n", runnerPath)
	if install {
		fmt.Fprintf(out, "  install %q as the systemd service %s on the Control Node, enable and start it and check that it is active\n",
			runCommand(runnerPath), scripts.RunnerUnit)
	} else {
		fmt.Fprintf(out, "  print the command that starts the Runner: %s\n", runCommand(runnerPath))
	}
}

// installRunner installs the Runner as a systemd service on the Control
// Node with the runner step (FL-122). The step's options come from the
// Runner configuration at runnerPath loaded as the service will load it,
// without the environment of this command.
func installRunner(ctx context.Context, c *cli.Context, s fleetSeams, cfg *config.Config, runnerPath string, out io.Writer) error {
	rcfg, err := config.Load(runnerPath,
		config.WithEnv(func(string) (string, bool) { return "", false }),
		config.WithLogger(slog.New(slog.NewTextHandler(c.App.ErrWriter, nil))))
	if err != nil {
		return fmt.Errorf("the Runner configuration %s does not load without this command's environment, "+
			"which the service will not have: %w", runnerPath, err)
	}
	if s.Executable == nil {
		return errors.New("no flr binary to install")
	}
	bin, err := s.Executable()
	if err != nil {
		return fmt.Errorf("finding the flr binary: %w", err)
	}
	if bin, err = filepath.EvalSymlinks(bin); err != nil {
		return fmt.Errorf("finding the flr binary: %w", err)
	}
	stateDir := rcfg.StateDir
	if !filepath.IsAbs(stateDir) {
		return fmt.Errorf("state_dir %q is relative; the service needs an absolute path", stateDir)
	}
	fmt.Fprintf(out, "installing the Runner as the systemd service %s on the Control Node\n", scripts.RunnerUnit)
	return controlStep(ctx, s, cfg, fleet.StepRunner, map[string]string{
		scripts.OptionRunnerConfig:      runnerPath,
		scripts.OptionRunnerBinary:      bin,
		scripts.OptionRunnerStateDir:    stateDir,
		scripts.OptionRunnerReads:       strings.Join(runnerReads(rcfg, runnerPath), "\n"),
		scripts.OptionRunnerStopTimeout: (rcfg.GitLab.ShutdownTimeout + 30*time.Second).String(),
	}, out)
}

// removeRunner stops and disables the Runner's service on the Control Node
// when fleet up --install-runner installed it there.
func removeRunner(ctx context.Context, s fleetSeams, cfg *config.Config, out io.Writer) error {
	if s.RunnerInstalled == nil || !s.RunnerInstalled() {
		return nil
	}
	return controlStep(ctx, s, cfg, fleet.StepRunner, map[string]string{scripts.OptionRunnerRemove: "true"}, out)
}

// controlStep renders step for the Control Node and runs it there.
func controlStep(ctx context.Context, s fleetSeams, cfg *config.Config, step fleet.Step, options map[string]string, out io.Writer) error {
	if s.ControlRemote == nil {
		return errors.New("fleet: no way to run scripts on the Control Node")
	}
	scr, err := s.scripts(cfg)
	if err != nil {
		return err
	}
	self := fleet.Instance{ID: controlNodeName, Arch: config.Architecture(runtime.GOARCH), State: "running"}
	script, err := scr.Render(step, fleet.RenderInput{Fleet: *cfg.Fleet, Instance: self, Options: options})
	if err != nil {
		return err
	}
	res, err := s.ControlRemote.Run(ctx, self, script, out)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s script exited %d: %s", step, res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}

// runnerReads lists the files the Runner reads: its configuration and every
// file its configuration names, found as the non-empty string fields whose
// YAML name is "file" or ends in "_file", the Inventory's Hosts included.
// The fleet section is left out: the Runner reads no file from it, and it
// names the Hosts' server keys, which the Runner has no business reading.
// Relative paths are taken against the configuration's directory.
func runnerReads(cfg *config.Config, path string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" {
			return
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		if p = filepath.Clean(p); !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add(path)
	c := *cfg
	c.Fleet = nil
	walkFileFields(reflect.ValueOf(c), add)
	return out
}

// walkFileFields calls add with every file-naming string field under v.
func walkFileFields(v reflect.Value, add func(string)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkFileFields(v.Elem(), add)
		}
	case reflect.Struct:
		t := v.Type()
		for i := range v.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if f.Type.Kind() == reflect.String && (name == "file" || strings.HasSuffix(name, "_file")) {
				add(v.Field(i).String())
				continue
			}
			walkFileFields(v.Field(i), add)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkFileFields(v.Index(i), add)
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			walkFileFields(iter.Value(), add)
		}
	default:
	}
}
