package discovery

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsclient"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
)

const refreshEvery = 5 * time.Minute

func launchTemplateFleet() *config.Fleet {
	return &config.Fleet{
		Region:    "eu-west-1",
		Discovery: config.Discovery{TagKey: "flintlock-runner", TagValue: "ci", InstanceIDs: []string{"i-ignored"}},
		LaunchTemplate: &config.LaunchTemplate{
			InventoryRefreshInterval: refreshEvery,
		},
	}
}

// fakeFactory hands out ec2 and counts how often a client was built.
type fakeFactory struct {
	ec2    *awsfake.EC2
	builds int
}

func (f *fakeFactory) build(context.Context, string) (Describer, error) {
	f.builds++
	return f.ec2, nil
}

//= docs/requirements/06-fleet.md#launch-template-mode
//= type=test
//# Where launch template mode is selected, the Runner SHALL refresh
//# its Inventory from tag discovery at the configured interval so that
//# self-provisioned Hosts join without a restart.

// TestRefresherPicksUpNewHostsAtTheInterval runs the refresher on a fake
// clock: a Host that appears under the tag is delivered after one interval,
// and an unchanged set is not delivered again.
func TestRefresherPicksUpNewHostsAtTheInterval(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	clk := clock.NewFake(start)
	ec2 := awsfake.NewEC2(metal("i-1", ciTag), metal("i-2", nil))
	factory := &fakeFactory{ec2: ec2}
	changes := make(chan []string, 4)
	r, err := NewRefresher(ctx, launchTemplateFleet(), func(_ context.Context, insts []fleet.Instance) error {
		changes <- ids(insts)
		return nil
	}, WithClock(clk), WithDescriberFactory(factory.build))
	if err != nil || r == nil {
		t.Fatalf("NewRefresher = %v, %v", r, err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	if got := <-changes; !reflect.DeepEqual(got, []string{"i-1"}) {
		t.Fatalf("first refresh = %v, want [i-1]", got)
	}
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := clk.Deadlines(); !reflect.DeepEqual(got, []time.Time{start.Add(refreshEvery)}) {
		t.Fatalf("next refresh at %v, want one interval later", got)
	}

	// A self-provisioned Host appears under the tag.
	ec2.Put(metal("i-3", ciTag))
	clk.Advance(refreshEvery)
	if got := <-changes; !reflect.DeepEqual(got, []string{"i-1", "i-3"}) {
		t.Fatalf("second refresh = %v, want [i-1 i-3]", got)
	}

	// Nothing changes: the loop runs again but delivers nothing.
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	calls := len(ec2.DescribeCalls())
	// Advance fires and removes the pending timer, so the next pending timer
	// is the one armed after the refresh has finished.
	clk.Advance(refreshEvery)
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if got := len(ec2.DescribeCalls()); got != calls+1 {
		t.Fatalf("DescribeInstances calls = %d, want %d", got, calls+1)
	}
	select {
	case got := <-changes:
		t.Errorf("unchanged set delivered again: %v", got)
	default:
	}

	for _, c := range ec2.DescribeCalls() {
		if c.TagKey != "flintlock-runner" || c.TagValue != "ci" || len(c.InstanceIDs) != 0 {
			t.Errorf("refresh used %+v, want tag discovery", c)
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
}

//= docs/requirements/09-security.md#least-privilege
//= type=test
//# The Runner SHALL NOT require any AWS permission at run time
//# unless launch template mode's Inventory refresh is enabled, in which case
//# it SHALL require only `ec2:DescribeInstances`.

// TestRunnerNeedsAWSOnlyForRefresh checks that without refresh no AWS
// client is built, and that with it every call the Runner makes is in the
// Runner's published policy.
func TestRunnerNeedsAWSOnlyForRefresh(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	noop := func(context.Context, []fleet.Instance) error { return nil }

	disabled := map[string]*config.Fleet{
		"no fleet section":      nil,
		"no launch template":    {Discovery: config.Discovery{TagKey: "k", TagValue: "v"}},
		"refresh interval zero": {Discovery: config.Discovery{TagKey: "k", TagValue: "v"}, LaunchTemplate: &config.LaunchTemplate{}},
	}
	for name, f := range disabled {
		factory := &fakeFactory{ec2: awsfake.NewEC2()}
		r, err := NewRefresher(ctx, f, noop, WithDescriberFactory(factory.build))
		if r != nil || err != nil || factory.builds != 0 {
			t.Errorf("%s: NewRefresher = %v, %v with %d AWS clients built; want none", name, r, err, factory.builds)
		}
	}

	ec2 := awsfake.NewEC2(metal("i-1", ciTag))
	factory := &fakeFactory{ec2: ec2}
	rctx, cancel := context.WithCancel(ctx)
	r, err := NewRefresher(rctx, launchTemplateFleet(), func(context.Context, []fleet.Instance) error {
		cancel()
		return nil
	}, WithDescriberFactory(factory.build), WithClock(clock.NewFake(time.Unix(0, 0))))
	if err != nil || factory.builds != 1 {
		t.Fatalf("enabled: NewRefresher err %v, %d clients built", err, factory.builds)
	}
	_ = r.Run(rctx)

	var used []string
	for range ec2.DescribeCalls() {
		used = append(used, awsclient.ActionDescribeInstances)
	}
	for range ec2.TerminateCalls() {
		used = append(used, awsclient.ActionTerminateInstances)
	}
	if len(used) == 0 {
		t.Fatal("refresh made no call")
	}
	allowed := awsclient.RunnerRefreshPolicy.Actions()
	for _, a := range used {
		if !slices.Contains(allowed, a) {
			t.Errorf("Runner used %s, outside its policy %v", a, allowed)
		}
	}
	if err := (describeOnly{ec2}).TerminateInstances(ctx, []string{"i-1"}); err == nil || len(ec2.TerminateCalls()) != 0 {
		t.Error("the refresher's client can terminate instances")
	}
}
