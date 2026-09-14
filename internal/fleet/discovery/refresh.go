package discovery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/awsclient"
)

// Describer is the one AWS call the Runner may make, and only for launch
// template mode's Inventory refresh (SE-041).
type Describer interface {
	DescribeInstances(ctx context.Context, f fleet.DescribeFilter) ([]fleet.Instance, error)
}

// DescriberFactory builds the Describer for a region. The default loads the
// SDK's default credential chain and returns awsclient.EC2 narrowed to
// Describer.
type DescriberFactory func(ctx context.Context, region string) (Describer, error)

func defaultFactory(ctx context.Context, region string) (Describer, error) {
	cfg, err := awsclient.LoadConfig(ctx, region)
	if err != nil {
		return nil, err
	}
	return awsclient.NewEC2(cfg), nil
}

// RefreshEnabled reports whether launch template mode's Inventory refresh is
// on for f: launch template mode is selected with a positive refresh interval.
// When it is off the Runner builds no AWS client at all (SE-041).
func RefreshEnabled(f *config.Fleet) bool {
	return f != nil && f.LaunchTemplate != nil && f.LaunchTemplate.InventoryRefreshInterval > 0
}

// OnChange receives the supported instances matching the discovery tag every
// time that set changes, starting with the first refresh.
type OnChange func(ctx context.Context, insts []fleet.Instance) error

// Refresher re-runs tag discovery at the configured interval for the Runner
// in launch template mode, so that self-provisioned Hosts join without a
// restart (FL-092).
type Refresher struct {
	disc     fleet.Discovery
	interval time.Duration
	clock    clock.Clock
	onChange OnChange
	logger   *slog.Logger
}

// RefreshOption configures NewRefresher.
type RefreshOption func(*refreshOptions)

type refreshOptions struct {
	clock   clock.Clock
	factory DescriberFactory
	logger  *slog.Logger
}

// WithClock sets the clock the interval is measured on.
func WithClock(c clock.Clock) RefreshOption { return func(o *refreshOptions) { o.clock = c } }

// WithDescriberFactory replaces how the Describer is built.
func WithDescriberFactory(fn DescriberFactory) RefreshOption {
	return func(o *refreshOptions) { o.factory = fn }
}

// WithLogger sets the logger for refresh failures and unsupported instances.
func WithLogger(l *slog.Logger) RefreshOption { return func(o *refreshOptions) { o.logger = l } }

//= docs/requirements/09-security.md#least-privilege
//# The Runner SHALL NOT require any AWS permission at run time
//# unless launch template mode's Inventory refresh is enabled, in which case
//# it SHALL require only `ec2:DescribeInstances`.

// NewRefresher returns the Runner's Inventory refresher for f, or nil and no
// error when refresh is not enabled, in which case no AWS client is built
// and no credential is looked up. When enabled, the only client built is a
// Describer, so DescribeInstances is the only call the Runner can make.
func NewRefresher(ctx context.Context, f *config.Fleet, onChange OnChange, opts ...RefreshOption) (*Refresher, error) {
	if !RefreshEnabled(f) {
		return nil, nil
	}
	if onChange == nil {
		return nil, errors.New("discovery: refresher needs an OnChange")
	}
	if f.Discovery.TagKey == "" || f.Discovery.TagValue == "" {
		return nil, errors.New("discovery: launch template mode's Inventory refresh needs fleet.discovery.tag_key and tag_value")
	}
	o := refreshOptions{clock: clock.Real{}, factory: defaultFactory, logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	d, err := o.factory(ctx, f.Region)
	if err != nil {
		return nil, fmt.Errorf("discovery: build EC2 client for Inventory refresh: %w", err)
	}
	logger := o.logger
	return &Refresher{
		disc: &Filtered{
			Discovery: NewEC2ByTag(describeOnly{d}, f.Discovery.TagKey, f.Discovery.TagValue),
			Report: func(u Unsupported) {
				logger.Warn("inventory refresh: excluding unsupported instance", "instance", u.Instance.ID, "type", u.Instance.Type, "reason", u.Reason)
			},
		},
		interval: f.LaunchTemplate.InventoryRefreshInterval,
		clock:    o.clock,
		onChange: onChange,
		logger:   logger,
	}, nil
}

//= docs/requirements/06-fleet.md#launch-template-mode
//# Where launch template mode is selected, the Runner SHALL refresh
//# its Inventory from tag discovery at the configured interval so that
//# self-provisioned Hosts join without a restart.

// Run refreshes at once and then every interval until ctx is done, calling
// OnChange whenever the set of supported instances differs from the last one
// delivered. A failed discovery or OnChange is logged and retried at the next
// interval; the Runner keeps its current Inventory meanwhile. Run returns
// ctx's error.
func (r *Refresher) Run(ctx context.Context) error {
	var last []fleet.Instance
	delivered := false
	for {
		insts, err := r.disc.Discover(ctx)
		switch {
		case err != nil:
			r.logger.Warn("inventory refresh failed", "err", err)
		case delivered && reflect.DeepEqual(insts, last):
		default:
			if err := r.onChange(ctx, insts); err != nil {
				r.logger.Warn("inventory refresh: applying discovered hosts failed", "err", err)
			} else {
				last, delivered = insts, true
			}
		}
		t := r.clock.NewTimer(r.interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C():
		}
	}
}

// describeOnly hides every method but DescribeInstances, so that a Describer
// that happens to be a full fleet.EC2 still cannot be asked for more.
type describeOnly struct{ d Describer }

// DescribeInstances implements fleet.EC2 by calling the Describer.
func (d describeOnly) DescribeInstances(ctx context.Context, f fleet.DescribeFilter) ([]fleet.Instance, error) {
	return d.d.DescribeInstances(ctx, f)
}

// TerminateInstances implements fleet.EC2 by refusing.
func (d describeOnly) TerminateInstances(context.Context, []string) error {
	return errors.New("discovery: the Runner may not terminate instances")
}
