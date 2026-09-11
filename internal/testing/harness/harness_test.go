package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"github.com/liquidmetal-dev/flintlock/api/types"
	"gitlab.com/gitlab-org/gitlab-runner/common/spec"
	"gopkg.in/yaml.v3"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/flintlock"
	hostfake "github.com/phoban01/flintlock-runner/internal/flintlock/fake"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	pmfake "github.com/phoban01/flintlock-runner/internal/poolmgr/fake"
)

// These tests exercise the harness without the Runner binary, so they run
// under plain `go test`. The scenarios that run the binary are in
// e2e_test.go behind the e2e build tag (`make e2e`).

// start starts a Stack for a test whose Shutdown the test checks itself; a
// cleanup shuts it down in case the test fails first.
func start(t *testing.T, opts Options) *Stack {
	t.Helper()
	if opts.Logf == nil {
		opts.Logf = t.Logf
	}
	s, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	return s
}

// poolSpec is a Pool in the harness namespace over hosts, as the Runner
// would declare it, for tests that drive the Pool Manager without a Runner.
func poolSpec(hosts []string) poolmgr.PoolSpec {
	return poolmgr.PoolSpec{
		Ref:                      poolmgr.PoolRef{Namespace: Namespace, Name: ProfileName},
		Template:                 &types.MicroVMSpec{Namespace: Namespace, Vcpu: 1, MemoryInMb: 256},
		Size:                     1,
		FlintlockHosts:           hosts,
		Replenishment:            poolmgr.ReplenishmentStrategy{Type: poolmgrv1.ReplenishmentStrategyType_IMMEDIATE_ON_LEASE},
		HookFailurePolicy:        poolmgrv1.HookFailurePolicy_DELETE_AND_REPLACE,
		HeartbeatInterval:        10 * time.Second,
		HeartbeatExpiryThreshold: 60 * time.Second,
	}
}

// claim declares a Pool through c and claims its MicroVM, retrying while
// the Pool fills.
func claim(t *testing.T, c poolmgr.Client, hosts []string) *poolmgr.Claim {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.CreatePool(ctx, poolSpec(hosts)); err != nil {
		t.Fatalf("CreatePool: %v", err)
	}
	for {
		cl, err := c.ClaimVM(ctx, poolmgr.PoolRef{Namespace: Namespace, Name: ProfileName})
		if err == nil {
			return cl
		}
		if !errors.Is(err, poolmgr.ErrExhausted) {
			t.Fatalf("ClaimVM: %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ClaimVM: pool never filled: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func hostNames(s *Stack) []string {
	var names []string
	for _, h := range s.Hosts {
		names = append(names, h.Config().Name)
	}
	return names
}

func TestStartWritesAConfigurationForTheFakes(t *testing.T) {
	s := start(t, Options{Hosts: 3, PoolSize: 2})

	if got := len(s.Hosts); got != 3 {
		t.Fatalf("started %d fake Hosts, want 3", got)
	}
	cfg := s.Config
	if cfg.GitLab.URL != s.GitLab.URL() || string(cfg.GitLab.Token) != RunnerToken {
		t.Errorf("gitlab = %s with token %q, want the fake at %s", cfg.GitLab.URL, cfg.GitLab.Token, s.GitLab.URL())
	}
	if cfg.PoolManager.Endpoint != s.PoolManager.Addr() || !cfg.PoolManager.TLS.Insecure {
		t.Errorf("pool_manager = %s (insecure %v), want the fake at %s in plaintext", cfg.PoolManager.Endpoint, cfg.PoolManager.TLS.Insecure, s.PoolManager.Addr())
	}
	if cfg.Scheduler.Namespace != Namespace {
		t.Errorf("scheduler.namespace = %q, want %q", cfg.Scheduler.Namespace, Namespace)
	}
	if cfg.DistributedCache != nil {
		t.Errorf("distributed_cache = %+v, want none", cfg.DistributedCache)
	}
	for name, svc := range map[string]config.Service{
		"buildkit": cfg.HostServices.Buildkit.Service, "go_proxy": cfg.HostServices.GoProxy.Service,
		"registry_mirror": cfg.HostServices.RegistryMirror.Service, "http_cache": cfg.HostServices.HTTPCache.Service,
	} {
		if svc.IsEnabled() {
			t.Errorf("host service %s is enabled, want every one disabled", name)
		}
	}

	if len(cfg.Profiles) != 1 {
		t.Fatalf("%d profiles, want 1", len(cfg.Profiles))
	}
	p := cfg.Profiles[0]
	if !p.Default || p.Transport.Kind != config.TransportExec || p.Pool.Size != 2 {
		t.Errorf("profile = default %v, transport %s, pool size %d; want the default exec profile of size 2", p.Default, p.Transport.Kind, p.Pool.Size)
	}
	if filepath.Base(p.Shell) != "bash" || !filepath.IsAbs(p.Shell) {
		t.Errorf("profile shell = %q, want this machine's bash", p.Shell)
	}
	// The generated scripts use these as absolute paths on this machine,
	// so they have to be under the root and never /builds or /cache.
	for field, dir := range map[string]string{"builds_dir": p.BuildsDir, "cache_dir": p.CacheDir} {
		if !strings.HasPrefix(dir, s.Root+string(filepath.Separator)) {
			t.Errorf("profile %s = %q, want a directory under the harness root %s", field, dir, s.Root)
		}
	}

	// CF-032: the Inventory names are the names the Pool Manager places on.
	var inventory []string
	for _, h := range cfg.Inventory.Hosts {
		inventory = append(inventory, h.Name)
		if string(h.Token) != HostToken || !h.TLS.Insecure {
			t.Errorf("inventory %s: token %q insecure %v, want the fake's token in plaintext", h.Name, h.Token, h.TLS.Insecure)
		}
	}
	if pm := s.pmHosts.Names(); !slices.Equal(pm, inventory) || !slices.Equal(pm, hostNames(s)) {
		t.Errorf("pool manager hosts %v, inventory %v, fake hosts %v: want the same names", pm, inventory, hostNames(s))
	}

	info, err := os.Stat(s.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode %o, want 600: it holds the tokens", perm)
	}
}

func TestShutdownIsCleanAndRemovesTheRoot(t *testing.T) {
	s := start(t, Options{})
	c := s.PoolManager.Client()
	defer func() { _ = c.Close() }()
	cl := claim(t, c, hostNames(s))
	if err := c.ReleaseVM(context.Background(), cl.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown after the lease was released: %v", err)
	}
	if _, err := os.Stat(s.Root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("root %s still exists after Shutdown (stat: %v)", s.Root, err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The harness SHALL fail if any scenario leaves a Lease held or
//# a sandbox directory behind after the Runner has shut down.

func TestShutdownFailsOnAHeldLease(t *testing.T) {
	s := start(t, Options{})
	c := s.PoolManager.Client()
	defer func() { _ = c.Close() }()
	cl := claim(t, c, hostNames(s))

	err := s.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown succeeded with a Lease still held")
	}
	if !strings.Contains(err.Error(), cl.LeaseID) {
		t.Errorf("Shutdown error does not name the held lease %s: %v", cl.LeaseID, err)
	}
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The harness SHALL fail if any scenario leaves a Lease held or
//# a sandbox directory behind after the Runner has shut down.

func TestShutdownFailsOnALeftoverSandbox(t *testing.T) {
	s := start(t, Options{})
	// A MicroVM the Pool Manager does not know about is one nothing will
	// delete: its sandbox outlives the Stack.
	vm, err := s.Hosts[0].Client().CreateMicroVM(context.Background(), &types.MicroVMSpec{Namespace: Namespace})
	if err != nil {
		t.Fatalf("CreateMicroVM: %v", err)
	}
	uid := vm.GetSpec().GetUid()

	err = s.Shutdown(context.Background())
	if err == nil {
		t.Fatal("Shutdown succeeded with a sandbox left on a fake Host")
	}
	if !strings.Contains(err.Error(), uid) {
		t.Errorf("Shutdown error does not name the leftover sandbox %s: %v", uid, err)
	}
}

func TestSandboxHoldsTheProfileDirectories(t *testing.T) {
	s := start(t, Options{Hosts: 1})
	c := s.PoolManager.Client()
	defer func() { _ = c.Close() }()
	cl := claim(t, c, hostNames(s))

	sandbox, ok := s.Hosts[0].SandboxPath(cl.VMUID)
	if !ok {
		t.Fatalf("host-1 has no sandbox for the claimed microvm %s", cl.VMUID)
	}
	p := s.Config.Profiles[0]
	for _, dir := range []string{p.BuildsDir, p.CacheDir} {
		if info, err := os.Stat(filepath.Join(sandbox, dir)); err != nil || !info.IsDir() {
			t.Errorf("sandbox is missing %s, which a Stage's cwd resolves to: %v", dir, err)
		}
	}
	if err := c.ReleaseVM(context.Background(), cl.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
}

func TestOptionsFromEnv(t *testing.T) {
	env := map[string]string{
		EnvHardwareInventory: "/etc/inventory.yaml",
		EnvPoolManager:       "10.0.0.5:9440",
		EnvKernelImage:       "k:1",
		EnvRootFSImage:       "r:1",
		EnvRunnerBinary:      "/bin/flintlock-runner",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	got := optionsFromLookup(Options{PoolManagerEndpoint: "explicit:1"}, lookup)
	want := Options{
		HardwareInventory: "/etc/inventory.yaml", PoolManagerEndpoint: "explicit:1",
		KernelImage: "k:1", RootFSImage: "r:1", RunnerBinary: "/bin/flintlock-runner",
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("optionsFromLookup = %+v, want %+v (explicit options win)", got, want)
	}

	none := func(string) (string, bool) { return "", false }
	if got := optionsFromLookup(Options{}, none); got.Hardware() || got.PoolManagerEndpoint != "" {
		t.Errorf("with no environment: %+v, want the fake tier", got)
	}

	fake := fakeTier(lookup)
	if fake.Hardware() || fake.PoolManagerEndpoint != "" || fake.RunnerBinary != env[EnvRunnerBinary] {
		t.Errorf("fakeTier with a hardware inventory set = %+v, want fakes throughout and the binary kept", fake)
	}
	delete(env, EnvHardwareInventory)
	if fake := fakeTier(lookup); fake.PoolManagerEndpoint != env[EnvPoolManager] {
		t.Errorf("fakeTier with only a pool manager set: endpoint %q, want %q", fake.PoolManagerEndpoint, env[EnvPoolManager])
	}
}

// skipRecorder is a testing.TB whose Skip does not stop the test, so a
// test can see whether a helper skipped.
type skipRecorder struct {
	testing.TB
	skipped bool
}

func (r *skipRecorder) Skipf(string, ...any) { r.skipped = true }

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# Where the environment variable naming a hardware Inventory is
//# set, the harness SHALL run the same scenarios against the real Hosts it
//# lists instead of fake Hosts, and SHALL be skipped otherwise.

func TestHardwareTierIsSkippedWithoutAnInventory(t *testing.T) {
	r := &skipRecorder{TB: t}
	hardwareTier(r, func(string) (string, bool) { return "", false })
	if !r.skipped {
		t.Error("the hardware tier ran with no hardware Inventory named")
	}

	r = &skipRecorder{TB: t}
	opts := hardwareTier(r, func(k string) (string, bool) {
		if k == EnvHardwareInventory {
			return "/etc/inventory.yaml", true
		}
		return "", false
	})
	if r.skipped || opts.HardwareInventory != "/etc/inventory.yaml" {
		t.Errorf("with %s set: skipped %v, options %+v; want it to run against that Inventory", EnvHardwareInventory, r.skipped, opts)
	}
}

// serveHost serves a fake Host outside any Stack, standing in for a real
// flintlockd, and stops it at the end of the test.
func serveHost(t *testing.T, name string) *hostfake.Host {
	t.Helper()
	h := hostfake.New(flintlock.FakeHostConfig{
		Name: name, Listen: "127.0.0.1:0", SandboxRoot: filepath.Join(t.TempDir(), name),
		ExecEnabled: true, Token: "metal-token",
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	select {
	case <-h.Ready():
	case err := <-done:
		t.Fatalf("serving %s: %v", name, err)
	}
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# Where the environment variable naming a hardware Inventory is
//# set, the harness SHALL run the same scenarios against the real Hosts it
//# lists instead of fake Hosts, and SHALL be skipped otherwise.

func TestHardwareInventoryReplacesTheFakeHosts(t *testing.T) {
	metal := serveHost(t, "metal-1")
	inv := config.InventoryFile{Hosts: []config.HostEntry{{
		Name: "metal-1", Endpoint: metal.Addr(), Arch: config.ArchARM64, VCPU: 4, MemoryMB: 8192,
		Token: "metal-token", TLS: config.ClientTLS{Insecure: true},
	}}}
	data, err := yaml.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Start(context.Background(), Options{HardwareInventory: path}); err == nil ||
		!strings.Contains(err.Error(), EnvKernelImage) {
		t.Errorf("Start on the hardware tier without images: %v, want an error naming %s", err, EnvKernelImage)
	}

	s := start(t, Options{HardwareInventory: path, KernelImage: "ghcr.io/x/kernel:6.1", RootFSImage: "ghcr.io/x/rootfs:1"})
	if len(s.Hosts) != 0 {
		t.Errorf("started %d fake Hosts on the hardware tier, want none", len(s.Hosts))
	}
	if got := s.Config.Inventory.Hosts; len(got) != 1 || got[0].Name != "metal-1" || got[0].Endpoint != metal.Addr() {
		t.Errorf("runner inventory = %+v, want exactly metal-1 at %s", got, metal.Addr())
	}
	p := s.Config.Profiles[0]
	if p.BuildsDir != config.DefaultBuildsDir || p.Shell != config.DefaultShell || p.RootFS != "ghcr.io/x/rootfs:1" {
		t.Errorf("hardware profile = builds %s shell %s rootfs %s, want the guest's own paths and the given image", p.BuildsDir, p.Shell, p.RootFS)
	}

	// The fake Pool Manager places on the listed Host over gRPC.
	c := s.PoolManager.Client()
	defer func() { _ = c.Close() }()
	cl := claim(t, c, []string{"metal-1"})
	if cl.Host.Name != "metal-1" {
		t.Errorf("claim placed on %q, want metal-1", cl.Host.Name)
	}
	vms, err := metal.Client().ListMicroVMs(context.Background(), Namespace)
	if err != nil || len(vms) == 0 {
		t.Errorf("metal-1 lists %d microvms in %s (%v), want the pool's", len(vms), Namespace, err)
	}
	if err := c.ReleaseVM(context.Background(), cl.LeaseID); err != nil {
		t.Fatalf("ReleaseVM: %v", err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if vms, err := metal.Client().ListMicroVMs(context.Background(), Namespace); err != nil || len(vms) != 0 {
		t.Errorf("after Shutdown metal-1 lists %d microvms in %s (%v), want none", len(vms), Namespace, err)
	}
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# Where the environment variable naming a real Pool Manager
//# endpoint is set, the harness SHALL use it instead of the fake Pool
//# Manager.

func TestPoolManagerEndpointReplacesTheFake(t *testing.T) {
	// The "real" Pool Manager is a fake served on its own, which the
	// harness only knows by address.
	external := pmfake.NewHosts()
	pm := pmfake.New(poolmgr.FakeConfig{Listen: "127.0.0.1:0", Hosts: external, ReconcileInterval: 100 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- pm.Serve(ctx) }()
	<-pm.Ready()
	t.Cleanup(func() {
		cancel()
		<-served
	})

	s := start(t, Options{Hosts: 1, PoolManagerEndpoint: pm.Addr()})
	if s.PoolManager != nil {
		t.Error("the harness started its own Pool Manager with an endpoint given")
	}
	if s.Config.PoolManager.Endpoint != pm.Addr() || s.PoolManagerAddr() != pm.Addr() {
		t.Errorf("pool_manager.endpoint = %s, want the given %s", s.Config.PoolManager.Endpoint, pm.Addr())
	}

	external.Add("host-1", s.Hosts[0].Client(), s.Hosts[0].Addr())
	c, err := pmfake.Dial(pm.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	claim(t, c, []string{"host-1"})

	// The lease check asks the given Pool Manager, not a fake of its own.
	err = s.Shutdown(context.Background())
	if err == nil || !strings.Contains(err.Error(), "leased") {
		t.Errorf("Shutdown with a lease held at the given Pool Manager: %v, want the held lease reported", err)
	}
}

func TestBuildJob(t *testing.T) {
	job := BuildJob(7, Job{
		Name:        "fails",
		Script:      []string{"echo hi", "exit 3"},
		AfterScript: []string{"echo after"},
		Timeout:     90 * time.Second,
		Variables:   map[string]string{"FOO": "bar", "GIT_STRATEGY": "none"},
	})
	if job.ID != 7 || job.Token == "" || job.RunnerInfo.Timeout != 90 {
		t.Errorf("id %d token %q timeout %d, want 7, a token and 90", job.ID, job.Token, job.RunnerInfo.Timeout)
	}
	vars := map[string]string{}
	for _, v := range job.Variables {
		vars[v.Key] = v.Value
	}
	if vars["GIT_STRATEGY"] != "none" || vars["CI_JOB_ID"] != "7" || vars["FOO"] != "bar" {
		t.Errorf("variables %v, want GIT_STRATEGY=none, CI_JOB_ID=7 and FOO=bar", vars)
	}
	if len(job.Steps) != 2 || job.Steps[0].Name != spec.StepNameScript || job.Steps[1].Name != spec.StepNameAfterScript ||
		!slices.Equal([]string(job.Steps[0].Script), []string{"echo hi", "exit 3"}) {
		t.Errorf("steps = %+v, want the script then after_script", job.Steps)
	}
	if len(job.Artifacts) != 0 || len(job.Cache) != 0 {
		t.Error("the job carries artifacts or cache, which need gitlab-runner-helper")
	}
}
