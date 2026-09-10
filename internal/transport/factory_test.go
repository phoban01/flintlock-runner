package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

//= docs/requirements/02-executor.md#guest-transport
//= type=test
//# The Guest Transport SHALL be selectable per Profile from the
//# implementations `exec` and `ssh`, with `exec` as the default.

// TestTransportKindSelectsTheImplementation builds a Transport for each Kind
// and checks which one was built by what it reaches for: the exec transport
// opens an ExecCommand stream, the ssh transport opens an SSH proxy stream,
// and a Profile that says nothing about its transport gets exec. A Kind that
// is neither is a configuration error rather than a silent fallback to the
// default.
func TestTransportKindSelectsTheImplementation(t *testing.T) {
	t.Parallel()
	key, public := generateKeyPair(t)

	cases := []struct {
		name      string
		kind      transport.Kind
		wantExec  int
		wantProxy int
	}{
		{name: "exec", kind: transport.KindExec, wantExec: 1},
		{name: "the default", kind: "", wantExec: 1},
		{name: "ssh", kind: transport.KindSSH, wantProxy: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			guest := newSSHGuest(t, public, "", "", 0)
			host := &stubHost{
				name: "h1",
				exec: func(context.Context) (flintlock.ExecStream, error) {
					return newScriptedStream(exitCode(0)), nil
				},
				sshProxy: func(context.Context, string) (io.ReadWriteCloser, error) {
					return guest.dialProxy(), nil
				},
			}
			tr, err := transport.NewFactory().New(ctx, transport.Target{
				Kind:  tc.kind,
				Host:  host,
				VMUID: "vm",
				SSH:   transport.SSHOptions{User: "runner", PrivateKey: key},
			})
			if err != nil {
				t.Fatalf("New(%q): %v", tc.kind, err)
			}
			t.Cleanup(func() { _ = tr.Close() })
			if _, err := tr.Run(ctx, transport.Command{Path: "true"}); err != nil {
				t.Fatalf("Run: %v", err)
			}
			execs, proxies, _ := host.counts()
			if execs != tc.wantExec || proxies != tc.wantProxy {
				t.Errorf("kind %q opened %d exec streams and %d proxy streams, want %d and %d",
					tc.kind, execs, proxies, tc.wantExec, tc.wantProxy)
			}
		})
	}

	t.Run("an unknown kind", func(t *testing.T) {
		t.Parallel()
		_, err := transport.NewFactory().New(context.Background(), transport.Target{
			Kind:  transport.Kind("telnet"),
			Host:  &stubHost{name: "h1"},
			VMUID: "vm",
		})
		if err == nil {
			t.Fatal("New accepted a guest transport that does not exist")
		}
		if !strings.Contains(err.Error(), "telnet") {
			t.Errorf("the error %q does not name the unknown transport", err)
		}
	})
}

//= docs/requirements/05-hosts.md#inventory
//= type=test
//# If a Host reports that the exec service is disabled, then the
//# Runner SHALL log a warning naming the Host, because Jobs the Pool Manager
//# places there cannot be run over the `exec` Guest Transport.

// TestFactoryRefusesAHostWithoutTheService asks for a transport on a Host
// that reports the service it needs disabled. Building it has to fail with
// ErrServiceDisabled and log a warning naming the Host, because a Job placed
// there cannot be run over that transport and saying so is more use than a
// stream error later. A Host that does not implement ServerInfo says nothing
// either way, so the transport is built (HO-013).
func TestFactoryRefusesAHostWithoutTheService(t *testing.T) {
	t.Parallel()
	key, _ := generateKeyPair(t)

	cases := []struct {
		name string
		kind transport.Kind
		info *flintlock.HostInfo
	}{
		{
			name: "exec disabled",
			kind: transport.KindExec,
			info: &flintlock.HostInfo{Name: "h1", VersionKnown: true, SSHProxy: flintlock.GuestService{Enabled: true}},
		},
		{
			name: "ssh proxy disabled",
			kind: transport.KindSSH,
			info: &flintlock.HostInfo{Name: "h1", VersionKnown: true, Exec: flintlock.GuestService{Enabled: true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
			defer cancel()
			rec := &records{}
			host := &stubHost{name: "h1", info: tc.info}
			_, err := transport.NewFactory(transport.WithLogger(rec.logger())).New(ctx, transport.Target{
				Kind:  tc.kind,
				Host:  host,
				VMUID: "vm",
				SSH:   transport.SSHOptions{PrivateKey: key},
			})
			if !errors.Is(err, transport.ErrServiceDisabled) {
				t.Fatalf("New returned %v, want ErrServiceDisabled", err)
			}
			if !strings.Contains(err.Error(), "h1") {
				t.Errorf("the error %q does not name the host", err)
			}
			if rec.warning("h1") == nil {
				t.Errorf("no warning names the host; the log was %v", rec.all())
			}
		})
	}

	t.Run("a host without ServerInfo", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
		defer cancel()
		host := &stubHost{
			name:    "old",
			infoErr: flintlock.ErrUnimplemented,
			exec: func(context.Context) (flintlock.ExecStream, error) {
				return newScriptedStream(exitCode(0)), nil
			},
		}
		tr, err := transport.NewFactory().New(ctx, transport.Target{Host: host, VMUID: "vm"})
		if err != nil {
			t.Fatalf("New against a host without ServerInfo: %v", err)
		}
		t.Cleanup(func() { _ = tr.Close() })
		if _, err := tr.Run(ctx, transport.Command{Path: "true"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
}

// records captures slog output so that a test can assert on the warnings.
type records struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// logger returns a logger writing into the recorder.
func (r *records) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(r, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Write implements io.Writer.
func (r *records) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

// all returns the captured records.
func (r *records) all() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(r.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out
}

// warning returns the first warning about a host, or nil.
func (r *records) warning(host string) map[string]any {
	for _, rec := range r.all() {
		if rec["level"] == "WARN" && rec["host"] == host {
			return rec
		}
	}
	return nil
}
