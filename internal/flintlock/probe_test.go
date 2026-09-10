package flintlock_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
)

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# When starting, the Runner SHALL call `ServerInfo` on every Host
//# and SHALL log the Host's flintlock version and whether the exec and SSH
//# proxy services are enabled.

// TestProbeAllCallsServerInfoOnEveryHost starts three Hosts that differ in
// version and in which guest-agent services they serve, probes the whole
// Registry once and reads back what was logged. Every Host has to be asked
// and every answer has to appear in the log with the version and both
// service flags, because that log line is all an operator has to tell a
// fleet of mismatched Hosts apart.
func TestProbeAllCallsServerInfoOnEveryHost(t *testing.T) {
	t.Parallel()
	hosts := []flintlock.FakeHostConfig{
		{Name: "h1", Version: "v0.7.0", ExecEnabled: true, SSHProxyEnabled: true},
		{Name: "h2", Version: "v0.6.0", ExecEnabled: true},
		{Name: "h3", Version: "v0.5.0", ExecEnabled: true, SSHProxyEnabled: true},
	}
	var endpoints []flintlock.Endpoint
	for _, cfg := range hosts {
		host := startHost(t, cfg)
		endpoints = append(endpoints, flintlock.Endpoint{
			Name:    cfg.Name,
			Address: host.Addr(),
			TLS:     flintlock.TLSOptions{Insecure: true},
		})
	}

	reg := newRegistry(t, endpoints)
	rec := &logRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	infos, err := flintlock.ProbeAll(ctx, reg, testNamespace, rec.logger())
	if err != nil {
		t.Fatalf("ProbeAll: %v", err)
	}
	if len(infos) != len(hosts) {
		t.Fatalf("ProbeAll returned %d hosts, want %d", len(infos), len(hosts))
	}
	for _, cfg := range hosts {
		info, ok := infos[cfg.Name]
		if !ok {
			t.Fatalf("ProbeAll did not probe host %s", cfg.Name)
		}
		if !info.VersionKnown || info.Version != cfg.Version {
			t.Errorf("host %s reported %+v, want version %s", cfg.Name, info, cfg.Version)
		}
		if info.Exec.Enabled != cfg.ExecEnabled || info.SSHProxy.Enabled != cfg.SSHProxyEnabled {
			t.Errorf("host %s reported exec=%v ssh=%v, want exec=%v ssh=%v",
				cfg.Name, info.Exec.Enabled, info.SSHProxy.Enabled, cfg.ExecEnabled, cfg.SSHProxyEnabled)
		}
		rec := rec.find(t, "INFO", cfg.Name, "flintlock host")
		if rec == nil {
			t.Fatalf("nothing was logged for host %s", cfg.Name)
		}
		if rec["version"] != cfg.Version {
			t.Errorf("host %s was logged with version %v, want %s", cfg.Name, rec["version"], cfg.Version)
		}
		if rec["exec_enabled"] != cfg.ExecEnabled {
			t.Errorf("host %s was logged with exec_enabled %v, want %v", cfg.Name, rec["exec_enabled"], cfg.ExecEnabled)
		}
		if rec["ssh_proxy_enabled"] != cfg.SSHProxyEnabled {
			t.Errorf("host %s was logged with ssh_proxy_enabled %v, want %v",
				cfg.Name, rec["ssh_proxy_enabled"], cfg.SSHProxyEnabled)
		}
	}
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# If a Host reports that the exec service is disabled, then the
//# Runner SHALL log a warning naming the Host, because Jobs the Pool Manager
//# places there cannot be run over the `exec` Guest Transport.

// TestProbeWarnsAboutAHostWithoutTheExecService probes a Host that serves no
// exec service and one that does. The first has to produce a warning naming
// it; the second must not, so that the warning stays worth reading.
func TestProbeWarnsAboutAHostWithoutTheExecService(t *testing.T) {
	t.Parallel()
	disabled := startHost(t, flintlock.FakeHostConfig{Name: "no-exec", Version: "v0.7.0"})
	enabled := startHost(t, flintlock.FakeHostConfig{Name: "with-exec", Version: "v0.7.0", ExecEnabled: true})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	rec := &logRecorder{}
	if _, err := flintlock.Probe(ctx, dial(t, disabled), testNamespace, rec.logger()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	warning := rec.find(t, "WARN", "no-exec", "exec service disabled")
	if warning == nil {
		t.Fatalf("no warning names the host with the exec service disabled; log was %v", rec.records(t))
	}

	quiet := &logRecorder{}
	if _, err := flintlock.Probe(ctx, dial(t, enabled), testNamespace, quiet.logger()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if w := quiet.find(t, "WARN", "with-exec", ""); w != nil {
		t.Errorf("a host serving exec produced the warning %v", w)
	}
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# If `ServerInfo` is not implemented by a Host, then the Runner
//# SHALL treat the Host's version as unknown and continue.

// TestProbeContinuesWhenServerInfoIsUnimplemented probes a Registry holding
// a Host that answers UNIMPLEMENTED next to one that answers properly. The
// old Host is reported with an unknown version rather than as a failure,
// and the Host after it is still probed, which is what continuing means.
func TestProbeContinuesWhenServerInfoIsUnimplemented(t *testing.T) {
	t.Parallel()
	old := startHost(t, flintlock.FakeHostConfig{
		Name:                    "old",
		ServerInfoUnimplemented: true,
		ExecEnabled:             true,
	})
	current := startHost(t, flintlock.FakeHostConfig{Name: "current", Version: "v0.7.0", ExecEnabled: true})

	reg := newRegistry(t, []flintlock.Endpoint{
		{Name: "old", Address: old.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
		{Name: "current", Address: current.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	rec := &logRecorder{}
	infos, err := flintlock.ProbeAll(ctx, reg, testNamespace, rec.logger())
	if err != nil {
		t.Fatalf("ProbeAll: %v", err)
	}
	info, ok := infos["old"]
	if !ok {
		t.Fatal("the host without ServerInfo was dropped instead of being reported with an unknown version")
	}
	if info.VersionKnown || info.Version != "" {
		t.Errorf("the host without ServerInfo reported %+v, want an unknown version", info)
	}
	if _, ok := infos["current"]; !ok {
		t.Error("probing stopped at the host without ServerInfo instead of continuing")
	}
	if rec.find(t, "INFO", "old", "flintlock host") == nil {
		t.Errorf("nothing was logged for the host without ServerInfo; log was %v", rec.records(t))
	}
}

//= docs/requirements/05-hosts.md#flintlock-client
//= type=test
//# The Runner SHALL only call `ListMicroVMs` with its own namespace
//# and only as a health probe fallback.

// TestListMicroVMsIsOnlyTheProbeFallback records every call the Runner makes
// to a Host. A Host that answers ServerInfo is never asked to list, and the
// one that does not is asked exactly once, with the Runner's own namespace
// and nothing else, because that call is a liveness probe and not a way to
// look at MicroVMs.
func TestListMicroVMsIsOnlyTheProbeFallback(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Run("a host with ServerInfo is never asked to list", func(t *testing.T) {
		client := &recordingHost{name: "h1", info: &flintlock.HostInfo{Name: "h1", VersionKnown: true, Version: "v1", Exec: flintlock.GuestService{Enabled: true}}}
		if _, err := flintlock.Probe(ctx, client, testNamespace, nil); err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if got := client.namespaces(); len(got) != 0 {
			t.Errorf("ListMicroVMs was called with %v; a host that answers ServerInfo is not listed", got)
		}
	})

	t.Run("a host without it is listed once, in the runner's namespace", func(t *testing.T) {
		client := &recordingHost{name: "h1", infoErr: flintlock.ErrUnimplemented}
		info, err := flintlock.Probe(ctx, client, testNamespace, nil)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if info.VersionKnown {
			t.Error("the fallback reported a known version")
		}
		if got := client.namespaces(); !reflect.DeepEqual(got, []string{testNamespace}) {
			t.Errorf("ListMicroVMs was called with %v, want exactly [%s]", got, testNamespace)
		}
	})

	t.Run("a host that cannot even list is a failure", func(t *testing.T) {
		client := &recordingHost{name: "h1", infoErr: flintlock.ErrUnimplemented, listErr: flintlock.ErrUnavailable}
		if _, err := flintlock.Probe(ctx, client, testNamespace, nil); !errors.Is(err, flintlock.ErrUnavailable) {
			t.Errorf("Probe returned %v, want the host's failure", err)
		}
	})
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# When starting, the Runner SHALL compare the Inventory with the
//# Hosts the Pool Manager reports for each Pool and SHALL log a warning for
//# any Pool Host missing from the Inventory, because a MicroVM placed there
//# could not be reached.

// TestCheckPoolHostsWarnsAboutHostsMissingFromTheInventory gives the check a
// Pool whose Hosts are all in the Inventory and one whose Hosts are not.
// Only the missing ones are reported, and each is named together with its
// Pool, because that is the pair an operator has to fix.
func TestCheckPoolHostsWarnsAboutHostsMissingFromTheInventory(t *testing.T) {
	t.Parallel()
	h1 := startHost(t, flintlock.FakeHostConfig{Name: "h1", ExecEnabled: true})
	h2 := startHost(t, flintlock.FakeHostConfig{Name: "h2", ExecEnabled: true})
	reg := newRegistry(t, []flintlock.Endpoint{
		{Name: "h1", Address: h1.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
		{Name: "h2", Address: h2.Addr(), TLS: flintlock.TLSOptions{Insecure: true}},
	})

	rec := &logRecorder{}
	missing := flintlock.CheckPoolHosts(reg, map[string][]string{
		"known":   {"h1", "h2"},
		"partial": {"h1", "h3"},
		"absent":  {"h4", "h3"},
	}, rec.logger())

	want := map[string][]string{
		"partial": {"h3"},
		"absent":  {"h3", "h4"},
	}
	if !reflect.DeepEqual(missing, want) {
		t.Fatalf("CheckPoolHosts reported %v, want %v", missing, want)
	}
	for pool, hosts := range want {
		for _, host := range hosts {
			// h3 is missing from two Pools, so the pair is what has to be
			// matched: a warning naming the Host alone would not tell an
			// operator which Pool to fix.
			if !warnsAbout(t, rec, pool, host) {
				t.Fatalf("no warning names pool host %s together with its pool %s; log was %v",
					host, pool, rec.records(t))
			}
		}
	}
	for _, r := range rec.records(t) {
		if r["pool"] == "known" {
			t.Errorf("a pool whose hosts are all in the inventory produced %v", r)
		}
	}
}

// recordingHost is a HostClient that answers from its fields and records the
// namespaces it was asked to list, so that a test can see which calls the
// Runner made rather than which answers it got.
type recordingHost struct {
	name    string
	info    *flintlock.HostInfo
	infoErr error
	listErr error

	mu   sync.Mutex
	list []string
}

// Name implements flintlock.HostClient.
func (h *recordingHost) Name() string { return h.name }

// ServerInfo implements flintlock.HostClient.
func (h *recordingHost) ServerInfo(context.Context) (*flintlock.HostInfo, error) {
	if h.infoErr != nil {
		return nil, h.infoErr
	}
	return h.info, nil
}

// GetMicroVM implements flintlock.HostClient.
func (h *recordingHost) GetMicroVM(context.Context, string) (*types.MicroVM, error) {
	return &types.MicroVM{}, nil
}

// ListMicroVMs implements flintlock.HostClient.
func (h *recordingHost) ListMicroVMs(_ context.Context, namespace string) ([]*types.MicroVM, error) {
	h.mu.Lock()
	h.list = append(h.list, namespace)
	h.mu.Unlock()
	if h.listErr != nil {
		return nil, h.listErr
	}
	return nil, nil
}

// Exec implements flintlock.HostClient.
func (h *recordingHost) Exec(context.Context) (flintlock.ExecStream, error) {
	return nil, errors.New("not used")
}

// SSHProxy implements flintlock.HostClient.
func (h *recordingHost) SSHProxy(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, errors.New("not used")
}

// Close implements flintlock.HostClient.
func (h *recordingHost) Close() error { return nil }

// namespaces returns the namespaces ListMicroVMs was called with.
func (h *recordingHost) namespaces() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.list...)
}
