package discovery

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"slices"
	"strconv"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// TypeStatic is the Instance.Type of a machine from the static list
// (TD-044), which has no EC2 instance type.
const TypeStatic = "static"

// stateRunning is the only instance state provisioned (FL-001).
const stateRunning = "running"

var (
	_ fleet.Discovery = (*EC2)(nil)
	_ fleet.Discovery = (*Static)(nil)
	_ fleet.Discovery = (*Filtered)(nil)
)

// EC2 discovers instances through the EC2 DescribeInstances API, by tag or
// by explicit instance ids.
type EC2 struct {
	client   fleet.EC2
	tagKey   string
	tagValue string
	ids      []string
}

// NewEC2ByTag discovers running instances carrying tag key=value (FL-001).
func NewEC2ByTag(client fleet.EC2, key, value string) *EC2 {
	return &EC2{client: client, tagKey: key, tagValue: value}
}

// NewEC2ByIDs discovers the running instances among ids (FL-002).
func NewEC2ByIDs(client fleet.EC2, ids []string) *EC2 {
	return &EC2{client: client, ids: slices.Clone(ids)}
}

//= docs/requirements/06-fleet.md#discovery
//# The Fleet Controller SHALL discover candidate instances with the
//# EC2 `DescribeInstances` API filtered by the configured tag key and value and
//# by the `running` instance state.

//= docs/requirements/06-fleet.md#discovery
//# Where explicit instance ids are configured, the Fleet Controller
//# SHALL use them instead of tag discovery.

// Discover implements fleet.Discovery. With ids it asks for exactly those
// instances and no tag; otherwise it asks for the tag. Either way only the
// running state is requested. Instances come back sorted by id.
func (d *EC2) Discover(ctx context.Context) ([]fleet.Instance, error) {
	f := fleet.DescribeFilter{States: []string{stateRunning}}
	switch {
	case len(d.ids) > 0:
		f.InstanceIDs = slices.Clone(d.ids)
	case d.tagKey != "" && d.tagValue != "":
		f.TagKey, f.TagValue = d.tagKey, d.tagValue
	default:
		return nil, errors.New("discovery: EC2 discovery needs a tag key and value or instance ids")
	}
	insts, err := d.client.DescribeInstances(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("discover instances: %w", err)
	}
	// Defend against a client that ignores the state filter: only running
	// instances are candidates.
	insts = slices.DeleteFunc(insts, func(in fleet.Instance) bool { return in.State != "" && in.State != stateRunning })
	slices.SortFunc(insts, func(a, b fleet.Instance) int { return cmp.Compare(a.ID, b.ID) })
	return insts, nil
}

//= docs/requirements/10-test-doubles.md#fake-aws
//# The Fleet Controller SHALL provide a static discovery provider
//# that reads Hosts from a list in the configuration, so that provisioning
//# over SSH can be exercised against any Linux machine without an AWS
//# account.

// Static discovers the machines listed in the configuration. It makes no
// AWS call. Each machine becomes an Instance whose ID is its name, Type is
// TypeStatic, PrivateIP is its address and Tags are its labels.
type Static struct {
	hosts []config.StaticHost
}

// NewStatic returns a provider for hosts.
func NewStatic(hosts []config.StaticHost) *Static {
	return &Static{hosts: slices.Clone(hosts)}
}

// Discover implements fleet.Discovery, in configuration order.
func (s *Static) Discover(context.Context) ([]fleet.Instance, error) {
	out := make([]fleet.Instance, 0, len(s.hosts))
	for _, h := range s.hosts {
		if h.Name == "" || h.Address == "" {
			return nil, fmt.Errorf("discovery: static host %q needs a name and an address", h.Name)
		}
		out = append(out, fleet.Instance{
			ID:        h.Name,
			Type:      TypeStatic,
			Arch:      h.Arch,
			PrivateIP: h.Address,
			State:     stateRunning,
			Tags:      maps.Clone(h.Labels),
			VCPU:      h.VCPU,
			MemoryMB:  h.MemoryMB,
		})
	}
	return out, nil
}

// Unsupported is a discovered instance the Fleet Controller will not
// provision, with the reason.
type Unsupported struct {
	Instance fleet.Instance
	Reason   string
}

func (u Unsupported) Error() string {
	return fmt.Sprintf("instance %s (%s) is unsupported: %s", u.Instance.ID, u.Instance.Type, u.Reason)
}

//= docs/requirements/06-fleet.md#discovery
//# The Fleet Controller SHALL NOT exclude a discovered instance
//# from provisioning because of its instance type.

// Supported splits insts into those that can be provisioned and those that
// cannot: an instance whose architecture is neither amd64 nor arm64 has no
// binaries. The instance type is not looked at. KVM comes with bare-metal
// types and, where nested virtualization is enabled, with some virtualized
// ones, so the type name does not say whether an instance has it; the
// Provisioner checks /dev/kvm on the Host instead (FL-117).
func Supported(insts []fleet.Instance) ([]fleet.Instance, []Unsupported) {
	var ok []fleet.Instance
	var bad []Unsupported
	for _, in := range insts {
		if in.Arch != config.ArchAMD64 && in.Arch != config.ArchARM64 {
			bad = append(bad, Unsupported{Instance: in, Reason: fmt.Sprintf("architecture %q is not amd64 or arm64", in.Arch)})
			continue
		}
		ok = append(ok, in)
	}
	return ok, bad
}

// Filtered wraps a Discovery so that Discover returns only supported
// instances and reports every other one to Report.
type Filtered struct {
	Discovery fleet.Discovery
	// Report is called once per unsupported instance per Discover.
	Report func(Unsupported)
}

// Discover implements fleet.Discovery.
func (f *Filtered) Discover(ctx context.Context) ([]fleet.Instance, error) {
	insts, err := f.Discovery.Discover(ctx)
	if err != nil {
		return nil, err
	}
	ok, bad := Supported(insts)
	for _, u := range bad {
		if f.Report != nil {
			f.Report(u)
		}
	}
	return ok, nil
}

// Option configures New.
type Option func(*options)

type options struct {
	report func(Unsupported)
}

// WithUnsupported sets the function every unsupported instance is reported
// to. The default logs a warning with slog.Default.
func WithUnsupported(fn func(Unsupported)) Option {
	return func(o *options) { o.report = fn }
}

// New returns the Discovery for f: the static list when it is configured,
// then explicit instance ids, then the tag, as config.Discovery documents.
// The result reports and drops unsupported instances. client may be
// nil for the static list, which never calls AWS.
func New(f *config.Fleet, client fleet.EC2, opts ...Option) (fleet.Discovery, error) {
	if f == nil {
		return nil, errors.New("discovery: no fleet section")
	}
	o := options{report: func(u Unsupported) {
		slog.Default().Warn("excluding unsupported instance", "instance", u.Instance.ID, "type", u.Instance.Type, "reason", u.Reason)
	}}
	for _, opt := range opts {
		opt(&o)
	}
	d := f.Discovery
	var inner fleet.Discovery
	switch {
	case len(d.Static) > 0:
		inner = NewStatic(d.Static)
	case client == nil:
		return nil, errors.New("discovery: EC2 discovery needs an EC2 client")
	case len(d.InstanceIDs) > 0:
		inner = NewEC2ByIDs(client, d.InstanceIDs)
	case d.TagKey != "" && d.TagValue != "":
		inner = NewEC2ByTag(client, d.TagKey, d.TagValue)
	default:
		return nil, errors.New("discovery: configure fleet.discovery with a tag, instance ids or a static list")
	}
	return &Filtered{Discovery: inner, Report: o.report}, nil
}

//= docs/requirements/06-fleet.md#discovery
//# The Fleet Controller SHALL use the instance's private IP address
//# as the Host endpoint address unless an override is configured.

// EndpointAddress is the address the Control Node reaches inst at: the
// override configured for its id, else its private IP.
func EndpointAddress(inst fleet.Instance, overrides map[string]string) string {
	if a, ok := overrides[inst.ID]; ok && a != "" {
		return a
	}
	return inst.PrivateIP
}

// Endpoint is inst's flintlockd endpoint, host:port, for the Inventory
// (FL-060) and the Pool Manager host list (FL-051).
func Endpoint(inst fleet.Instance, f *config.Fleet) string {
	port := config.DefaultFlintlockdPort
	var overrides map[string]string
	if f != nil {
		overrides = f.EndpointOverrides
		if f.Flintlockd.Port != 0 {
			port = f.Flintlockd.Port
		}
	}
	return net.JoinHostPort(EndpointAddress(inst, overrides), strconv.Itoa(port))
}
