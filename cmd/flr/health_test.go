package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

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

// startHealth starts a health server on a loopback port in front of
// upstream and returns its base URL.
func startHealth(t *testing.T, upstream string) (*healthServer, string) {
	t.Helper()
	h, err := newHealthServer("127.0.0.1:0", upstream, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	_, base := startHealth(t, "127.0.0.1:1")

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
	h, base := startHealth(t, "127.0.0.1:1")
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
	h, base := startHealth(t, "127.0.0.1:1")
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
	h, base := startHealth(t, "127.0.0.1:1")
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "gitlab_runner_version_info 1\n")
	}))
	t.Cleanup(upstream.Close)
	_, base := startHealth(t, strings.TrimPrefix(upstream.URL, "http://"))

	code, body := get(t, base+"/metrics")
	if code != http.StatusOK || !strings.Contains(body, "gitlab_runner_version_info") {
		t.Fatalf("/metrics = %d %q, want the run loop's metrics", code, body)
	}
}

// TestHealthServerRefusesABusyAddress checks that a listen address another
// process holds fails the start of `run` rather than a later probe.
func TestHealthServerRefusesABusyAddress(t *testing.T) {
	t.Parallel()
	h, _ := startHealth(t, "127.0.0.1:1")
	if _, err := newHealthServer(h.Addr(), "127.0.0.1:1", slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Fatal("a second health server bound the same address")
	}
}
