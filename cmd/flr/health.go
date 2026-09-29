package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

// healthServer serves the Runner's observability listen address. It answers
// /healthz and /readyz itself (OB-030 to OB-032) and passes every other path,
// /metrics among them, to gitlab-runner's metrics server, which listens on a
// loopback address that only this process knows.
//
// gitlab-runner's run loop builds its metrics server with a private mux and a
// private Prometheus registry, so nothing can be added to it and its
// registry cannot be served from elsewhere. The Runner therefore owns the
// configured address, and the run loop gets the loopback one (runRunner).
// Scrapers and probes see one port, as deploy/runner expects.
type healthServer struct {
	log *slog.Logger
	srv *http.Server
	lis net.Listener

	// verified is set once GitLab has accepted the runner token (GL-015).
	verified atomic.Bool
	// status is the Scheduler, once it is built. It is read on every probe
	// and holds the state its own loops keep, so a probe makes no call to
	// the Pool Manager, to a Host or to the API server.
	status atomic.Pointer[scheduler.Status]
	// pools is the claim backend's view of the Runner's Pools, nil under
	// any other backend. Where it is set, a Ready Pool takes the place of
	// a healthy Host (OB-031).
	pools atomic.Pointer[poolReadiness]
}

// poolReadiness is what the claim backend says about the readiness of the
// Runner's Pools, from its watch (claim.Backend).
type poolReadiness interface {
	PoolsReady() []claim.PoolReadiness
}

// readinessReport is the body of /readyz (OB-032).
type readinessReport struct {
	Ready bool `json:"ready"`
	// TokenVerified is true once GitLab has accepted the runner token.
	TokenVerified bool `json:"token_verified"`
	// PoolManagerContacted is true once the Pool Manager has answered.
	PoolManagerContacted bool `json:"pool_manager_contacted"`
	// PoolManagerReachable is false before the first answer, during the
	// backoff of a failed claim and after consecutive failed probes.
	PoolManagerReachable bool         `json:"pool_manager_reachable"`
	HealthyHosts         int          `json:"healthy_hosts"`
	Pools                []poolReport `json:"pools"`
}

// poolReport is one Pool in the body of /readyz.
type poolReport struct {
	Pool      string `json:"pool"`
	Available int32  `json:"available"`
	// Ready is whether the Pool's status reports Ready. Only the claim
	// backend reports it.
	Ready *bool `json:"ready,omitempty"`
}

// newHealthServer listens on addr and proxies every path but its own to
// upstream, gitlab-runner's metrics server. The listener is bound here, so
// that a busy address fails the start of `run` at once.
func newHealthServer(addr, upstream string, log *slog.Logger) (*healthServer, error) {
	target, err := url.Parse("http://" + upstream)
	if err != nil {
		return nil, fmt.Errorf("metrics upstream %q: %w", upstream, err)
	}
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("observability listen address: %w", err)
	}
	h := &healthServer{log: log, lis: lis}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// Until the run loop starts its server, and after it stops, /metrics
	// fails with a 502; that is not worth a log line at every scrape.
	proxy.ErrorLog = slog.NewLogLogger(log.Handler(), slog.LevelDebug)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.serveHealthz)
	mux.HandleFunc("/readyz", h.serveReadyz)
	mux.Handle("/", proxy)
	h.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return h, nil
}

// Addr is the address the server listens on.
func (h *healthServer) Addr() string { return h.lis.Addr().String() }

// serve serves until close. It is started before the Runner verifies its
// token or reaches the Pool Manager, so that liveness answers from the start
// of the process.
func (h *healthServer) serve() {
	go func() {
		if err := h.srv.Serve(h.lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.log.Error("health server stopped", "error", err)
		}
	}()
}

// close stops the server.
func (h *healthServer) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = h.srv.Shutdown(ctx)
}

// tokenVerified records that GitLab accepted the runner token.
func (h *healthServer) tokenVerified() { h.verified.Store(true) }

// setStatus gives the server the Scheduler to read readiness from.
func (h *healthServer) setStatus(s scheduler.Status) { h.status.Store(&s) }

// setPools gives the server the claim backend's Pool readiness. It is
// called only where the claim backend is configured.
func (h *healthServer) setPools(p poolReadiness) { h.pools.Store(&p) }

//= docs/requirements/08-observability.md#health
//# The Runner SHALL serve a liveness endpoint at `/healthz` that
//# returns success while the process is running.

// serveHealthz answers 200 whenever the process can answer at all. It checks
// nothing else: a Runner that cannot reach its Pool Manager or GitLab is not
// ready, but restarting it would not help.
func (h *healthServer) serveHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, "ok")
}

//= docs/requirements/08-observability.md#health
//# The Runner SHALL serve a readiness endpoint at `/readyz` that
//# returns success only after the runner token has been verified, the Pool
//# Manager has answered at least once and at least one Host is healthy,
//# except that where the claim backend is configured, at least one of the
//# Runner's Pools reporting Ready in its status takes the place of the
//# healthy Host.

//= docs/requirements/04-pool-manager.md#client
//# If the Pool Manager is unreachable at startup, then the Scheduler SHALL
//# keep retrying the connection with exponential backoff and the Runner
//# SHALL NOT report itself ready until it succeeds.

// serveReadyz answers 200 when the Runner is ready and 503 when it is not,
// with the report as JSON either way.
func (h *healthServer) serveReadyz(w http.ResponseWriter, _ *http.Request) {
	report := h.readiness()
	code := http.StatusServiceUnavailable
	if report.Ready {
		code = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(report)
}

//= docs/requirements/08-observability.md#health
//# The readiness endpoint SHALL report, in its response body, the
//# number of healthy Hosts, whether the Pool Manager is reachable and the
//# available count of each Pool.

// readiness builds the report from the token state, the Scheduler's
// Snapshot and, under the claim backend, the Pools' readiness. Before the
// Scheduler exists the Runner is not ready.
func (h *healthServer) readiness() readinessReport {
	report := readinessReport{TokenVerified: h.verified.Load(), Pools: []poolReport{}}
	sp := h.status.Load()
	if sp == nil {
		return report
	}
	snap := (*sp).Snapshot()
	report.PoolManagerContacted = snap.PoolManagerContacted
	report.PoolManagerReachable = snap.PoolManagerHealthy
	for _, host := range snap.Hosts {
		if host.Healthy {
			report.HealthyHosts++
		}
	}
	for _, p := range snap.Pools {
		report.Pools = append(report.Pools, poolReport{Pool: p.Pool.String(), Available: p.Status.Available})
	}

	// Under the claim backend the Runner probes no Host, so a Pool that
	// battery-operator marks Ready stands in for a healthy Host.
	capacity := report.HealthyHosts > 0
	if pp := h.pools.Load(); pp != nil {
		capacity = false
		for _, p := range (*pp).PoolsReady() {
			capacity = capacity || p.Ready
			report.setPoolReady(p.Pool.String(), p.Ready)
		}
	}
	report.Ready = report.TokenVerified && report.PoolManagerContacted && capacity
	return report
}

// setPoolReady records a Pool's readiness in the report, adding the Pool if
// the Scheduler does not track it.
func (r *readinessReport) setPoolReady(pool string, ready bool) {
	for i := range r.Pools {
		if r.Pools[i].Pool == pool {
			r.Pools[i].Ready = &ready
			return
		}
	}
	r.Pools = append(r.Pools, poolReport{Pool: pool, Ready: &ready})
}

// freeLoopbackAddr returns a loopback address whose port was free a moment
// ago, for gitlab-runner's metrics server behind the health server. The port
// is released before the run loop binds it; if another process takes it in
// between, the run loop fails to start and says so.
func freeLoopbackAddr() (string, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("finding a port for the metrics server: %w", err)
	}
	addr := lis.Addr().String()
	if err := lis.Close(); err != nil {
		return "", fmt.Errorf("finding a port for the metrics server: %w", err)
	}
	return addr, nil
}
