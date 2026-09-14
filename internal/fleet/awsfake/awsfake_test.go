package awsfake

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

func ids(insts []fleet.Instance) []string {
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = in.ID
	}
	return out
}

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# The project SHALL provide fakes for `DescribeInstances`,
//# `SendCommand`, `GetCommandInvocation`, `ListCommandInvocations` and
//# `GetParameter` that record every call and return results the test
//# configures.

// TestFakesRecordEveryCallAndReturnConfiguredResults drives each of the five
// faked APIs and checks both halves of TD-041: the configured result comes
// back and the call is in the record.
func TestFakesRecordEveryCallAndReturnConfiguredResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("DescribeInstances", func(t *testing.T) {
		t.Parallel()
		ec2 := NewEC2(
			fleet.Instance{ID: "i-a", Type: "m7g.metal", Tags: map[string]string{"fleet": "ci"}},
			fleet.Instance{ID: "i-b", Type: "m7g.metal", Tags: map[string]string{"fleet": "other"}},
			fleet.Instance{ID: "i-c", Type: "m7g.metal", Tags: map[string]string{"fleet": "ci"}, State: "stopped"},
			fleet.Instance{ID: "i-d", Type: "c7g.metal", Tags: map[string]string{"fleet": "ci"}},
		)
		byTag := fleet.DescribeFilter{TagKey: "fleet", TagValue: "ci"}
		got, err := ec2.DescribeInstances(ctx, byTag)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"i-a", "i-d"}; !reflect.DeepEqual(ids(got), want) {
			t.Errorf("by tag = %v, want %v", ids(got), want)
		}
		byID := fleet.DescribeFilter{InstanceIDs: []string{"i-b", "i-c"}, States: []string{"running", "stopped"}}
		got, err = ec2.DescribeInstances(ctx, byID)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"i-b", "i-c"}; !reflect.DeepEqual(ids(got), want) {
			t.Errorf("by id = %v, want %v", ids(got), want)
		}
		boom := errors.New("throttled")
		ec2.FailDescribe(boom)
		if _, err := ec2.DescribeInstances(ctx, byTag); !errors.Is(err, boom) {
			t.Errorf("injected error = %v, want %v", err, boom)
		}
		if want := []fleet.DescribeFilter{byTag, byID, byTag}; !reflect.DeepEqual(ec2.DescribeCalls(), want) {
			t.Errorf("DescribeCalls = %+v, want %+v", ec2.DescribeCalls(), want)
		}
		if err := ec2.TerminateInstances(ctx, []string{"i-a"}); err != nil {
			t.Fatal(err)
		}
		if in, _ := ec2.Instance("i-a"); in.State != StateTerminated {
			t.Errorf("i-a state = %q after terminate", in.State)
		}
		if err := ec2.TerminateInstances(ctx, []string{"i-nope"}); err == nil {
			t.Error("terminating an unknown instance succeeded")
		}
		if want := [][]string{{"i-a"}, {"i-nope"}}; !reflect.DeepEqual(ec2.TerminateCalls(), want) {
			t.Errorf("TerminateCalls = %v, want %v", ec2.TerminateCalls(), want)
		}
	})

	t.Run("SendCommand and invocations", func(t *testing.T) {
		t.Parallel()
		ssm := NewSSM()
		ssm.SetReply("i-a", Reply{ExitCode: 0, Stdout: "one\ntwo\n", Progress: []string{"one\n"}})
		ssm.SetReply("i-b", Reply{ExitCode: 3, Stderr: "nope\n"})
		in := fleet.SendCommandInput{InstanceIDs: []string{"i-a", "i-b"}, Script: "echo hi", Comment: "detect", Timeout: time.Minute}
		id, err := ssm.SendCommand(ctx, in)
		if err != nil {
			t.Fatal(err)
		}

		list, err := ssm.ListCommandInvocations(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 2 || list[0].Status != StatusInProgress || list[1].Status != StatusFailed || list[1].ResponseCode != 3 {
			t.Errorf("ListCommandInvocations = %+v", list)
		}

		first, err := ssm.GetCommandInvocation(ctx, id, "i-a")
		if err != nil {
			t.Fatal(err)
		}
		if first.Status != StatusInProgress || first.Stdout != "one\n" {
			t.Errorf("first poll = %+v, want InProgress with the progress output", first)
		}
		last, err := ssm.GetCommandInvocation(ctx, id, "i-a")
		if err != nil {
			t.Fatal(err)
		}
		want := fleet.Invocation{CommandID: id, InstanceID: "i-a", Status: StatusSuccess, Stdout: "one\ntwo\n"}
		if *last != want {
			t.Errorf("final poll = %+v, want %+v", *last, want)
		}
		failed, err := ssm.GetCommandInvocation(ctx, id, "i-b")
		if err != nil {
			t.Fatal(err)
		}
		if failed.Status != StatusFailed || failed.ResponseCode != 3 || failed.Stderr != "nope\n" {
			t.Errorf("i-b = %+v", failed)
		}
		if _, err := ssm.GetCommandInvocation(ctx, id, "i-z"); !errors.Is(err, ErrInvocationDoesNotExist) {
			t.Errorf("unknown instance err = %v", err)
		}

		wantGets := []InvocationCall{{id, "i-a"}, {id, "i-a"}, {id, "i-b"}, {id, "i-z"}}
		if !reflect.DeepEqual(ssm.InvocationCalls(), wantGets) {
			t.Errorf("InvocationCalls = %v, want %v", ssm.InvocationCalls(), wantGets)
		}
		if !reflect.DeepEqual(ssm.ListCalls(), []string{id}) {
			t.Errorf("ListCalls = %v", ssm.ListCalls())
		}
		if !reflect.DeepEqual(ssm.SentCommands(), []fleet.SendCommandInput{in}) {
			t.Errorf("SentCommands = %+v", ssm.SentCommands())
		}

		ssm.SetResponder(func(in fleet.SendCommandInput, inst string) Reply {
			return Reply{Stdout: in.Comment + "@" + inst}
		})
		id2, err := ssm.SendCommand(ctx, fleet.SendCommandInput{InstanceIDs: []string{"i-c"}, Comment: "prepull"})
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := ssm.GetCommandInvocation(ctx, id2, "i-c"); got.Stdout != "prepull@i-c" {
			t.Errorf("responder output = %q", got.Stdout)
		}
	})

	t.Run("GetParameter", func(t *testing.T) {
		t.Parallel()
		p := NewParameters(map[string]string{"/fleet/token": "s3cret"})
		v, err := p.GetParameter(ctx, "/fleet/token")
		if err != nil || v != "s3cret" {
			t.Errorf("GetParameter = %q, %v", v, err)
		}
		if _, err := p.GetParameter(ctx, "/fleet/missing"); !errors.Is(err, ErrParameterNotFound) {
			t.Errorf("missing parameter err = %v", err)
		}
		if want := []string{"/fleet/token", "/fleet/missing"}; !reflect.DeepEqual(p.ParameterCalls(), want) {
			t.Errorf("ParameterCalls = %v, want %v", p.ParameterCalls(), want)
		}
	})
}

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# The fake Systems Manager SHALL capture the script content of
//# every `SendCommand` so that tests can assert on what would have run on a
//# Host.

func TestSSMCapturesScriptContentOfEverySendCommand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ssm := NewSSM()
	scripts := []string{"#!/bin/bash\nset -euo pipefail\ncontainerd --version\n", "#!/bin/bash\nsystemctl is-active flintlockd\n"}
	boom := errors.New("undeliverable")
	for i, s := range scripts {
		if i == 1 {
			// A failed send is still what would have run; it is captured too.
			ssm.FailSend(boom)
		}
		_, _ = ssm.SendCommand(ctx, fleet.SendCommandInput{InstanceIDs: []string{"i-a"}, Script: s})
	}
	sent := ssm.SentCommands()
	if len(sent) != len(scripts) {
		t.Fatalf("captured %d commands, want %d", len(sent), len(scripts))
	}
	for i, s := range scripts {
		if sent[i].Script != s {
			t.Errorf("command %d script = %q, want %q", i, sent[i].Script, s)
		}
	}
	// The record is a copy: mutating it does not change the fake.
	sent[0].InstanceIDs[0] = "i-mutated"
	if got := ssm.SentCommands()[0].InstanceIDs[0]; got != "i-a" {
		t.Errorf("record aliased caller slice: %q", got)
	}
}
