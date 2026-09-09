package main

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// testTimeout bounds every test that starts the binary.
const testTimeout = 30 * time.Second

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL be runnable as a standalone binary as well as
//# in process, so that a fleet without battery can run on it.

// TestRunServesUntilTheContextEnds starts the binary the way main does, with
// an inventory of flintlockd endpoints and a listen address, and drives the
// three services over a real gRPC connection to the address it reports. It
// then ends the context, as a signal does, and checks that run returns
// cleanly. Nothing outside the process is needed: the Hosts are dialled
// lazily, so the inventory may name endpoints nothing is listening on.
func TestRunServesUntilTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	writeFile(t, tokenFile, "runner:hunter2\n")
	inventory := filepath.Join(dir, "inventory.yaml")
	writeFile(t, inventory, `
hosts:
  - name: host-a
    endpoint: 127.0.0.1:19090
    token_file: `+tokenFile+`
    tls:
      insecure: true
  - name: host-b
    endpoint: 127.0.0.1:19091
    arch: arm64
    vcpu: 8
    tls:
      insecure: true
`)

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	addr, done := start(t, ctx, []string{
		"-listen", "127.0.0.1:0",
		"-inventory", inventory,
		// A long interval keeps the control loop out of the way; the
		// binary runs on the real clock.
		"-reconcile-interval", "1h",
		"-log-level", "error",
	})

	client, err := fake.Dial(addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = client.Close() }()

	// PoolAdmin, Lease and Events all answer.
	pools, err := client.ListPools(ctx, "")
	if err != nil {
		t.Fatalf("ListPools: %v", err)
	}
	if len(pools) != 0 {
		t.Fatalf("ListPools = %v, want none on a fresh pool manager", pools)
	}
	ref := poolmgr.PoolRef{Name: "nope", Namespace: "runner-ns"}
	if _, err := client.GetPool(ctx, ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("GetPool on an undeclared pool = %v, want ErrNotFound", err)
	}
	if _, err := client.ClaimVM(ctx, ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Fatalf("ClaimVM on an undeclared pool = %v, want ErrNotFound", err)
	}
	stream, err := client.Subscribe(ctx, poolmgr.EventFilter{})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("closing the events stream: %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run returned %v, want a clean shutdown", err)
	}
}

// start runs the binary on a goroutine and returns the address it reports
// and the channel its result arrives on. It fails the test if the binary
// stops before it is listening.
func start(t *testing.T, ctx context.Context, args []string) (string, <-chan error) {
	t.Helper()
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pr.Close() })
	done := make(chan error, 1)
	go func() {
		err := run(ctx, args, pw)
		_ = pw.Close()
		done <- err
	}()
	line := bufio.NewScanner(pr)
	if !line.Scan() {
		t.Fatalf("the binary reported no listen address; it returned %v", <-done)
	}
	addr, ok := strings.CutPrefix(line.Text(), "fake-poolmgr listening on ")
	if !ok {
		t.Fatalf("first line %q, want the listen address", line.Text())
	}
	return addr, done
}

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//= type=test
//# The fake Pool Manager SHALL be runnable as a standalone binary as well as
//# in process, so that a fleet without battery can run on it.

// TestRunReportsStartupFailures checks the two ways starting the binary can
// fail before it serves: a command line it cannot parse and an inventory it
// cannot read. Both come back as an error for main to exit on rather than a
// process that is up but useless.
func TestRunReportsStartupFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "unknown flag",
			args: []string{"-nope"},
			want: "flag provided but not defined",
		},
		{
			name: "missing inventory",
			args: []string{"-inventory", filepath.Join(t.TempDir(), "absent.yaml")},
			want: "no such file",
		},
		{
			name: "unusable listen address",
			args: []string{"-listen", "256.256.256.256:1", "-host", "host-a=127.0.0.1:9090"},
			want: "listen",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(ctx, tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("run = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}

// TestParseFlags covers the command line the binary accepts and the values
// it refuses, including the switches that reach poolmgr.FakeConfig.
func TestParseFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    poolmgr.FakeConfig
		hosts   []flintlock.Endpoint
		wantErr string
	}{
		{
			name: "defaults",
			want: poolmgr.FakeConfig{
				Listen:            defaultListen,
				Placement:         poolmgr.PlacementLeastVMs,
				ReconcileInterval: defaultReconcileInterval,
				ReadyTimeout:      defaultReadyTimeout,
				EventReplay:       defaultEventReplay,
			},
		},
		{
			name: "every switch",
			args: []string{
				"-listen", "127.0.0.1:9440",
				"-placement", "round_robin",
				"-reconcile-interval", "250ms",
				"-ready-timeout", "5s",
				"-event-replay", "7",
				"-omit-host-on-claim",
				"-namespace", "runner-ns",
			},
			want: poolmgr.FakeConfig{
				Listen:            "127.0.0.1:9440",
				Placement:         poolmgr.PlacementRoundRobin,
				ReconcileInterval: 250 * time.Millisecond,
				ReadyTimeout:      5 * time.Second,
				EventReplay:       7,
				OmitHostOnClaim:   true,
				Namespace:         "runner-ns",
			},
		},
		{
			name:  "hosts verify tls by default",
			args:  []string{"-host", "host-a=10.0.0.1:9090", "-host", "host-b=10.0.0.2:9090"},
			hosts: []flintlock.Endpoint{{Name: "host-a", Address: "10.0.0.1:9090"}, {Name: "host-b", Address: "10.0.0.2:9090"}},
		},
		{
			name: "plaintext has to be explicit",
			args: []string{"-host", "host-a=10.0.0.1:9090", "-insecure"},
			hosts: []flintlock.Endpoint{
				{Name: "host-a", Address: "10.0.0.1:9090", TLS: flintlock.TLSOptions{Insecure: true}},
			},
		},
		{name: "host without an address", args: []string{"-host", "host-a"}, wantErr: "want name=address"},
		{name: "duplicate host name", args: []string{"-host", "a=1:1", "-host", "a=2:2"}, wantErr: "declared twice"},
		{name: "duplicate address", args: []string{"-host", "a=1:1", "-host", "b=1:1"}, wantErr: "declared twice"},
		{name: "unknown placement", args: []string{"-placement", "random"}, wantErr: "-placement"},
		{name: "zero reconcile interval", args: []string{"-reconcile-interval", "0"}, wantErr: "-reconcile-interval"},
		{name: "negative ready timeout", args: []string{"-ready-timeout", "-1s"}, wantErr: "-ready-timeout"},
		{name: "zero event replay", args: []string{"-event-replay", "0"}, wantErr: "-event-replay"},
		{name: "unknown log level", args: []string{"-log-level", "chatty"}, wantErr: "-log-level"},
		{name: "unexpected argument", args: []string{"serve"}, wantErr: "unexpected argument"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseFlags(tt.args, io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseFlags = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if tt.want != (poolmgr.FakeConfig{}) && opts.cfg != tt.want {
				t.Fatalf("config = %+v, want %+v", opts.cfg, tt.want)
			}
			if len(tt.hosts) > 0 && !sameEndpoints(opts.endpoints, tt.hosts) {
				t.Fatalf("endpoints = %+v, want %+v", opts.endpoints, tt.hosts)
			}
		})
	}
}

// TestLoadInventory covers the inventory file the -inventory flag names.
func TestLoadInventory(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	writeFile(t, token, "runner:hunter2\n")
	empty := filepath.Join(dir, "empty-token")
	writeFile(t, empty, "\n")

	tests := []struct {
		name    string
		body    string
		want    []flintlock.Endpoint
		wantErr string
	}{
		{
			name: "endpoints with tls and a token file",
			body: `
hosts:
  - name: host-a
    endpoint: 10.0.0.1:9090
    token_file: ` + token + `
    tls:
      ca_file: /etc/ca.pem
      cert_file: /etc/cert.pem
      key_file: /etc/key.pem
  - name: host-b
    address: 10.0.0.2:9090
    token: inline:secret
    tls:
      insecure: true
`,
			want: []flintlock.Endpoint{
				{
					Name:    "host-a",
					Address: "10.0.0.1:9090",
					Token:   "runner:hunter2",
					TLS:     flintlock.TLSOptions{CAFile: "/etc/ca.pem", CertFile: "/etc/cert.pem", KeyFile: "/etc/key.pem"},
				},
				{
					Name:    "host-b",
					Address: "10.0.0.2:9090",
					Token:   "inline:secret",
					TLS:     flintlock.TLSOptions{Insecure: true},
				},
			},
		},
		{
			name: "a fleet inventory's extra fields are ignored",
			body: `
hosts:
  - name: host-a
    endpoint: 10.0.0.1:9090
    arch: arm64
    vcpu: 16
    memory_mb: 32768
    labels:
      pool: default
    services:
      cache: 10.0.0.1:9000
`,
			want: []flintlock.Endpoint{{Name: "host-a", Address: "10.0.0.1:9090"}},
		},
		{name: "no hosts", body: "hosts: []\n", wantErr: "no hosts"},
		{name: "not yaml", body: "\thosts:\n", wantErr: "yaml"},
		{name: "no name", body: "hosts:\n  - endpoint: 10.0.0.1:9090\n", wantErr: "name is required"},
		{name: "no endpoint", body: "hosts:\n  - name: host-a\n", wantErr: "endpoint is required"},
		{
			name:    "token and token file",
			body:    "hosts:\n  - name: host-a\n    endpoint: 10.0.0.1:9090\n    token: a\n    token_file: " + token + "\n",
			wantErr: "mutually exclusive",
		},
		{
			name:    "insecure with tls files",
			body:    "hosts:\n  - name: host-a\n    endpoint: 10.0.0.1:9090\n    tls:\n      insecure: true\n      ca_file: /etc/ca.pem\n",
			wantErr: "contradictory",
		},
		{
			name:    "half a client certificate",
			body:    "hosts:\n  - name: host-a\n    endpoint: 10.0.0.1:9090\n    tls:\n      cert_file: /etc/cert.pem\n",
			wantErr: "go together",
		},
		{
			name:    "empty token file",
			body:    "hosts:\n  - name: host-a\n    endpoint: 10.0.0.1:9090\n    token_file: " + empty + "\n",
			wantErr: "is empty",
		},
		{
			name:    "missing token file",
			body:    "hosts:\n  - name: host-a\n    endpoint: 10.0.0.1:9090\n    token_file: " + filepath.Join(dir, "absent") + "\n",
			wantErr: "token_file",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "inventory.yaml")
			writeFile(t, path, tt.body)
			got, err := loadInventory(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadInventory = %v, want an error containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadInventory: %v", err)
			}
			if !sameEndpoints(got, tt.want) {
				t.Fatalf("endpoints = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// sameEndpoints compares two endpoint lists element by element.
func sameEndpoints(got, want []flintlock.Endpoint) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// writeFile writes body to path.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
