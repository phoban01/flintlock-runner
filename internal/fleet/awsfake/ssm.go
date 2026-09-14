package awsfake

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// Systems Manager command statuses as GetCommandInvocation reports them.
const (
	StatusPending    = "Pending"
	StatusInProgress = "InProgress"
	StatusSuccess    = "Success"
	StatusFailed     = "Failed"
	StatusTimedOut   = "TimedOut"
	StatusCancelled  = "Cancelled"
)

// ErrInvocationDoesNotExist is returned for a command id and instance id
// pair the fake has not seen, as Systems Manager's InvocationDoesNotExist.
var ErrInvocationDoesNotExist = errors.New("awsfake: InvocationDoesNotExist")

// ErrParameterNotFound is returned by GetParameter for a name the test has
// not set, as Systems Manager's ParameterNotFound.
var ErrParameterNotFound = errors.New("awsfake: ParameterNotFound")

var (
	_ fleet.SSM                = (*SSM)(nil)
	_ fleet.SSMRecorder        = (*SSM)(nil)
	_ fleet.Parameters         = (*Parameters)(nil)
	_ fleet.ParametersRecorder = (*Parameters)(nil)
)

// Reply is what the fake SSM reports for one command on one instance.
type Reply struct {
	// Status is the final status. Empty means Success when ExitCode is zero
	// and Failed otherwise.
	Status   string
	ExitCode int
	Stdout   string
	Stderr   string
	// Progress is the cumulative standard output reported by successive
	// GetCommandInvocation calls while the command is InProgress, before the
	// final status is reported. Empty means the first call already reports
	// the final status.
	Progress []string
}

func (r Reply) final() string {
	switch {
	case r.Status != "":
		return r.Status
	case r.ExitCode == 0:
		return StatusSuccess
	default:
		return StatusFailed
	}
}

// Responder computes the Reply for one instance of a SendCommand, so that a
// test can answer by script content or name (the Comment).
type Responder func(in fleet.SendCommandInput, instanceID string) Reply

// InvocationCall is one recorded GetCommandInvocation.
type InvocationCall struct {
	CommandID  string
	InstanceID string
}

type invocation struct {
	reply Reply
	polls int
}

type command struct {
	in          fleet.SendCommandInput
	invocations map[string]*invocation
}

//= docs/requirements/10-test-doubles.md#fake-aws
//# The fake Systems Manager SHALL capture the script content of
//# every `SendCommand` so that tests can assert on what would have run on a
//# Host.

// SSM is a fake fleet.SSM. SendCommand records its whole input, script
// content included, and creates one invocation per instance whose outcome
// comes from the Responder, the per-instance Reply or the default Reply, in
// that order. It is safe for concurrent use.
type SSM struct {
	mu        sync.Mutex
	seq       int
	sent      []fleet.SendCommandInput
	getCalls  []InvocationCall
	listCalls []string
	commands  map[string]*command

	responder Responder
	replies   map[string]Reply
	def       Reply
	sendErr   error
}

// NewSSM returns a fake whose commands succeed with empty output until the
// test configures otherwise.
func NewSSM() *SSM {
	return &SSM{commands: map[string]*command{}, replies: map[string]Reply{}}
}

// SetDefault sets the Reply for instances with no per-instance Reply.
func (f *SSM) SetDefault(r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.def = cloneReply(r)
}

// SetReply sets the Reply for every later command on instanceID.
func (f *SSM) SetReply(instanceID string, r Reply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies[instanceID] = cloneReply(r)
}

// SetResponder installs fn, which takes precedence over SetReply and
// SetDefault. It is called with the fake's lock released.
func (f *SSM) SetResponder(fn Responder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responder = fn
}

// FailSend makes every later SendCommand return err; nil clears it. The call
// is still recorded.
func (f *SSM) FailSend(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// SendCommand implements fleet.SSM.
func (f *SSM) SendCommand(_ context.Context, in fleet.SendCommandInput) (string, error) {
	in.InstanceIDs = slices.Clone(in.InstanceIDs)
	f.mu.Lock()
	f.sent = append(f.sent, in)
	if err := f.sendErr; err != nil {
		f.mu.Unlock()
		return "", err
	}
	if len(in.InstanceIDs) == 0 {
		f.mu.Unlock()
		return "", errors.New("awsfake: SendCommand needs at least one instance id")
	}
	f.seq++
	id := fmt.Sprintf("cmd-%08d", f.seq)
	responder, replies, def := f.responder, maps.Clone(f.replies), f.def
	f.mu.Unlock()

	cmd := &command{in: in, invocations: map[string]*invocation{}}
	for _, inst := range in.InstanceIDs {
		r, ok := replies[inst]
		switch {
		case responder != nil:
			r = responder(in, inst)
		case !ok:
			r = def
		}
		cmd.invocations[inst] = &invocation{reply: cloneReply(r)}
	}
	f.mu.Lock()
	f.commands[id] = cmd
	f.mu.Unlock()
	return id, nil
}

// GetCommandInvocation implements fleet.SSM. While the Reply has Progress
// left, each call reports InProgress with the next cumulative output; after
// that it reports the final status.
func (f *SSM) GetCommandInvocation(_ context.Context, commandID, instanceID string) (*fleet.Invocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, InvocationCall{CommandID: commandID, InstanceID: instanceID})
	cmd, ok := f.commands[commandID]
	if !ok {
		return nil, fmt.Errorf("%w: command %s", ErrInvocationDoesNotExist, commandID)
	}
	inv, ok := cmd.invocations[instanceID]
	if !ok {
		return nil, fmt.Errorf("%w: command %s on %s", ErrInvocationDoesNotExist, commandID, instanceID)
	}
	out := snapshot(commandID, instanceID, inv)
	if inv.polls < len(inv.reply.Progress) {
		inv.polls++
	}
	return &out, nil
}

// ListCommandInvocations implements fleet.SSM. It reports the current state
// of every instance of the command without advancing its Progress.
func (f *SSM) ListCommandInvocations(_ context.Context, commandID string) ([]fleet.Invocation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, commandID)
	cmd, ok := f.commands[commandID]
	if !ok {
		return nil, fmt.Errorf("%w: command %s", ErrInvocationDoesNotExist, commandID)
	}
	out := make([]fleet.Invocation, 0, len(cmd.in.InstanceIDs))
	for _, inst := range cmd.in.InstanceIDs {
		out = append(out, snapshot(commandID, inst, cmd.invocations[inst]))
	}
	return out, nil
}

func snapshot(commandID, instanceID string, inv *invocation) fleet.Invocation {
	out := fleet.Invocation{CommandID: commandID, InstanceID: instanceID}
	if inv.polls < len(inv.reply.Progress) {
		out.Status = StatusInProgress
		out.ResponseCode = -1
		out.Stdout = inv.reply.Progress[inv.polls]
		return out
	}
	out.Status = inv.reply.final()
	out.ResponseCode = inv.reply.ExitCode
	out.Stdout = inv.reply.Stdout
	out.Stderr = inv.reply.Stderr
	return out
}

// SentCommands implements fleet.SSMRecorder: every SendCommand in call
// order, with its script content.
func (f *SSM) SentCommands() []fleet.SendCommandInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.SendCommandInput, len(f.sent))
	for i, in := range f.sent {
		in.InstanceIDs = slices.Clone(in.InstanceIDs)
		out[i] = in
	}
	return out
}

// InvocationCalls returns every GetCommandInvocation in call order.
func (f *SSM) InvocationCalls() []InvocationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.getCalls)
}

// ListCalls returns the command id of every ListCommandInvocations in call
// order.
func (f *SSM) ListCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.listCalls)
}

func cloneReply(r Reply) Reply {
	r.Progress = slices.Clone(r.Progress)
	return r
}

// Parameters is a fake fleet.Parameters holding decrypted values the test
// sets. It records every GetParameter. It is safe for concurrent use.
type Parameters struct {
	mu     sync.Mutex
	values map[string]string
	calls  []string
}

// NewParameters returns a fake holding values, keyed by parameter name.
func NewParameters(values map[string]string) *Parameters {
	v := maps.Clone(values)
	if v == nil {
		v = map[string]string{}
	}
	return &Parameters{values: v}
}

// Set stores value under name.
func (p *Parameters) Set(name, value string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.values[name] = value
}

// GetParameter implements fleet.Parameters.
func (p *Parameters) GetParameter(_ context.Context, name string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, name)
	v, ok := p.values[name]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrParameterNotFound, name)
	}
	return v, nil
}

// ParameterCalls implements fleet.ParametersRecorder.
func (p *Parameters) ParameterCalls() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}
