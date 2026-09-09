package config

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is a bytes.Buffer safe to write from the Reloader's goroutine
// while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reloadFixture is a configuration file in a temporary directory with a
// Reloader over it. logs captures everything the Reloader and Load write.
type reloadFixture struct {
	path string
	r    *Reloader
	logs *syncBuffer
}

// newReloadFixture writes twoProfileConfig to a temporary config.yaml, loads
// it and returns a Reloader over it. A test then rewrites the file and
// reloads.
func newReloadFixture(t *testing.T) *reloadFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, twoProfileConfig)
	logs := &syncBuffer{}
	opts := []Option{WithEnv(noEnv), WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))}
	cfg, err := Load(path, opts...)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return &reloadFixture{path: path, r: NewReloader(cfg, path, opts...), logs: logs}
}

// rewrite replaces the configuration file's content.
func (f *reloadFixture) rewrite(t *testing.T, content string) {
	t.Helper()
	writeFile(t, f.path, content)
}

// dropBuildersProfile returns cfg without its second Profile.
func dropBuildersProfile(t *testing.T, cfg string) string {
	t.Helper()
	before, _, found := strings.Cut(cfg, "  - name: builders")
	if !found {
		t.Fatalf("configuration has no builders Profile to drop:\n%s", cfg)
	}
	return before
}

// twoProfileConfig is a configuration with two Profiles and two Hosts; the
// reload tests remove one of each, or add one.
const twoProfileConfig = `
gitlab:
  url: https://gitlab.example.com
  token: glrt-reload
  name: metal-runner
  concurrent: 8
pool_manager:
  endpoint: 10.0.0.5:9091
inventory:
  hosts:
    - name: host-a
      endpoint: 10.0.1.10:9090
      arch: arm64
      vcpu: 8
      memory_mb: 16384
      labels: {tier: general}
    - name: host-b
      endpoint: 10.0.1.11:9090
      arch: arm64
      vcpu: 8
      memory_mb: 16384
      labels: {tier: builders}
profiles:
  - name: general
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    host_selector: {tier: general}
    pool:
      size: 2
  - name: builders
    arch: arm64
    kernel:
      image: ghcr.io/example/kernel@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567
    host_selector: {tier: builders}
    pool:
      size: 1
`

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# When the Runner receives `SIGHUP`, the Runner SHALL reload the
//# Profiles and the Inventory from disk without interrupting running Jobs.

// TestReloadOnSIGHUP checks the three parts of the requirement: a SIGHUP
// makes the Reloader re-read the file, the Profiles and the Inventory it
// publishes are the ones now on disk, and a caller that is already holding a
// configuration - a running Job - keeps the values it started with, because
// the swap publishes a new pointer instead of mutating the old one.
func TestReloadOnSIGHUP(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t)

	// What a running Job holds when the signal arrives.
	running := f.r.Current()
	if len(running.Profiles) != 2 || len(running.Inventory.Hosts) != 2 {
		t.Fatalf("initial configuration = %d profiles, %d hosts", len(running.Profiles), len(running.Inventory.Hosts))
	}
	runningProfile := running.Profiles[0]

	var reloaded *Config
	f.r.Subscribe(func(_ context.Context, cfg *Config) error {
		reloaded = cfg
		return nil
	})

	// A third Host and a bigger Pool for the first Profile, and the second
	// Profile removed.
	next := strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 5\n", 1)
	next = dropBuildersProfile(t, next)
	next = strings.Replace(next, "    - name: host-b\n", "    - name: host-c\n", 1)
	f.rewrite(t, next)

	ctx := t.Context()
	signals := make(chan os.Signal, 1)
	done := make(chan error, 1)
	go func() { done <- f.r.Serve(ctx, signals) }()

	signals <- syscall.SIGHUP
	close(signals)
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}

	after := f.r.Current()
	if len(after.Profiles) != 1 || after.Profiles[0].Pool.Size != 5 {
		t.Errorf("profiles after reload = %+v, want the one Profile with a Pool of 5", after.Profiles)
	}
	if len(after.Inventory.Hosts) != 2 || after.Inventory.Hosts[1].Name != "host-c" {
		t.Errorf("inventory after reload = %+v, want host-a and host-c", after.Inventory.Hosts)
	}
	if reloaded != after {
		t.Error("subscribers were not given the configuration that was published")
	}

	// The Job that was running when the signal arrived is untouched: it holds
	// the old pointer, and nothing under it changed.
	if len(running.Profiles) != 2 || running.Profiles[0].Pool.Size != 2 {
		t.Errorf("the running Job's configuration changed under it: %+v", running.Profiles)
	}
	if running.Profiles[0].Name != runningProfile.Name || running.Profiles[0].Pool.Size != runningProfile.Pool.Size {
		t.Error("the Profile a running Job resolved was mutated by the reload")
	}
	if len(running.Inventory.Hosts) != 2 || running.Inventory.Hosts[1].Name != "host-b" {
		t.Errorf("the running Job's Inventory changed under it: %+v", running.Inventory.Hosts)
	}
	if running == after {
		t.Error("Current() returned the same pointer after a reload that changed the file")
	}

	// Sections other than the Profiles and the Inventory keep their values
	// until a restart.
	if after.GitLab.URL != running.GitLab.URL || after.PoolManager.Endpoint != running.PoolManager.Endpoint {
		t.Error("a reload changed a section other than the Profiles and the Inventory")
	}
}

// TestReloadIsAtomicUnderConcurrentReaders checks that a Job reading the
// configuration while a reload runs always sees one whole configuration,
// never a half-swapped one, and that -race finds no data race between the
// readers and the reload.
func TestReloadIsAtomicUnderConcurrentReaders(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t)
	oneProfile := dropBuildersProfile(t, twoProfileConfig)
	oneProfile = strings.Replace(oneProfile, "    - name: host-b\n", "    - name: host-c\n", 1)

	ctx := t.Context()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				cfg := f.r.Current()
				// Each version of the file pairs a Profile count with a Host
				// name; a torn read would pair them the wrong way round.
				switch len(cfg.Profiles) {
				case 2:
					if cfg.Inventory.Hosts[1].Name != "host-b" {
						t.Errorf("torn read: 2 profiles with %q", cfg.Inventory.Hosts[1].Name)
						return
					}
				case 1:
					if cfg.Inventory.Hosts[1].Name != "host-c" {
						t.Errorf("torn read: 1 profile with %q", cfg.Inventory.Hosts[1].Name)
						return
					}
				default:
					t.Errorf("torn read: %d profiles", len(cfg.Profiles))
					return
				}
			}
		}()
	}

	for i := range 10 {
		if i%2 == 0 {
			f.rewrite(t, oneProfile)
		} else {
			f.rewrite(t, twoProfileConfig)
		}
		if err := f.r.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

//= docs/requirements/07-configuration.md#file-and-precedence
//= type=test
//# If a reload produces an invalid configuration, then the Runner
//# SHALL keep the previous configuration and log the validation error.

// TestReloadKeepsThePreviousConfigurationWhenInvalid checks that a file that
// no longer loads or no longer validates leaves the running configuration
// exactly as it was, that the reason is logged and returned, and that a
// later valid file is picked up again.
func TestReloadKeepsThePreviousConfigurationWhenInvalid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		wantLog string
	}{
		{
			name:    "a Profile with no Pool size",
			content: strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 0\n", 1),
			wantLog: "profiles[0].pool.size",
		},
		{
			name:    "an Inventory with duplicate Host names",
			content: strings.Replace(twoProfileConfig, "    - name: host-b\n", "    - name: host-a\n", 1),
			wantLog: "inventory.hosts[1].name",
		},
		{
			name:    "an image that is no longer pinned",
			content: strings.Replace(twoProfileConfig, "rootfs: ghcr.io/example/ubuntu-ci@sha256:89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567", "rootfs: ghcr.io/example/ubuntu-ci", 1),
			wantLog: "neither a digest nor a tag",
		},
		{
			name:    "a file that is not YAML any more",
			content: "profiles: [\n",
			wantLog: "reload",
		},
		{
			name:    "an empty file",
			content: "",
			wantLog: "at least one Profile is required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newReloadFixture(t)
			before := f.r.Current()
			var called int
			f.r.Subscribe(func(context.Context, *Config) error { called++; return nil })

			f.rewrite(t, tt.content)
			err := f.r.Reload(t.Context())
			if err == nil {
				t.Fatal("Reload accepted an invalid configuration")
			}

			if f.r.Current() != before {
				t.Error("the previous configuration was not kept")
			}
			if len(before.Profiles) != 2 || len(before.Inventory.Hosts) != 2 {
				t.Errorf("the previous configuration was modified: %d profiles, %d hosts",
					len(before.Profiles), len(before.Inventory.Hosts))
			}
			if called != 0 {
				t.Error("subscribers were run for a reload that failed")
			}
			logs := f.logs.String()
			if !strings.Contains(logs, "keeping the previous configuration") {
				t.Errorf("the rejection was not logged:\n%s", logs)
			}
			if !strings.Contains(logs, tt.wantLog) {
				t.Errorf("the log does not carry the reason %q:\n%s", tt.wantLog, logs)
			}
			if !strings.Contains(err.Error(), f.path) {
				t.Errorf("error = %v, want it to name the file", err)
			}

			// A corrected file is picked up.
			f.rewrite(t, strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 3\n", 1))
			if err := f.r.Reload(t.Context()); err != nil {
				t.Fatalf("Reload after the file was corrected: %v", err)
			}
			if got := f.r.Current().Profiles[0].Pool.Size; got != 3 {
				t.Errorf("pool size after the corrected reload = %d, want 3", got)
			}
		})
	}
}

// TestServeSIGHUPListensForTheSignal checks the wiring the Runner installs:
// ServeSIGHUP reloads on a real SIGHUP delivered to the process and returns
// when its context is cancelled, leaving no signal handler behind.
func TestServeSIGHUPListensForTheSignal(t *testing.T) {
	// Not parallel: it sends a signal to the whole process.
	f := newReloadFixture(t)
	f.rewrite(t, strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 4\n", 1))

	reloaded := make(chan struct{}, 1)
	f.r.Subscribe(func(context.Context, *Config) error {
		select {
		case reloaded <- struct{}{}:
		default:
		}
		return nil
	})

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.r.ServeSIGHUP(ctx) }()

	// signal.Notify may not have run yet when the first SIGHUP is sent, so
	// keep sending until the reload happens or the test times out. The
	// process cannot die from the signal: this test's own handler is
	// registered first, which stops the default action.
	own := make(chan os.Signal, 1)
	signal.Notify(own, syscall.SIGHUP)
	defer signal.Stop(own)

	deadline := time.After(10 * time.Second)
	for seen := false; !seen; {
		if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
			t.Fatalf("kill: %v", err)
		}
		select {
		case <-reloaded:
			seen = true
		case <-deadline:
			t.Fatal("no reload after SIGHUP")
		case <-time.After(20 * time.Millisecond):
		}
	}

	if got := f.r.Current().Profiles[0].Pool.Size; got != 4 {
		t.Errorf("pool size after SIGHUP = %d, want 4", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("ServeSIGHUP = %v, want nil on cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeSIGHUP did not return when its context was cancelled")
	}
}

// TestReloadReportsSubscriberErrors checks that a subscriber that fails does
// not undo the swap, so one component refusing the new configuration cannot
// leave the Runner half-reloaded.
func TestReloadReportsSubscriberErrors(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t)
	boom := errors.New("boom")
	var order []string
	f.r.Subscribe(func(context.Context, *Config) error { order = append(order, "first"); return boom })
	f.r.Subscribe(func(context.Context, *Config) error { order = append(order, "second"); return nil })

	f.rewrite(t, strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 6\n", 1))
	err := f.r.Reload(t.Context())
	if !errors.Is(err, boom) {
		t.Errorf("Reload = %v, want the subscriber error", err)
	}
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Errorf("subscribers ran %v, want both in registration order", order)
	}
	if got := f.r.Current().Profiles[0].Pool.Size; got != 6 {
		t.Errorf("pool size = %d, want the new 6; a subscriber error must not undo the swap", got)
	}
}

// TestReloadStopsOnContextCancellation checks that a cancelled context stops
// a reload before it touches the file.
func TestReloadStopsOnContextCancellation(t *testing.T) {
	t.Parallel()
	f := newReloadFixture(t)
	before := f.r.Current()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	f.rewrite(t, strings.Replace(twoProfileConfig, "      size: 2\n", "      size: 7\n", 1))
	if err := f.r.Reload(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Reload = %v, want context.Canceled", err)
	}
	if f.r.Current() != before {
		t.Error("a cancelled reload swapped the configuration anyway")
	}
}
