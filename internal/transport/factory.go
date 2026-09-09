package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

// FactoryOption configures the Factory built by NewFactory.
type FactoryOption func(*factory)

// WithClock makes the Transports time their liveness watch against clk
// instead of the wall clock (EX-051), so that tests advance a fake clock
// rather than wait.
func WithClock(clk clock.Clock) FactoryOption {
	return func(f *factory) {
		if clk != nil {
			f.clk = clk
		}
	}
}

// WithLogger sets the logger the Transports report on. The default
// discards.
func WithLogger(log *slog.Logger) FactoryOption {
	return func(f *factory) {
		if log != nil {
			f.log = log
		}
	}
}

// NewFactory returns the production Factory.
func NewFactory(opts ...FactoryOption) Factory {
	f := &factory{clk: clock.Real{}, log: slog.New(slog.DiscardHandler)}
	for _, o := range opts {
		o(f)
	}
	return f
}

// factory is the production Factory.
type factory struct {
	clk clock.Clock
	log *slog.Logger
}

//= docs/requirements/02-executor.md#guest-transport
//# The Guest Transport SHALL be selectable per Profile from the
//# implementations `exec` and `ssh`, with `exec` as the default.

// New implements Factory. The Target's Kind picks the implementation and an
// empty Kind is exec, which is what a Profile that says nothing about its
// transport gets; any other value is a configuration error rather than a
// silent fallback. Nothing in the guest is contacted here: New only checks
// that the Host serves the guest-agent service the chosen transport needs
// (HO-012), so that a Job placed on a Host without it fails with a reason
// instead of a stream error.
func (f *factory) New(ctx context.Context, target Target) (Transport, error) {
	if target.Host == nil {
		return nil, errors.New("transport: target has no host client")
	}
	if target.VMUID == "" {
		return nil, fmt.Errorf("transport: host %s: target has no microvm uid", target.Host.Name())
	}

	switch target.Kind {
	case KindExec, "":
		if err := f.checkService(ctx, target, KindExec); err != nil {
			return nil, err
		}
		return &execTransport{target: target, clk: f.clk, log: f.log}, nil
	case KindSSH:
		if err := f.checkService(ctx, target, KindSSH); err != nil {
			return nil, err
		}
		return newSSHTransport(target, f.clk, f.log)
	default:
		return nil, fmt.Errorf("transport: unknown guest transport %q, expected %q or %q",
			target.Kind, KindExec, KindSSH)
	}
}

//= docs/requirements/05-hosts.md#inventory
//# If a Host reports that the exec service is disabled, then the
//# Runner SHALL log a warning naming the Host, because Jobs the Pool Manager
//# places there cannot be run over the `exec` Guest Transport.

// checkService asks the Host whether the guest-agent service the transport
// needs is enabled. A Host that says it is disabled gets a warning naming
// it and ErrServiceDisabled, because no Job can run there over this
// transport. A Host that does not implement ServerInfo says nothing either
// way and the transport is built (HO-013); so does a Host that fails the
// probe, because the operation itself will report the real failure with a
// better message than a refusal to build the transport would.
func (f *factory) checkService(ctx context.Context, target Target, kind Kind) error {
	info, err := target.Host.ServerInfo(ctx)
	if err != nil {
		if !errors.Is(err, flintlock.ErrUnimplemented) {
			f.log.Warn("could not read host capabilities; building the guest transport anyway",
				"host", target.Host.Name(), "transport", string(kind), "error", err)
		}
		return nil
	}
	service, name := info.Exec, "exec"
	if kind == KindSSH {
		service, name = info.SSHProxy, "ssh proxy"
	}
	if service.Enabled {
		return nil
	}
	f.log.Warn("host has the guest-agent service disabled; jobs placed there cannot run over this guest transport",
		"host", target.Host.Name(), "service", name, "transport", string(kind))
	return fmt.Errorf("transport: host %s has the %s service disabled: %w",
		target.Host.Name(), name, ErrServiceDisabled)
}

// Compile-time check.
var _ Factory = (*factory)(nil)
