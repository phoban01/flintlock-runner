package flintlock_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/flintlock/fake"
)

// testTimeout bounds every wait in these tests; hitting it means a hang,
// not a slow machine.
const testTimeout = 30 * time.Second

// testNamespace is the Runner namespace the probes use.
const testNamespace = "flintlock-runner-test"

// startHost builds a fake Host, serves it until the test ends and returns
// it. The Host answers on a loopback port with the given configuration.
func startHost(t *testing.T, cfg flintlock.FakeHostConfig) *fake.Host {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "h1"
	}
	if cfg.SandboxRoot == "" {
		cfg.SandboxRoot = t.TempDir()
	}
	h := fake.New(cfg)
	serve(t, h)
	return h
}

// serve runs h.Serve in the background until the test ends.
func serve(t *testing.T, h *fake.Host) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- h.Serve(ctx) }()
	select {
	case <-h.Ready():
	case err := <-errc:
		cancel()
		t.Fatalf("fake host %s exited before listening: %v", h.Config().Name, err)
	case <-time.After(testTimeout):
		cancel()
		t.Fatalf("fake host %s did not start listening", h.Config().Name)
	}
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-errc:
				if err != nil {
					t.Errorf("fake host %s: Serve returned %v", h.Config().Name, err)
				}
			case <-time.After(testTimeout):
				t.Errorf("fake host %s: Serve did not return", h.Config().Name)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// dial connects to a serving fake Host with the production Dialer and
// closes the client when the test ends.
func dial(t *testing.T, h *fake.Host, opts ...flintlock.DialerOption) flintlock.HostClient {
	t.Helper()
	return dialEndpoint(t, flintlock.Endpoint{
		Name:    h.Config().Name,
		Address: h.Addr(),
		Token:   h.Config().Token,
		TLS:     flintlock.TLSOptions{Insecure: true},
	}, opts...)
}

// dialEndpoint connects to an Endpoint with the production Dialer.
func dialEndpoint(t *testing.T, ep flintlock.Endpoint, opts ...flintlock.DialerOption) flintlock.HostClient {
	t.Helper()
	client, err := flintlock.NewDialer(opts...).Dial(context.Background(), ep)
	if err != nil {
		t.Fatalf("Dial(%s): %v", ep.Name, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// createVM adds a MicroVM to a fake Host through its in-memory admin client,
// which is the Pool Manager's side of the split (HO-007), and returns its
// uid.
func createVM(t *testing.T, h *fake.Host) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	vm, err := h.Client().CreateMicroVM(ctx, &types.MicroVMSpec{
		Id:        "vm",
		Namespace: testNamespace,
	})
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	return vm.GetSpec().GetUid()
}

// logRecorder captures slog records as JSON so that a test can assert on
// what was logged (OB-001).
type logRecorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// logger returns a logger writing into the recorder at debug level.
func (r *logRecorder) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Write implements io.Writer.
func (r *logRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

// records returns the captured records, decoded.
func (r *logRecorder) records(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("decoding log record %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// warnsAbout reports whether the recorder holds a warning naming both a Pool
// and one of its Hosts as missing from the Inventory. Matching the pair is
// what matters where a Host is missing from more than one Pool.
func warnsAbout(t *testing.T, r *logRecorder, pool, host string) bool {
	t.Helper()
	for _, rec := range r.records(t) {
		msg, _ := rec["msg"].(string)
		if rec["level"] == "WARN" && rec["host"] == host && rec["pool"] == pool &&
			strings.Contains(msg, "missing from the inventory") {
			return true
		}
	}
	return false
}

// find returns the first record whose level and host match and whose
// message contains want, or nil.
func (r *logRecorder) find(t *testing.T, level, host, want string) map[string]any {
	t.Helper()
	for _, rec := range r.records(t) {
		if rec["level"] != level {
			continue
		}
		if host != "" && rec["host"] != host {
			continue
		}
		msg, _ := rec["msg"].(string)
		if want == "" || strings.Contains(msg, want) {
			return rec
		}
	}
	return nil
}
