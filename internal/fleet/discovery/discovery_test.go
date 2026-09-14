package discovery

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsfake"
)

func ids(insts []fleet.Instance) []string {
	out := make([]string, len(insts))
	for i, in := range insts {
		out[i] = in.ID
	}
	return out
}

func metal(id string, tags map[string]string) fleet.Instance {
	return fleet.Instance{ID: id, Type: "m7g.metal", Arch: config.ArchARM64, PrivateIP: "10.0.0." + id[len(id)-1:], Tags: tags}
}

var ciTag = map[string]string{"flintlock-runner": "ci"}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# The Fleet Controller SHALL discover candidate instances with the
//# EC2 `DescribeInstances` API filtered by the configured tag key and value and
//# by the `running` instance state.

func TestEC2DiscoveryFiltersByTagAndRunningState(t *testing.T) {
	t.Parallel()
	stopped := metal("i-3", ciTag)
	stopped.State = "stopped"
	ec2 := awsfake.NewEC2(
		metal("i-2", ciTag),
		metal("i-1", map[string]string{"flintlock-runner": "other"}),
		stopped,
		metal("i-0", ciTag),
	)
	d, err := New(&config.Fleet{Discovery: config.Discovery{TagKey: "flintlock-runner", TagValue: "ci"}}, ec2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-0", "i-2"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("discovered %v, want %v", ids(got), want)
	}
	want := []fleet.DescribeFilter{{TagKey: "flintlock-runner", TagValue: "ci", States: []string{"running"}}}
	if calls := ec2.DescribeCalls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("DescribeInstances calls = %+v, want %+v", calls, want)
	}
}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# Where explicit instance ids are configured, the Fleet Controller
//# SHALL use them instead of tag discovery.

func TestInstanceIDsReplaceTagDiscovery(t *testing.T) {
	t.Parallel()
	ec2 := awsfake.NewEC2(metal("i-1", ciTag), metal("i-2", nil), metal("i-3", ciTag))
	d, err := New(&config.Fleet{Discovery: config.Discovery{
		TagKey: "flintlock-runner", TagValue: "ci",
		InstanceIDs: []string{"i-2", "i-3"},
	}}, ec2)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-2", "i-3"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("discovered %v, want %v", ids(got), want)
	}
	want := []fleet.DescribeFilter{{InstanceIDs: []string{"i-2", "i-3"}, States: []string{"running"}}}
	if calls := ec2.DescribeCalls(); !reflect.DeepEqual(calls, want) {
		t.Errorf("DescribeInstances calls = %+v, want %+v (ids and no tag)", calls, want)
	}
}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# If a discovered instance's type does not end in `.metal`, then
//# the Fleet Controller SHALL exclude it and report it as unsupported because
//# KVM is unavailable.

func TestNonMetalInstancesAreExcludedAndReported(t *testing.T) {
	t.Parallel()
	virt := fleet.Instance{ID: "i-virt", Type: "m7g.16xlarge", Arch: config.ArchARM64, Tags: ciTag}
	metalish := fleet.Instance{ID: "i-metalish", Type: "m7i.metal-24xl", Arch: config.ArchAMD64, Tags: ciTag}
	ec2 := awsfake.NewEC2(metal("i-1", ciTag), virt, metalish)
	var reported []Unsupported
	d, err := New(&config.Fleet{Discovery: config.Discovery{TagKey: "flintlock-runner", TagValue: "ci"}}, ec2,
		WithUnsupported(func(u Unsupported) { reported = append(reported, u) }))
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"i-1"}; !reflect.DeepEqual(ids(got), want) {
		t.Errorf("discovered %v, want %v", ids(got), want)
	}
	if len(reported) != 2 || reported[0].Instance.ID != "i-metalish" || reported[1].Instance.ID != "i-virt" {
		t.Fatalf("reported %+v, want i-metalish and i-virt", reported)
	}
	for _, u := range reported {
		if !strings.Contains(u.Error(), "KVM is unavailable") {
			t.Errorf("report %q does not say KVM is unavailable", u.Error())
		}
	}

	// A static machine has no instance type and is kept.
	ok, bad := Supported([]fleet.Instance{{ID: "lab", Type: TypeStatic, Arch: config.ArchAMD64}})
	if len(ok) != 1 || len(bad) != 0 {
		t.Errorf("static machine: kept %v, excluded %v", ok, bad)
	}
}

//= docs/requirements/06-fleet.md#discovery
//= type=test
//# The Fleet Controller SHALL use the instance's private IP address
//# as the Host endpoint address unless an override is configured.

func TestEndpointUsesPrivateIPUnlessOverridden(t *testing.T) {
	t.Parallel()
	f := &config.Fleet{
		Flintlockd:        config.Flintlockd{Port: 9443},
		EndpointOverrides: map[string]string{"i-2": "host-2.example.internal"},
	}
	a := fleet.Instance{ID: "i-1", PrivateIP: "10.0.0.1"}
	b := fleet.Instance{ID: "i-2", PrivateIP: "10.0.0.2"}
	if got := Endpoint(a, f); got != "10.0.0.1:9443" {
		t.Errorf("Endpoint(i-1) = %q, want the private IP", got)
	}
	if got := Endpoint(b, f); got != "host-2.example.internal:9443" {
		t.Errorf("Endpoint(i-2) = %q, want the override", got)
	}
	if got := Endpoint(fleet.Instance{ID: "i-6", PrivateIP: "fd00::6"}, nil); got != "[fd00::6]:9090" {
		t.Errorf("Endpoint with defaults = %q", got)
	}
}

//= docs/requirements/10-test-doubles.md#fake-aws
//= type=test
//# The Fleet Controller SHALL provide a static discovery provider
//# that reads Hosts from a list in the configuration, so that provisioning
//# over SSH can be exercised against any Linux machine without an AWS
//# account.

// TestStaticDiscoveryReadsConfiguredHosts checks the static list wins over
// the other settings and needs no EC2 client; remote's SSH tests run
// scripts on the Instances it yields.
func TestStaticDiscoveryReadsConfiguredHosts(t *testing.T) {
	t.Parallel()
	f := &config.Fleet{Discovery: config.Discovery{
		TagKey: "flintlock-runner", TagValue: "ci",
		Static: []config.StaticHost{
			{Name: "lab-1", Address: "192.168.1.10", Arch: config.ArchAMD64, VCPU: 16, MemoryMB: 65536, Labels: map[string]string{"rack": "a"}},
			{Name: "lab-2", Address: "192.168.1.11", Arch: config.ArchARM64, VCPU: 8, MemoryMB: 32768},
		},
	}}
	d, err := New(f, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []fleet.Instance{
		{ID: "lab-1", Type: TypeStatic, Arch: config.ArchAMD64, PrivateIP: "192.168.1.10", State: "running", Tags: map[string]string{"rack": "a"}, VCPU: 16, MemoryMB: 65536},
		{ID: "lab-2", Type: TypeStatic, Arch: config.ArchARM64, PrivateIP: "192.168.1.11", State: "running", VCPU: 8, MemoryMB: 32768},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("static discovery:\n got %+v\nwant %+v", got, want)
	}
	if _, err := New(&config.Fleet{Discovery: config.Discovery{TagKey: "k", TagValue: "v"}}, nil); err == nil {
		t.Error("tag discovery without an EC2 client was accepted")
	}
}

func TestEC2DiscoveryError(t *testing.T) {
	t.Parallel()
	ec2 := awsfake.NewEC2()
	boom := errors.New("throttled")
	ec2.FailDescribe(boom)
	_, err := NewEC2ByTag(ec2, "k", "v").Discover(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}
