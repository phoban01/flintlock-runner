package opstest

import (
	"context"
	"fmt"
	"io"
	"maps"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// Discovery is a fleet.Discovery returning a fixed, changeable list.
type Discovery struct {
	mu        sync.Mutex
	instances []fleet.Instance
	err       error
}

// SetInstances replaces what Discover returns.
func (d *Discovery) SetInstances(insts ...fleet.Instance) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.instances = append([]fleet.Instance(nil), insts...)
}

// SetErr makes Discover fail.
func (d *Discovery) SetErr(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

// Discover implements fleet.Discovery.
func (d *Discovery) Discover(context.Context) ([]fleet.Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	return append([]fleet.Instance(nil), d.instances...), nil
}

// RenderCall is one Scripts.Render call.
type RenderCall struct {
	Step  fleet.Step
	Input fleet.RenderInput
}

// Scripts is a fleet.Scripts that renders `echo <step>` and records every
// call.
type Scripts struct {
	mu    sync.Mutex
	calls []RenderCall
}

// Render implements fleet.Scripts.
func (s *Scripts) Render(step fleet.Step, in fleet.RenderInput) (fleet.Script, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in.Options = maps.Clone(in.Options)
	s.calls = append(s.calls, RenderCall{Step: step, Input: in})
	return fleet.Script{Name: string(step), Content: "echo " + string(step) + "\n"}, nil
}

// All implements fleet.Scripts.
func (s *Scripts) All() []fleet.Step { return []fleet.Step{fleet.StepDrain, fleet.StepTeardown} }

// Calls returns every Render call so far.
func (s *Scripts) Calls() []RenderCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RenderCall(nil), s.calls...)
}

// RunCall is one Remote.Run call.
type RunCall struct {
	Instance fleet.Instance
	Script   string
}

// Remote is a fleet.Remote that records every script and succeeds, except
// on instances listed in Fail.
type Remote struct {
	mu    sync.Mutex
	calls []RunCall
	// Fail maps an instance id to the exit code its scripts return.
	Fail map[string]int
}

// Run implements fleet.Remote.
func (r *Remote) Run(_ context.Context, inst fleet.Instance, s fleet.Script, out io.Writer) (*fleet.RunResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, RunCall{Instance: inst, Script: s.Name})
	code := r.Fail[inst.ID]
	r.mu.Unlock()
	if out != nil {
		fmt.Fprintf(out, "[%s] %s\n", inst.ID, s.Name)
	}
	res := &fleet.RunResult{ExitCode: code, Stdout: s.Name + "\n"}
	if code != 0 {
		res.Stderr = "scripted failure"
	}
	return res, nil
}

// Calls returns every Run call so far.
func (r *Remote) Calls() []RunCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RunCall(nil), r.calls...)
}

// EC2 is a fleet.EC2 and fleet.EC2Recorder that records calls.
type EC2 struct {
	mu        sync.Mutex
	describe  []fleet.DescribeFilter
	terminate [][]string
}

// DescribeInstances implements fleet.EC2; it returns nothing.
func (e *EC2) DescribeInstances(_ context.Context, f fleet.DescribeFilter) ([]fleet.Instance, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.describe = append(e.describe, f)
	return nil, nil
}

// TerminateInstances implements fleet.EC2.
func (e *EC2) TerminateInstances(_ context.Context, ids []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.terminate = append(e.terminate, append([]string(nil), ids...))
	return nil
}

// DescribeCalls implements fleet.EC2Recorder.
func (e *EC2) DescribeCalls() []fleet.DescribeFilter {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]fleet.DescribeFilter(nil), e.describe...)
}

// TerminateCalls implements fleet.EC2Recorder.
func (e *EC2) TerminateCalls() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][]string(nil), e.terminate...)
}

// Provisioner is a fleet.Provisioner that records which instances it was
// asked to provision and returns an entry built by Entry, or fails for the
// instances listed in Fail.
type Provisioner struct {
	mu   sync.Mutex
	seen []string
	// Entry builds the entry for an instance; nil means one named after it
	// with versions recorded.
	Entry func(fleet.Instance) config.HostEntry
	// Fail lists instance ids whose provisioning fails.
	Fail map[string]bool
}

// Provision implements fleet.Provisioner.
func (p *Provisioner) Provision(_ context.Context, inst fleet.Instance) fleet.HostResult {
	p.mu.Lock()
	p.seen = append(p.seen, inst.ID)
	fail := p.Fail[inst.ID]
	build := p.Entry
	p.mu.Unlock()
	if fail {
		return fleet.HostResult{Instance: inst, Err: fmt.Errorf("scripted provisioning failure on %s", inst.ID),
			Steps: []fleet.StepResult{{Step: fleet.StepFlintlock, Err: fmt.Errorf("exit 1")}}}
	}
	var e config.HostEntry
	if build != nil {
		e = build(inst)
	} else {
		e = config.HostEntry{Name: inst.ID, Versions: config.InstalledVersions{Flintlock: "v0.9.0", Containerd: "v1.7.0"}}
	}
	return fleet.HostResult{Instance: inst, Entry: &e}
}

// Seen returns the ids of every instance provisioned so far, in call order.
func (p *Provisioner) Seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

// Compile-time interface checks.
var (
	_ fleet.Discovery   = (*Discovery)(nil)
	_ fleet.Scripts     = (*Scripts)(nil)
	_ fleet.Remote      = (*Remote)(nil)
	_ fleet.EC2         = (*EC2)(nil)
	_ fleet.EC2Recorder = (*EC2)(nil)
	_ fleet.Provisioner = (*Provisioner)(nil)
)
