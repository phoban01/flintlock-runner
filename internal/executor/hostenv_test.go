package executor

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/transport"
)

// hostServicesConfig enables every Host Service, with private module
// patterns and two HTTP cache upstreams.
func hostServicesConfig() config.HostServices {
	return config.HostServices{
		GoProxy: config.GoProxy{PrivatePatterns: []string{"gitlab.example.com/*", "go.example.org/private"}},
		HTTPCache: config.HTTPCache{Upstreams: []config.HTTPCacheUpstream{
			{Name: "nodejs", URL: "https://nodejs.org/dist", EnvVar: "NODEJS_MIRROR"},
			{Name: "pypi", URL: "https://pypi.org", EnvVar: "PIP_INDEX_URL"},
		}},
	}
}

// hostEntry is an Inventory entry with every Host Service address.
func hostEntry() config.HostEntry {
	return config.HostEntry{
		Name: "host-a",
		Services: config.HostServiceAddresses{
			Buildkit:       "tcp://172.31.0.1:1234",
			GoProxy:        "http://172.31.0.1:3000",
			RegistryMirror: "172.31.0.1:5000",
			HTTPCache:      map[string]string{"nodejs": "http://172.31.0.1:3128/nodejs", "pypi": "http://172.31.0.1:3128/pypi"},
		},
	}
}

func disabled() *bool { f := false; return &f }

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# When the Placement of a Job's MicroVM is known, the Executor
//# SHALL add to the Job's environment the variables that point at the Host
//# Services of that Host: `BUILDKIT_HOST`, `GOPROXY`, `GOFLAGS` with
//# `-modcacherw`, the registry mirror address as `CI_REGISTRY_MIRROR`, and
//# one variable per configured HTTP cache upstream under the name given in
//# the configuration.

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# When the Go module proxy is configured with private module
//# patterns, the Executor SHALL set `GONOSUMDB` to those patterns and
//# `GONOPROXY` to the empty string so that the Go toolchain fetches private
//# modules through the Host's proxy and does not consult the public checksum
//# database for them.

func TestResolveHostServiceEnv(t *testing.T) {
	t.Parallel()
	host := hostEntry()
	got := NewHostServiceEnv(hostServicesConfig()).Resolve(&host)
	want := map[string]string{
		"BUILDKIT_HOST":      "tcp://172.31.0.1:1234",
		"GOPROXY":            "http://172.31.0.1:3000",
		"GOFLAGS":            "-modcacherw",
		"GONOSUMDB":          "gitlab.example.com/*,go.example.org/private",
		"GONOPROXY":          "",
		"CI_REGISTRY_MIRROR": "172.31.0.1:5000",
		"NODEJS_MIRROR":      "http://172.31.0.1:3128/nodejs",
		"PIP_INDEX_URL":      "http://172.31.0.1:3128/pypi",
	}
	if !reflect.DeepEqual(got.Vars, want) {
		t.Errorf("vars = %v\nwant %v", got.Vars, want)
	}
	if v, ok := got.Vars["GONOPROXY"]; !ok || v != "" {
		t.Error("GONOPROXY is not set to the empty string")
	}
	wantServices := []string{ServiceBuildkit, ServiceGoProxy, ServiceRegistryMirror, ServiceHTTPCache}
	if !reflect.DeepEqual(got.Services, wantServices) {
		t.Errorf("services = %v, want %v", got.Services, wantServices)
	}

	cfg := hostServicesConfig()
	cfg.GoProxy.PrivatePatterns = nil
	got = NewHostServiceEnv(cfg).Resolve(&host)
	if _, ok := got.Vars["GONOSUMDB"]; ok {
		t.Error("GONOSUMDB set without private patterns")
	}
	if _, ok := got.Vars["GONOPROXY"]; ok {
		t.Error("GONOPROXY set without private patterns")
	}
}

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# Where a Host Service is disabled or the Host's Inventory entry
//# lacks its address, the Executor SHALL omit that service's variables rather
//# than point them at an unreachable address.

func TestResolveOmitsDisabledAndUnaddressedServices(t *testing.T) {
	t.Parallel()
	cfg := hostServicesConfig()
	cfg.Buildkit.Enabled = disabled()
	cfg.HTTPCache.Enabled = disabled()
	host := hostEntry()
	host.Services.GoProxy = ""
	got := NewHostServiceEnv(cfg).Resolve(&host)
	for _, k := range []string{"BUILDKIT_HOST", "GOPROXY", "GOFLAGS", "GONOSUMDB", "GONOPROXY", "NODEJS_MIRROR", "PIP_INDEX_URL"} {
		if v, ok := got.Vars[k]; ok {
			t.Errorf("%s = %q for a disabled or unaddressed service", k, v)
		}
	}
	if got.Vars["CI_REGISTRY_MIRROR"] != "172.31.0.1:5000" {
		t.Errorf("the registry mirror, enabled and addressed, is missing: %v", got.Vars)
	}
	if !reflect.DeepEqual(got.Services, []string{ServiceRegistryMirror}) {
		t.Errorf("services = %v", got.Services)
	}
	if env := NewHostServiceEnv(cfg).Resolve(nil); len(env.Vars) != 0 || len(env.Services) != 0 {
		t.Errorf("a host missing from the inventory got %v", env)
	}
}

// resolverFunc is a HostServiceEnvResolver from a function.
type resolverFunc func(*config.HostEntry) HostServiceEnv

func (f resolverFunc) Resolve(h *config.HostEntry) HostServiceEnv { return f(h) }

// inventoryMap is an InventoryLookup over a map.
type inventoryMap map[string]config.HostEntry

func (m inventoryMap) Host(name string) (*config.HostEntry, bool) {
	h, ok := m[name]
	if !ok {
		return nil, false
	}
	return &h, true
}

// withHostServices wires the fixture with the production resolver over the
// full configuration and an Inventory holding host-a.
func withHostServices(f *fixture) {
	cfg := hostServicesConfig()
	f.deps.Env = NewHostServiceEnv(cfg)
	f.deps.Inventory = inventoryMap{"host-a": hostEntry()}
	f.opts = append(f.opts, WithHTTPCacheUpstreams(cfg.HTTPCache.Upstreams))
}

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# The Executor SHALL add the Host Service variables before the
//# `prepare_script` Stage so that every Stage, including `get_sources`, sees
//# them.

// TestHostServiceVariablesReachEveryStage runs a whole Job. When the first
// Stage, prepare_script, reaches the guest the Job's variables already hold
// the Host Service variables, and get_sources and step_script export them.
// (The bash prepare_script exports no variables at all; it is the moment
// they are added that matters for it.)
func TestHostServiceVariablesReachEveryStage(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withHostServices(f)
	var atFirstStage string
	f.tr.hook = func(_ context.Context, _ transport.Command, _ []byte) (int, error) {
		if atFirstStage == "" {
			atFirstStage = "unset"
			if v := f.currentBuild.GetAllVariables().Value("BUILDKIT_HOST"); v != "" {
				atFirstStage = v
			}
		}
		return 0, nil
	}
	if _, _, err := f.runBuild(context.Background(), testJob()); err != nil {
		t.Fatal(err)
	}
	if atFirstStage != "tcp://172.31.0.1:1234" {
		t.Errorf("BUILDKIT_HOST when prepare_script ran = %q", atFirstStage)
	}
	for _, marker := range []string{"Skipping Git repository setup", `echo "$JOB_SECRET_VALUE"`} {
		s := string(stageScript(t, f.tr.stages(), marker).Stdin)
		for _, want := range []string{"BUILDKIT_HOST", "tcp://172.31.0.1:1234", "NODEJS_MIRROR", "GONOSUMDB"} {
			if !strings.Contains(s, want) {
				t.Errorf("the stage with %q lacks %q", marker, want)
			}
		}
	}
}

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# The Executor SHALL NOT override a variable that the Job itself
//# sets with the same name, so that a job can opt out of a Host Service.

func TestJobVariablesWinOverHostServices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withHostServices(f)
	job := testJob()
	job.Variables = append(job.Variables, spec.Variable{Key: "GOPROXY", Value: "direct", Public: true})
	_, b, err := f.runBuild(context.Background(), job)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.GetAllVariables().Value("GOPROXY"); got != "direct" {
		t.Errorf("GOPROXY = %q, want the job's own value", got)
	}
	n := 0
	for _, v := range b.Variables {
		if v.Key == "GOPROXY" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GOPROXY appears %d times in the job's variables", n)
	}
	if got := b.GetAllVariables().Value("BUILDKIT_HOST"); got != "tcp://172.31.0.1:1234" {
		t.Errorf("BUILDKIT_HOST = %q; the other services should still be added", got)
	}
}

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# The Executor SHALL NOT set `GOPRIVATE`, because it would make
//# the Go toolchain bypass the Host's proxy for private modules.

// TestGOPRIVATEIsNeverSet has a resolver return GOPRIVATE and names it as an
// HTTP cache variable too: it still never reaches the Job.
func TestGOPRIVATEIsNeverSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.deps.Env = resolverFunc(func(*config.HostEntry) HostServiceEnv {
		return HostServiceEnv{Vars: map[string]string{
			"GOPRIVATE": "gitlab.example.com/*", "GOPROXY": "http://172.31.0.1:3000", "UNLISTED": "x",
		}, Services: []string{ServiceGoProxy}}
	})
	f.deps.Inventory = inventoryMap{"host-a": hostEntry()}
	f.opts = append(f.opts, WithHTTPCacheUpstreams([]config.HTTPCacheUpstream{{Name: "evil", EnvVar: "GOPRIVATE"}}))
	_, b, err := f.runBuild(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	vars := b.GetAllVariables()
	if v := vars.Get("GOPRIVATE"); v != "" {
		t.Errorf("GOPRIVATE = %q", v)
	}
	if v := vars.Get("UNLISTED"); v != "" {
		t.Errorf("a variable outside the closed list was added: UNLISTED=%q", v)
	}
	if v := vars.Get("GOPROXY"); v != "http://172.31.0.1:3000" {
		t.Errorf("GOPROXY = %q, want the resolver's", v)
	}
	for _, r := range f.tr.stages() {
		if strings.Contains(string(r.Stdin), "GOPRIVATE") {
			t.Fatal("a stage script mentions GOPRIVATE")
		}
	}
}

//= docs/requirements/02-executor.md#host-service-environment
//= type=test
//# The Executor SHALL write a line to the `flintlock_prepare`
//# section of the Job log naming the Host Services that were made available
//# to the Job.

func TestPrepareSectionNamesTheHostServices(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	withHostServices(f)
	e, trace, err := f.prepareOnly(context.Background(), testJob())
	if err != nil {
		t.Fatal(err)
	}
	defer e.Cleanup()
	log := trace.String()
	end := strings.Index(log, "section_end:")
	line := "Host services: buildkit, go_proxy, registry_mirror, http_cache"
	i := strings.Index(log, line)
	if i < 0 || end < i {
		t.Errorf("section lacks %q before its end:\n%s", line, log)
	}
}
