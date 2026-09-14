package awsfake

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// StateRunning and StateTerminated are the EC2 instance states the fake
// understands; any other string is stored and filtered on as given.
const (
	StateRunning    = "running"
	StateTerminated = "terminated"
)

var (
	_ fleet.EC2         = (*EC2)(nil)
	_ fleet.EC2Recorder = (*EC2)(nil)
)

//= docs/requirements/10-test-doubles.md#fake-aws
//# The project SHALL provide fakes for `DescribeInstances`,
//# `SendCommand`, `GetCommandInvocation`, `ListCommandInvocations` and
//# `GetParameter` that record every call and return results the test
//# configures.

// EC2 is a fake fleet.EC2. It holds the instances the test configures,
// answers DescribeInstances by applying the filter the way EC2 applies its
// tag, instance-id and instance-state-name filters, and records every call.
// It is safe for concurrent use.
type EC2 struct {
	mu        sync.Mutex
	instances []fleet.Instance
	describe  []fleet.DescribeFilter
	terminate [][]string

	describeErr  error
	terminateErr error
}

// NewEC2 returns a fake holding insts. An instance with an empty State is
// stored as running.
func NewEC2(insts ...fleet.Instance) *EC2 {
	f := &EC2{}
	for _, in := range insts {
		f.Put(in)
	}
	return f
}

// Put adds inst, or replaces the stored instance with the same ID.
func (f *EC2) Put(inst fleet.Instance) {
	inst = cloneInstance(inst)
	if inst.State == "" {
		inst.State = StateRunning
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.instances {
		if f.instances[i].ID == inst.ID {
			f.instances[i] = inst
			return
		}
	}
	f.instances = append(f.instances, inst)
}

// Remove deletes the instance with id, as if it no longer existed.
func (f *EC2) Remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances = slices.DeleteFunc(f.instances, func(in fleet.Instance) bool { return in.ID == id })
}

// Instance returns the stored instance with id.
func (f *EC2) Instance(id string) (fleet.Instance, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, in := range f.instances {
		if in.ID == id {
			return cloneInstance(in), true
		}
	}
	return fleet.Instance{}, false
}

// FailDescribe makes every later DescribeInstances return err; nil clears it.
// The call is still recorded.
func (f *EC2) FailDescribe(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describeErr = err
}

// FailTerminate makes every later TerminateInstances return err; nil clears
// it. The call is still recorded.
func (f *EC2) FailTerminate(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminateErr = err
}

// DescribeInstances implements fleet.EC2. Filters combine as EC2's do: an
// instance matches when it carries the tag (if TagKey is set), is one of
// InstanceIDs (if any) and is in one of States, which defaults to running.
// Instances are returned in the order they were added.
func (f *EC2) DescribeInstances(_ context.Context, flt fleet.DescribeFilter) ([]fleet.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.describe = append(f.describe, cloneFilter(flt))
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	states := flt.States
	if len(states) == 0 {
		states = []string{StateRunning}
	}
	var out []fleet.Instance
	for _, in := range f.instances {
		if flt.TagKey != "" {
			v, ok := in.Tags[flt.TagKey]
			if !ok || (flt.TagValue != "" && v != flt.TagValue) {
				continue
			}
		}
		if len(flt.InstanceIDs) > 0 && !slices.Contains(flt.InstanceIDs, in.ID) {
			continue
		}
		if !slices.Contains(states, in.State) {
			continue
		}
		out = append(out, cloneInstance(in))
	}
	return out, nil
}

// TerminateInstances implements fleet.EC2. Known instances move to the
// terminated state; an unknown id is an error, as it is in EC2.
func (f *EC2) TerminateInstances(_ context.Context, ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminate = append(f.terminate, slices.Clone(ids))
	if f.terminateErr != nil {
		return f.terminateErr
	}
	for _, id := range ids {
		i := slices.IndexFunc(f.instances, func(in fleet.Instance) bool { return in.ID == id })
		if i < 0 {
			return fmt.Errorf("awsfake: InvalidInstanceID.NotFound: %s", id)
		}
	}
	for _, id := range ids {
		i := slices.IndexFunc(f.instances, func(in fleet.Instance) bool { return in.ID == id })
		f.instances[i].State = StateTerminated
	}
	return nil
}

// DescribeCalls implements fleet.EC2Recorder.
func (f *EC2) DescribeCalls() []fleet.DescribeFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.DescribeFilter, len(f.describe))
	for i, c := range f.describe {
		out[i] = cloneFilter(c)
	}
	return out
}

// TerminateCalls implements fleet.EC2Recorder.
func (f *EC2) TerminateCalls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.terminate))
	for i, c := range f.terminate {
		out[i] = slices.Clone(c)
	}
	return out
}

func cloneInstance(in fleet.Instance) fleet.Instance {
	in.Tags = maps.Clone(in.Tags)
	return in
}

func cloneFilter(f fleet.DescribeFilter) fleet.DescribeFilter {
	f.InstanceIDs = slices.Clone(f.InstanceIDs)
	f.States = slices.Clone(f.States)
	return f
}
