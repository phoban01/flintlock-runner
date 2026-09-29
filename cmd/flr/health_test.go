package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim"
	"github.com/phoban01/flintlock-runner/internal/scheduler"
)

// fakeStatus is a Scheduler Status whose Snapshot a test sets.
type fakeStatus struct {
	mu   sync.Mutex
	snap scheduler.Snapshot
}

func (f *fakeStatus) Snapshot() scheduler.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap
}

func (f *fakeStatus) set(snap scheduler.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = snap
}

// startHealth starts a health server on a loopback port, with no run loop
// behind it yet, and returns its base URL.
func startHealth(t *testing.T) (*healthServer, string) {
	t.Helper()
	h, err := newHealthServer("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("newHealthServer: %v", err)
	}
	h.serve()
	t.Cleanup(h.close)
	return h, "http://" + h.Addr()
}

// get fetches url and returns the status code and body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // a loopback test server
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading %s: %v", url, err)
	}
	return resp.StatusCode, string(body)
}

// readyz fetches /readyz and decodes its report.
func readyz(t *testing.T, base string) (int, readinessReport) {
	t.Helper()
	code, body := get(t, base+"/readyz")
	var report readinessReport
	if err := json.Unmarshal([]byte(body), &report); err != nil {
		t.Fatalf("/readyz body %q is not the JSON report: %v", body, err)
	}
	return code, report
}

//= docs/requirements/08-observability.md#health
//= type=test
//# The Runner SHALL serve a liveness endpoint at `/healthz` that
//# returns success while the process is running.

// TestHealthzAnswersBeforeTheRunnerIsReady checks that liveness does not wait
// for readiness: a Runner that has not verified its token or reached the
// Pool Manager is still alive, and the kubelet must not restart it.
func TestHealthzAnswersBeforeTheRunnerIsReady(t *testing.T) {
	t.Parallel()
	_, base := startHealth(t)

	if code, _ := get(t, base+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz = %d before anything is ready, want 503", code)
	}
	code, body := get(t, base+"/healthz")
	if code != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200", code)
	}
	if strings.TrimSpace(body) != "ok" {
		t.Fatalf("/healthz body = %q, want ok", body)
	}
}

//= docs/requirements/08-observability.md#health
//= type=test
//# The Runner SHALL serve a readiness endpoint at `/readyz` that
//# returns success only after the runner token has been verified, the Pool
//# Manager has answered at least once and at least one Host is healthy,

//= docs/requirements/04-pool-manager.md#client
//= type=test
//# If the Pool Manager is unreachable at startup, then the Scheduler SHALL
//# keep retrying the connection with exponential backoff and the Runner
//# SHALL NOT report itself ready until it succeeds.

// TestReadyzFailsUntilEveryConditionHolds walks a Runner through its start:
// /readyz fails with no Scheduler, with the token not verified, with the
// Pool Manager never contacted and with no healthy Host, and passes once all
// three conditions hold. It fails again when the last Host goes unhealthy.
func TestReadyzFailsUntilEveryConditionHolds(t *testing.T) {
	t.Parallel()
	h, base := startHealth(t)
	status := &fakeStatus{}

	expect := func(step string, want int) {
		t.Helper()
		if code, report := readyz(t, base); code != want || report.Ready != (want == http.StatusOK) {
			t.Fatalf("%s: /readyz = %d, ready %t; want %d", step, code, report.Ready, want)
		}
	}
	hostUp := []scheduler.HostHealth{{Name: "host-1", Healthy: true}}
	hostDown := []scheduler.HostHealth{{Name: "host-1", Healthy: false}}

	expect("no scheduler yet", http.StatusServiceUnavailable)

	// Everything but the token.
	status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: true, Hosts: hostUp})
	h.setStatus(status)
	expect("token not verified", http.StatusServiceUnavailable)

	h.tokenVerified()
	status.set(scheduler.Snapshot{Hosts: hostUp})
	expect("pool manager never answered", http.StatusServiceUnavailable)

	status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: true, Hosts: hostDown})
	expect("no healthy host", http.StatusServiceUnavailable)

	status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: true})
	expect("no host at all", http.StatusServiceUnavailable)

	status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: true, Hosts: hostUp})
	expect("ready", http.StatusOK)

	status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: true, Hosts: hostDown})
	expect("last host went unhealthy", http.StatusServiceUnavailable)
}

// fakePools is the claim backend's Pool readiness, as a test sets it.
type fakePools struct {
	mu    sync.Mutex
	pools []claim.PoolReadiness
}

func (f *fakePools) PoolsReady() []claim.PoolReadiness {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]claim.PoolReadiness(nil), f.pools...)
}

func (f *fakePools) set(pools ...claim.PoolReadiness) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pools = pools
}

//= docs/requirements/08-observability.md#health
//= type=test
//# except that where the claim backend is configured, at least one of the
//# Runner's Pools reporting Ready in its status takes the place of the
//# healthy Host.

// TestReadyzUnderTheClaimBackendWaitsForAReadyPool covers the claim backend,
// where the Runner probes no Host: with the token verified and the backend
// answering, /readyz fails while no Pool is Ready and passes once one is. A
// healthy Host does not count there, and the token and the backend's answer
// are still needed. The body carries each Pool's readiness (OB-032).
func TestReadyzUnderTheClaimBackendWaitsForAReadyPool(t *testing.T) {
	t.Parallel()
	h, base := startHealth(t)
	status := &fakeStatus{}
	pools := &fakePools{}
	h.setStatus(status)
	h.setPools(pools)

	small := poolmgr.PoolRef{Namespace: "ci", Name: "small"}
	large := poolmgr.PoolRef{Namespace: "ci", Name: "large"}
	status.set(scheduler.Snapshot{
		PoolManagerContacted: true,
		PoolManagerHealthy:   true,
		Pools:                []poolmgr.PoolAvailability{{Pool: small, Status: poolmgr.PoolStatus{Available: 1}}},
		// A healthy Host does not stand in for a Ready Pool.
		Hosts: []scheduler.HostHealth{{Name: "host-1", Healthy: true}},
	})
	expect := func(step string, want int) readinessReport {
		t.Helper()
		code, report := readyz(t, base)
		if code != want || report.Ready != (want == http.StatusOK) {
			t.Fatalf("%s: /readyz = %d, ready %t; want %d", step, code, report.Ready, want)
		}
		return report
	}

	h.tokenVerified()
	expect("no pool seen yet", http.StatusServiceUnavailable)

	pools.set(claim.PoolReadiness{Pool: large}, claim.PoolReadiness{Pool: small})
	report := expect("no pool ready", http.StatusServiceUnavailable)
	for _, p := range report.Pools {
		if p.Ready == nil || *p.Ready {
			t.Errorf("pool %s: ready = %v, want false", p.Pool, p.Ready)
		}
	}

	pools.set(claim.PoolReadiness{Pool: large}, claim.PoolReadiness{Pool: small, Ready: true})
	report = expect("one pool ready", http.StatusOK)
	want := map[string]bool{"ci/small": true, "ci/large": false}
	if len(report.Pools) != len(want) {
		t.Fatalf("pools = %+v, want %v", report.Pools, want)
	}
	for _, p := range report.Pools {
		if p.Ready == nil || *p.Ready != want[p.Pool] {
			t.Errorf("pool %s: ready = %v, want %t", p.Pool, p.Ready, want[p.Pool])
		}
		if p.Pool == "ci/small" && p.Available != 1 {
			t.Errorf("pool ci/small: available = %d, want 1", p.Available)
		}
	}

	status.set(scheduler.Snapshot{PoolManagerHealthy: false})
	expect("backend never answered", http.StatusServiceUnavailable)
}

//= docs/requirements/08-observability.md#health
//= type=test
//# The readiness endpoint SHALL report, in its response body, the
//# number of healthy Hosts, whether the Pool Manager is reachable and the
//# available count of each Pool.

// TestReadyzReportsHostsPoolManagerAndPools checks the body of /readyz, both
// when the Runner is ready and when a failed claim has made the Pool Manager
// unreachable, which does not by itself take readiness away (OB-031 asks
// only that it has answered once).
func TestReadyzReportsHostsPoolManagerAndPools(t *testing.T) {
	t.Parallel()
	h, base := startHealth(t)
	h.tokenVerified()
	status := &fakeStatus{}
	h.setStatus(status)

	pools := []poolmgr.PoolAvailability{
		{Pool: poolmgr.PoolRef{Namespace: "ci", Name: "small"}, Status: poolmgr.PoolStatus{Available: 3, Leased: 1}},
		{Pool: poolmgr.PoolRef{Namespace: "ci", Name: "large"}, Status: poolmgr.PoolStatus{Available: 0}},
	}
	hosts := []scheduler.HostHealth{
		{Name: "host-1", Healthy: true},
		{Name: "host-2", Healthy: false},
		{Name: "host-3", Healthy: true},
	}
	wantPools := []poolReport{{Pool: "ci/small", Available: 3}, {Pool: "ci/large", Available: 0}}

	for _, reachable := range []bool{true, false} {
		status.set(scheduler.Snapshot{PoolManagerContacted: true, PoolManagerHealthy: reachable, Pools: pools, Hosts: hosts})
		code, report := readyz(t, base)
		if code != http.StatusOK {
			t.Fatalf("reachable %t: /readyz = %d, want 200", reachable, code)
		}
		if report.HealthyHosts != 2 {
			t.Errorf("reachable %t: healthy_hosts = %d, want 2", reachable, report.HealthyHosts)
		}
		if report.PoolManagerReachable != reachable {
			t.Errorf("pool_manager_reachable = %t, want %t", report.PoolManagerReachable, reachable)
		}
		if len(report.Pools) != len(wantPools) {
			t.Fatalf("pools = %+v, want %+v", report.Pools, wantPools)
		}
		for i, p := range wantPools {
			if report.Pools[i] != p {
				t.Errorf("pools[%d] = %+v, want %+v", i, report.Pools[i], p)
			}
		}
	}
}

// TestMetricsArePassedToTheRunLoop checks that the health server hands every
// other path to gitlab-runner's metrics server, so /metrics stays on the
// configured listen address (OB-010, OB-011).
func TestMetricsArePassedToTheRunLoop(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(fakeRunLoopMetrics())
	t.Cleanup(upstream.Close)
	h, base := startHealth(t)

	if code, _ := get(t, base+"/metrics"); code != http.StatusServiceUnavailable {
		t.Fatalf("/metrics = %d before the run loop's server is known, want 503", code)
	}
	h.setRunLoop(strings.TrimPrefix(upstream.URL, "http://"))
	for path, want := range map[string]string{
		"/metrics":             "gitlab_runner_version_info",
		"/debug/jobs/list":     "jobs",
		"/debug/process/state": "running",
		"/debug/pprof/cmdline": "cmdline",
		"/healthz":             "ok",
	} {
		code, body := get(t, base+path)
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("%s = %d %q, want 200 and %q", path, code, body, want)
		}
	}
}

// fakeRunLoopMetrics stands in for gitlab-runner's metrics server: it
// answers /metrics and the /debug paths, and nothing else.
func fakeRunLoopMetrics() http.Handler {
	mux := http.NewServeMux()
	for path, body := range map[string]string{
		"/metrics":             "gitlab_runner_version_info 1\n",
		"/debug/jobs/list":     "jobs\n",
		"/debug/process/state": "running\n",
		"/debug/pprof/cmdline": "cmdline\n",
	} {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		})
	}
	return mux
}

// TestRunLoopMetricsSurviveATakenPort binds the run loop's metrics server
// as gitlab-runner does, net.Listen on runLoopMetricsAddr, after another
// listener takes the port that address names. It checks that the bind
// succeeds and that /metrics reaches the server through the health server.
// With a port chosen ahead of the bind, the other listener wins the port,
// and gitlab-runner stops the Runner (#109).
//
// The test is not parallel: followRunLoop looks at every listener of the
// process, and a parallel test's listener would look like a second
// candidate.
func TestRunLoopMetricsSurviveATakenPort(t *testing.T) {
	h, base := startHealth(t)
	before, err := loopbackListeners()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go h.followRunLoop(ctx, before, 5*time.Millisecond)

	// Another process takes the port the run loop is to bind, where the
	// address names one.
	_, port, err := net.SplitHostPort(runLoopMetricsAddr)
	if err != nil {
		t.Fatal(err)
	}
	if port != "0" {
		other, err := net.Listen("tcp", runLoopMetricsAddr)
		if err != nil {
			t.Fatalf("taking the run loop's port: %v", err)
		}
		t.Cleanup(func() { _ = other.Close() })
	}

	lis, err := net.Listen("tcp", runLoopMetricsAddr)
	if err != nil {
		t.Fatalf("the run loop cannot bind its metrics server: %v", err)
	}
	srv := &http.Server{Handler: fakeRunLoopMetrics(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := get(t, base+"/metrics")
		if code == http.StatusOK && strings.Contains(body, "gitlab_runner_version_info") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("/metrics = %d %q, want the run loop's metrics", code, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFollowRunLoopDoesNotGuess checks that followRunLoop waits, and does
// not pick one, while more than one loopback listener is new. It is not
// parallel, for the reason TestRunLoopMetricsSurviveATakenPort gives.
func TestFollowRunLoopDoesNotGuess(t *testing.T) {
	h, _ := startHealth(t)
	before, err := loopbackListeners()
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lis.Close() })
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	h.followRunLoop(ctx, before, 5*time.Millisecond)
	if h.runLoop.Load() != nil {
		t.Fatal("followRunLoop chose one of two new listeners")
	}
}

// TestLoopbackListenersSeesOnlyListeners checks that loopbackListeners
// finds a listener on the run loop's host, and leaves out a connected
// socket.
func TestLoopbackListenersSeesOnlyListeners(t *testing.T) {
	t.Parallel()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lis.Close() })
	conn, err := net.Dial("tcp", lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	got, err := loopbackListeners()
	if err != nil {
		t.Fatal(err)
	}
	if !got[netip.MustParseAddrPort(lis.Addr().String())] {
		t.Errorf("loopbackListeners() = %v, lacks the listener %s", got, lis.Addr())
	}
	if got[netip.MustParseAddrPort(conn.LocalAddr().String())] {
		t.Errorf("loopbackListeners() = %v, has the connected socket %s", got, conn.LocalAddr())
	}
}

// TestHealthServerRefusesABusyAddress checks that a listen address another
// process holds fails the start of `run` rather than a later probe.
func TestHealthServerRefusesABusyAddress(t *testing.T) {
	t.Parallel()
	h, _ := startHealth(t)
	if _, err := newHealthServer(h.Addr(), slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("a second health server bound the same address")
	}
}
