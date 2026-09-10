package scheduler

import (
	"context"
	"errors"
	"testing"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// profileFixtures are the Profiles the resolution tests share: an exact-name
// Profile, a glob Profile and the Default Profile, in configuration order.
func profileFixtures() []config.Profile {
	golang := testProfile("golang", 2)
	golang.Default = false
	golang.Images = []string{"golang:1.26"}
	golang.ImageGlobs = []string{"golang:*"}

	rust := testProfile("rust", 2)
	rust.Default = false
	rust.Images = []string{"rust:1.90"}
	rust.ImageGlobs = []string{"rust:*", "*-rust"}

	def := testProfile("default", 2)
	def.Default = true
	return []config.Profile{golang, rust, def}
}

//= docs/requirements/03-scheduler.md#profile-resolution
//= type=test
//# The Scheduler SHALL resolve a Profile for a Job by matching the
//# Job Image name against Profile image names exactly, then against Profile
//# image glob patterns in configuration order, then by falling back to the
//# Default Profile.

func TestResolveProfileMatchesExactThenGlobThenDefault(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{name: "exact name", image: "golang:1.26", want: "golang"},
		{name: "exact name of a later profile", image: "rust:1.90", want: "rust"},
		{name: "glob in configuration order", image: "golang:1.25", want: "golang"},
		{name: "glob of a later profile", image: "rust:nightly", want: "rust"},
		{name: "second glob of a profile", image: "musl-rust", want: "rust"},
		{name: "no image falls back to the default profile", image: "", want: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, envConfig{profiles: profileFixtures()})
			got, err := e.sched.ResolveProfile(JobInfo{ID: 7, Image: tt.image})
			if err != nil {
				t.Fatalf("ResolveProfile(%q): %v", tt.image, err)
			}
			if got.Name != tt.want {
				t.Fatalf("ResolveProfile(%q) = %q, want %q", tt.image, got.Name, tt.want)
			}
			if got.PoolRef != (poolmgr.PoolRef{Name: tt.want, Namespace: testNamespace}) {
				t.Fatalf("resolved pool = %s, want %s/%s", got.PoolRef, testNamespace, tt.want)
			}
		})
	}
}

func TestResolveProfilePrefersAnExactNameOverAnEarlierGlob(t *testing.T) {
	t.Parallel()
	// The first Profile's glob would match, but the second names the image
	// exactly, and exact names are tried first across every Profile.
	globbed := testProfile("globbed", 1)
	globbed.Default = false
	globbed.ImageGlobs = []string{"*"}
	exact := testProfile("exact", 1)
	exact.Default = false
	exact.Images = []string{"ubuntu:24.04"}

	e := newEnv(t, envConfig{profiles: []config.Profile{globbed, exact}})
	got, err := e.sched.ResolveProfile(JobInfo{Image: "ubuntu:24.04"})
	if err != nil {
		t.Fatalf("ResolveProfile: %v", err)
	}
	if got.Name != "exact" {
		t.Fatalf("ResolveProfile = %q, want the profile that names the image exactly", got.Name)
	}
}

//= docs/requirements/03-scheduler.md#profile-resolution
//= type=test
//# If a Job has no Job Image and no Default Profile is configured,
//# then the Scheduler SHALL report that no Profile can be resolved.

func TestResolveProfileWithoutImageAndWithoutDefault(t *testing.T) {
	t.Parallel()
	only := testProfile("golang", 1)
	only.Default = false
	only.Images = []string{"golang:1.26"}

	e := newEnv(t, envConfig{profiles: []config.Profile{only}})
	got, err := e.sched.ResolveProfile(JobInfo{ID: 3})
	if got != nil {
		t.Fatalf("ResolveProfile returned %q, want no profile", got.Name)
	}
	if !errors.Is(err, ErrNoProfile) {
		t.Fatalf("error = %v, want ErrNoProfile", err)
	}
	var perr *ProfileError
	if !errors.As(err, &perr) || perr.Image != "" {
		t.Fatalf("error = %v, want a ProfileError with no image", err)
	}
}

//= docs/requirements/03-scheduler.md#profile-resolution
//= type=test
//# If a Job Image matches no Profile and the configuration does not
//# allow falling back to the Default Profile for unknown images, then the
//# Scheduler SHALL report that no Profile can be resolved.

func TestResolveProfileUnknownImage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		allow       bool
		wantProfile string
	}{
		{name: "fallback not allowed", allow: false},
		{name: "fallback allowed", allow: true, wantProfile: "default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t, envConfig{
				profiles: profileFixtures(),
				tune: func(s *Settings) {
					s.Scheduler.AllowDefaultForUnknownImages = tt.allow
				},
			})
			got, err := e.sched.ResolveProfile(JobInfo{ID: 9, Image: "ubuntu:24.04"})
			if tt.wantProfile != "" {
				if err != nil {
					t.Fatalf("ResolveProfile: %v", err)
				}
				if got.Name != tt.wantProfile {
					t.Fatalf("ResolveProfile = %q, want %q", got.Name, tt.wantProfile)
				}
				return
			}
			if got != nil {
				t.Fatalf("ResolveProfile returned %q, want no profile", got.Name)
			}
			var perr *ProfileError
			if !errors.As(err, &perr) || perr.Image != "ubuntu:24.04" {
				t.Fatalf("error = %v, want a ProfileError naming the image", err)
			}
		})
	}
}

//= docs/requirements/03-scheduler.md#profile-resolution
//= type=test
//# The Scheduler SHALL resolve the Profile before claiming any
//# MicroVM so that a Job with an unknown image fails without consuming a warm
//# MicroVM.

func TestUnknownImageConsumesNoWarmMicroVM(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)

	host := newFakeHost(t, "host-1")
	pm, client := newFakePoolManager(t, ctx, map[string]flintlock.PoolHostClient{"host-1": host.Client()})
	counting := newCountingClient(client)

	e := newEnv(t, envConfig{
		client:   counting,
		profiles: profileFixtures(),
		hosts:    map[string]flintlock.HostClient{"host-1": runnerClient(t, host)},
	})
	e.declarer.admin = client
	e.startBare(ctx)
	e.sched.declareAll(ctx)

	pool := e.poolOf("golang")
	waitForClaimable(t, ctx, client, pool)
	before := poolAvailable(t, ctx, client, pool)
	if before == 0 {
		t.Fatal("the pool has no warm microvm to consume")
	}

	// A Job whose image matches nothing fails at resolution, so Allocate is
	// never reached and nothing is claimed.
	if _, err := e.sched.ResolveProfile(JobInfo{ID: 1, Image: "ubuntu:24.04"}); !errors.Is(err, ErrNoProfile) {
		t.Fatalf("ResolveProfile = %v, want ErrNoProfile", err)
	}
	if got := counting.claimCount(); got != 0 {
		t.Fatalf("claims made while resolving a profile = %d, want 0", got)
	}
	if got := poolAvailable(t, ctx, client, pool); got != before {
		t.Fatalf("available warm microvms = %d, want %d unchanged", got, before)
	}
	if leases := pm.Leases(); len(leases) != 0 {
		t.Fatalf("leases held after a failed resolution = %d, want 0", len(leases))
	}
}

// poolAvailable is the Pool's available count at the Pool Manager.
func poolAvailable(t *testing.T, ctx context.Context, client poolmgr.Client, ref poolmgr.PoolRef) int32 {
	t.Helper()
	pool, err := client.GetPool(ctx, ref)
	if err != nil {
		t.Fatalf("GetPool(%s): %v", ref, err)
	}
	return pool.Status.Available
}
