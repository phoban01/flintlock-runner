package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/phoban01/flintlock-runner/internal/clock"
	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/fleet"
	"github.com/phoban01/flintlock-runner/internal/fleet/inventory"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// The steps a verification failure names (FL-071).
const (
	// StepServerInfo is the ServerInfo call on a Host (FL-070).
	StepServerInfo fleet.Step = "server_info"
	// StepExecService is the exec service check on a Host (FL-070).
	StepExecService fleet.Step = "exec_service"
	// StepPool is the Pool Manager's listing of a declared Pool at its
	// target size (FL-070).
	StepPool fleet.Step = "pool"
	// StepExercise is claiming, running a trivial command in and releasing
	// a MicroVM on a Host (FL-072, FL-073).
	StepExercise fleet.Step = "exercise"
)

// defaultPollInterval is how often a Pool below its target size, or a Pool
// with no warm MicroVM to claim, is asked again.
const defaultPollInterval = 250 * time.Millisecond

// PoolManager is what verification needs of the Pool Manager: the Pool
// listing and the Lease service the Runner itself claims through.
type PoolManager interface {
	poolmgr.PoolAdmin
	poolmgr.Lease
}

// Config is what New needs.
type Config struct {
	// PoolManager is required.
	PoolManager PoolManager
	// Hosts dials a Host by its Inventory entry. Required. The production
	// value is the Runner's own flintlock.Dialer, so verification reaches
	// Hosts exactly as the Runner does.
	Hosts flintlock.Dialer
	// Transports builds the Guest Transport, as the Executor does. Required.
	Transports transport.Factory
	// Profiles are the declared Profiles; each one's Pool is checked and
	// exercised. Defaults have to be applied.
	Profiles []config.Profile
	// Timeout is the verification timeout (fleet.verification_timeout). It
	// bounds the wait for each Pool to reach its target size and, separately,
	// the exercise of each Pool (FL-072).
	Timeout time.Duration
	// TransportDeadline is the Guest Transport deadline (EX-051).
	TransportDeadline time.Duration
	// PollInterval defaults to 250ms.
	PollInterval time.Duration
	// Clock measures claim to readiness; nil means the real clock.
	Clock clock.Clock
	// Out receives one progress line per step; nil discards.
	Out io.Writer
}

// Verifier is the fleet.Verifier.
type Verifier struct {
	cfg Config
}

var _ fleet.Verifier = (*Verifier)(nil)

// New checks cfg and returns a Verifier.
func New(cfg Config) (*Verifier, error) {
	switch {
	case cfg.PoolManager == nil:
		return nil, errors.New("verify: a Pool Manager client is required")
	case cfg.Hosts == nil:
		return nil, errors.New("verify: a Host dialer is required")
	case cfg.Transports == nil:
		return nil, errors.New("verify: a transport factory is required")
	case cfg.Timeout <= 0:
		return nil, errors.New("verify: the verification timeout has to be positive")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Out == nil {
		cfg.Out = io.Discard
	}
	return &Verifier{cfg: cfg}, nil
}

// run is the state of one Verify call.
type run struct {
	v       *Verifier
	report  *fleet.VerifyReport
	clients map[string]flintlock.HostClient
	byHost  map[string]*fleet.HostVerification
}

//= docs/requirements/06-fleet.md#verification
//# The Fleet Controller SHALL provide a verification command that
//# checks every Host answers `ServerInfo` with the exec service enabled and
//# that the Pool Manager lists every declared Pool at its target size.

// Verify checks every Host in inv and every declared Pool, then exercises a
// MicroVM on every Host of every Pool. Failures are in the report, one per
// Host and step; the error is for a verification that could not run at all.
func (v *Verifier) Verify(ctx context.Context, inv *fleet.Inventory) (*fleet.VerifyReport, error) {
	if inv == nil {
		return nil, errors.New("verify: nil Inventory")
	}
	r := &run{
		v:       v,
		report:  &fleet.VerifyReport{},
		clients: map[string]flintlock.HostClient{},
		byHost:  map[string]*fleet.HostVerification{},
	}
	defer r.close()

	r.report.Hosts = make([]fleet.HostVerification, len(inv.Hosts))
	for i := range inv.Hosts {
		r.report.Hosts[i].Host = inv.Hosts[i].Name
		r.byHost[inv.Hosts[i].Name] = &r.report.Hosts[i]
	}
	for i := range inv.Hosts {
		r.checkHost(ctx, &inv.Hosts[i])
	}
	pools := r.checkPools(ctx)
	for i, p := range v.cfg.Profiles {
		if pools[i] == nil {
			continue
		}
		r.exercise(ctx, &p, pools[i])
	}
	return r.report, nil
}

func (r *run) close() {
	for _, c := range r.clients {
		_ = c.Close()
	}
}

func (r *run) fail(host string, step fleet.Step, err error) {
	r.report.Failures = append(r.report.Failures, fleet.Failure{Host: host, Step: step, Err: err})
	fmt.Fprintf(r.v.cfg.Out, "FAIL %s %s: %v\n", host, step, err)
}

// checkHost dials a Host and asks ServerInfo for the exec service.
func (r *run) checkHost(ctx context.Context, h *config.HostEntry) {
	hv := r.byHost[h.Name]
	client, err := r.v.cfg.Hosts.Dial(ctx, inventory.Endpoint(h))
	if err != nil {
		r.fail(h.Name, StepServerInfo, fmt.Errorf("dialing %s: %w", h.Endpoint, err))
		return
	}
	r.clients[h.Name] = client
	info, err := client.ServerInfo(ctx)
	if err != nil {
		r.fail(h.Name, StepServerInfo, fmt.Errorf("ServerInfo at %s: %w", h.Endpoint, err))
		return
	}
	hv.Info = info
	hv.ExecEnabled = info.Exec.Enabled
	if !info.Exec.Enabled {
		r.fail(h.Name, StepExecService, errors.New("the flintlockd exec service is disabled; the exec Guest Transport cannot reach guests on this Host"))
		return
	}
	fmt.Fprintf(r.v.cfg.Out, "ok   %s %s: flintlock %s, exec enabled\n", h.Name, StepServerInfo, info.Version)
}

// checkPools waits, up to the verification timeout, for the Pool Manager to
// list every declared Pool at its target size. It returns each Profile's
// Pool as last listed, nil for one the Pool Manager does not list.
func (r *run) checkPools(ctx context.Context) []*poolmgr.Pool {
	profiles := r.v.cfg.Profiles
	out := make([]*poolmgr.Pool, len(profiles))
	if len(profiles) == 0 {
		return out
	}
	wctx, cancel := context.WithTimeout(ctx, r.v.cfg.Timeout)
	defer cancel()
	var lastErr error
	for first := true; ; first = false {
		var iterErr error
		listed := map[poolmgr.PoolRef]*poolmgr.Pool{}
		for _, ns := range namespaces(profiles) {
			// The first listing runs on ctx so that a short timeout still
			// gets one complete answer; later ones stop at the timeout.
			lctx := wctx
			if first {
				lctx = ctx
			}
			pools, err := r.v.cfg.PoolManager.ListPools(lctx, ns)
			if err != nil {
				iterErr = err
				continue
			}
			for _, p := range pools {
				listed[p.Spec.Ref] = p
			}
		}
		if iterErr != nil && !first {
			// A later listing that fails leaves the previous answer standing.
			// The timeout is the usual cause, and wctx.Err() cannot be relied
			// on to say so: gRPC enforces the deadline with a timer of its own
			// and can fail the call a moment before the context reports that
			// it has expired, which used to replace a good answer with the
			// error. A Pool Manager that is briefly unreachable is the same.
			if !sleep(wctx, r.v.cfg.PollInterval) {
				break
			}
			continue
		}
		lastErr = iterErr
		done := lastErr == nil
		for i := range profiles {
			out[i] = listed[refOf(&profiles[i])]
			if out[i] == nil || out[i].Status.Available < int32(profiles[i].Pool.Size) {
				done = false
			}
		}
		if done || !sleep(wctx, r.v.cfg.PollInterval) {
			break
		}
	}
	for i := range profiles {
		p := &profiles[i]
		ref := refOf(p)
		pv := fleet.PoolVerification{Pool: ref, TargetSize: int32(p.Pool.Size)}
		switch pool := out[i]; {
		case pool == nil && lastErr != nil:
			r.fail(ref.String(), StepPool, fmt.Errorf("ListPools: %w", lastErr))
		case pool == nil:
			r.fail(ref.String(), StepPool, errors.New("the Pool Manager does not list this declared Pool; start the Runner to declare it, or pass --declare"))
		default:
			pv.Available = pool.Status.Available
			pv.AtTargetSize = pool.Status.Available >= pv.TargetSize
			if !pv.AtTargetSize {
				r.fail(ref.String(), StepPool, fmt.Errorf("%d of %d MicroVMs available after %s", pv.Available, pv.TargetSize, r.v.cfg.Timeout))
			} else {
				fmt.Fprintf(r.v.cfg.Out, "ok   %s %s: %d of %d available\n", ref, StepPool, pv.Available, pv.TargetSize)
			}
		}
		r.report.Pools = append(r.report.Pools, pv)
	}
	return out
}

//= docs/requirements/06-fleet.md#verification
//# The verification command SHALL claim warm MicroVMs from every
//# Pool, run a trivial command in each through the Guest Transport and
//# release them, continuing until it has exercised every Host in the Pool's
//# host list or the verification timeout elapses, and SHALL report the time
//# from claim to readiness per Host.

//= docs/requirements/06-fleet.md#verification
//# The verification command SHALL report any Host in a Pool's host
//# list on which no MicroVM could be exercised within the verification
//# timeout.

// exercise claims MicroVMs from one Pool until every Host in its host list
// has run a trivial command, or the verification timeout elapses. A claim
// that lands on a Host already exercised is held rather than released, so
// that the Pool Manager has to hand out a MicroVM elsewhere next; every
// lease is released before exercise returns. Each Host not exercised in
// time is reported with the last error seen for it.
func (r *run) exercise(ctx context.Context, p *config.Profile, pool *poolmgr.Pool) {
	ref := refOf(p)
	hosts := pool.Spec.FlintlockHosts
	pending := map[string]bool{}
	for _, h := range hosts {
		pending[h] = true
	}
	lastErr := map[string]error{}
	var anyErr error

	wctx, cancel := context.WithTimeout(ctx, r.v.cfg.Timeout)
	defer cancel()
	var held []string
	lastBeat := r.v.cfg.Clock.Now()
	defer func() { r.release(ctx, held) }()

	for len(pending) > 0 && wctx.Err() == nil {
		if now := r.v.cfg.Clock.Now(); len(held) > 0 && now.Sub(lastBeat) >= p.Pool.HeartbeatInterval {
			for _, id := range held {
				_, _ = r.v.cfg.PoolManager.Heartbeat(wctx, id)
			}
			lastBeat = now
		}
		start := r.v.cfg.Clock.Now()
		// The claim runs on ctx, not wctx: a claim cut off by the timeout
		// after the Pool Manager granted it would leave a lease nobody
		// releases. The client's call deadline bounds it instead.
		claim, err := r.v.cfg.PoolManager.ClaimVM(ctx, ref)
		if err != nil {
			if !errors.Is(err, poolmgr.ErrExhausted) {
				anyErr = fmt.Errorf("ClaimVM: %w", err)
			}
			sleep(wctx, r.v.cfg.PollInterval)
			continue
		}
		held = append(held, claim.LeaseID)
		host := r.placement(wctx, claim, hosts)
		if host == "" || !pending[host] {
			// Held, so the next claim is placed elsewhere.
			continue
		}
		readyIn, err := r.runTrivial(wctx, p, host, claim, start)
		if err != nil {
			lastErr[host] = err
			continue
		}
		delete(pending, host)
		if hv := r.byHost[host]; hv != nil {
			if !hv.Exercised || readyIn < hv.ClaimToReady {
				hv.ClaimToReady = readyIn
			}
			hv.Exercised = true
		}
		fmt.Fprintf(r.v.cfg.Out, "ok   %s %s: pool %s, claim to ready %s\n", host, StepExercise, ref, readyIn.Round(time.Millisecond))
	}

	for _, h := range hosts {
		if !pending[h] {
			continue
		}
		cause := lastErr[h]
		if cause == nil {
			cause = anyErr
		}
		msg := fmt.Sprintf("no MicroVM of pool %s could be exercised on this Host within %s", ref, r.v.cfg.Timeout)
		if cause != nil {
			r.fail(h, StepExercise, fmt.Errorf("%s: %w", msg, cause))
		} else {
			r.fail(h, StepExercise, errors.New(msg))
		}
	}
}

// placement is the Host a claimed MicroVM runs on: the claim's host field,
// or, from a Pool Manager that does not send it, the Host of the Pool's host
// list that has the MicroVM (the Scheduler's SC-031 fan-out).
func (r *run) placement(ctx context.Context, claim *poolmgr.Claim, hosts []string) string {
	if claim.Host.Name != "" {
		return claim.Host.Name
	}
	for _, h := range hosts {
		c := r.clients[h]
		if c == nil {
			continue
		}
		if _, err := c.GetMicroVM(ctx, claim.VMUID); err == nil {
			return h
		}
	}
	return ""
}

// runTrivial waits for the guest to be ready through the Guest Transport and
// runs `true` in it with the Profile's shell. It returns the time from the
// claim to readiness.
func (r *run) runTrivial(ctx context.Context, p *config.Profile, host string, claim *poolmgr.Claim, start time.Time) (time.Duration, error) {
	client := r.clients[host]
	if client == nil {
		return 0, errors.New("the Host could not be dialled")
	}
	target := transport.Target{
		Kind:     transport.Kind(p.Transport.Kind),
		Host:     client,
		VMUID:    claim.VMUID,
		Deadline: r.v.cfg.TransportDeadline,
	}
	if p.Transport.Kind == config.TransportSSH {
		key, err := os.ReadFile(p.Transport.SSH.PrivateKeyFile)
		if err != nil {
			return 0, fmt.Errorf("reading the ssh private key: %w", err)
		}
		target.SSH = transport.SSHOptions{User: p.Transport.SSH.User, PrivateKey: key, KnownHostKey: p.Transport.SSH.KnownHostKey}
	}
	tr, err := r.v.cfg.Transports.New(ctx, target)
	if err != nil {
		return 0, fmt.Errorf("guest transport: %w", err)
	}
	defer func() { _ = tr.Close() }()

	rctx, cancel := context.WithTimeout(ctx, p.ReadyTimeout)
	defer cancel()
	for {
		err = tr.Ready(rctx)
		if err == nil {
			break
		}
		if !sleep(rctx, r.v.cfg.PollInterval) {
			return 0, fmt.Errorf("guest not ready within %s: %w", p.ReadyTimeout, err)
		}
	}
	readyIn := r.v.cfg.Clock.Now().Sub(start)

	var stderr strings.Builder
	status, err := tr.Run(ctx, transport.Command{
		Path:   p.Shell,
		Args:   []string{"-c", "true"},
		User:   p.User,
		Stderr: &stderr,
	})
	if err != nil {
		return 0, fmt.Errorf("running a trivial command: %w", err)
	}
	if status != 0 {
		return 0, fmt.Errorf("a trivial command exited %d: %s", status, strings.TrimSpace(stderr.String()))
	}
	return readyIn, nil
}

// release releases every held lease. It runs on a context of its own so
// that a verification cut short by its timeout still returns its MicroVMs.
func (r *run) release(ctx context.Context, leases []string) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, id := range leases {
		if err := r.v.cfg.PoolManager.ReleaseVM(rctx, id); err != nil && !errors.Is(err, poolmgr.ErrNotFound) {
			fmt.Fprintf(r.v.cfg.Out, "warning: releasing lease %s: %v\n", id, err)
		}
	}
}

//= docs/requirements/06-fleet.md#verification
//# If verification fails on any Host or service, then the Fleet
//# Controller SHALL exit with a non-zero status naming each failure and the
//# step that failed.

// Summary is nil for a report without failures and otherwise an error whose
// message names every failure with its Host and the step that failed, one
// per line, sorted so the output is stable. `fleet verify` exits non-zero
// with it.
func Summary(report *fleet.VerifyReport) error {
	if report == nil || len(report.Failures) == 0 {
		return nil
	}
	lines := make([]string, 0, len(report.Failures))
	for _, f := range report.Failures {
		lines = append(lines, fmt.Sprintf("  %s: step %s: %v", f.Host, f.Step, f.Err))
	}
	sort.Strings(lines)
	return fmt.Errorf("verification failed with %d failure(s):\n%s", len(lines), strings.Join(lines, "\n"))
}

func refOf(p *config.Profile) poolmgr.PoolRef {
	return poolmgr.PoolRef{Name: p.Pool.Name, Namespace: p.Pool.Namespace}
}

func namespaces(profiles []config.Profile) []string {
	var out []string
	for i := range profiles {
		if ns := profiles[i].Pool.Namespace; !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

// sleep waits d or until ctx ends, reporting whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
