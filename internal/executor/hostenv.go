package executor

import (
	"slices"
	"sort"
	"strings"

	"gitlab.com/gitlab-org/gitlab-runner/common/spec"

	"github.com/phoban01/flintlock-runner/internal/config"
)

// Host Service names as they appear in the flintlock_prepare section
// (EX-064). They are the keys of the host_services configuration section.
const (
	ServiceBuildkit       = "buildkit"
	ServiceGoProxy        = "go_proxy"
	ServiceRegistryMirror = "registry_mirror"
	ServiceHTTPCache      = "http_cache"
)

// hostServiceEnv is the production HostServiceEnvResolver: a pure function
// of the host_services configuration and one Inventory entry.
type hostServiceEnv struct {
	cfg config.HostServices
}

// NewHostServiceEnv returns the resolver for the host_services section.
func NewHostServiceEnv(cfg config.HostServices) HostServiceEnvResolver {
	return hostServiceEnv{cfg: cfg}
}

// enabled reads a Service's enabled flag, which defaults to true.
func enabled(s config.Service) bool { return s.Enabled == nil || *s.Enabled }

//= docs/requirements/02-executor.md#host-service-environment
//# When the Placement of a Job's MicroVM is known, the Executor
//# SHALL add to the Job's environment the variables that point at the Host
//# Services of that Host: `BUILDKIT_HOST`, `GOPROXY`, `GOFLAGS` with
//# `-modcacherw`, the registry mirror address as `CI_REGISTRY_MIRROR`, and
//# one variable per configured HTTP cache upstream under the name given in
//# the configuration.

//= docs/requirements/02-executor.md#host-service-environment
//# When the Go module proxy is configured with private module
//# patterns, the Executor SHALL set `GONOSUMDB` to those patterns and
//# `GONOPROXY` to the empty string so that the Go toolchain fetches private
//# modules through the Host's proxy and does not consult the public checksum
//# database for them.

//= docs/requirements/02-executor.md#host-service-environment
//# Where a Host Service is disabled or the Host's Inventory entry
//# lacks its address, the Executor SHALL omit that service's variables rather
//# than point them at an unreachable address.

// Resolve implements HostServiceEnvResolver. A service contributes its
// variables only when it is enabled in the configuration and the Host's
// Inventory entry carries its address; otherwise it is left out entirely,
// so a Job never sees a variable pointing at a port nobody listens on.
func (r hostServiceEnv) Resolve(host *config.HostEntry) HostServiceEnv {
	out := HostServiceEnv{Vars: map[string]string{}}
	if host == nil {
		return out
	}
	addrs := host.Services

	if enabled(r.cfg.Buildkit.Service) && addrs.Buildkit != "" {
		out.Vars["BUILDKIT_HOST"] = addrs.Buildkit
		out.Services = append(out.Services, ServiceBuildkit)
	}
	if enabled(r.cfg.GoProxy.Service) && addrs.GoProxy != "" {
		out.Vars["GOPROXY"] = addrs.GoProxy
		out.Vars["GOFLAGS"] = "-modcacherw"
		if len(r.cfg.GoProxy.PrivatePatterns) > 0 {
			out.Vars["GONOSUMDB"] = strings.Join(r.cfg.GoProxy.PrivatePatterns, ",")
			out.Vars["GONOPROXY"] = ""
		}
		out.Services = append(out.Services, ServiceGoProxy)
	}
	if enabled(r.cfg.RegistryMirror.Service) && addrs.RegistryMirror != "" {
		out.Vars["CI_REGISTRY_MIRROR"] = addrs.RegistryMirror
		out.Services = append(out.Services, ServiceRegistryMirror)
	}
	if enabled(r.cfg.HTTPCache.Service) {
		any := false
		for _, u := range r.cfg.HTTPCache.Upstreams {
			if u.EnvVar == "" {
				continue
			}
			if url := addrs.HTTPCache[u.Name]; url != "" {
				out.Vars[u.EnvVar] = url
				any = true
			}
		}
		if any {
			out.Services = append(out.Services, ServiceHTTPCache)
		}
	}
	return out
}

// allowedVarNames is the closed set of names the Executor accepts from a
// resolver: the fixed Host Service names plus the configured HTTP cache
// variables. Anything else is dropped, GOPRIVATE included (EX-066).
func (e *executor) allowedVarNames() map[string]bool {
	allowed := make(map[string]bool, len(HostServiceVarNames))
	for _, n := range HostServiceVarNames {
		allowed[n] = true
	}
	for n := range e.p.httpCacheVars {
		allowed[n] = true
	}
	// Whatever the configuration says, GOPRIVATE is never one of them.
	delete(allowed, "GOPRIVATE")
	return allowed
}

//= docs/requirements/02-executor.md#host-service-environment
//# The Executor SHALL NOT set `GOPRIVATE`, because it would make
//# the Go toolchain bypass the Host's proxy for private modules.

//= docs/requirements/02-executor.md#host-service-environment
//# The Executor SHALL NOT override a variable that the Job itself
//# sets with the same name, so that a job can opt out of a Host Service.

//= docs/requirements/02-executor.md#host-service-environment
//# The Executor SHALL add the Host Service variables before the
//# `prepare_script` Stage so that every Stage, including `get_sources`, sees
//# them.

//= docs/requirements/02-executor.md#host-service-environment
//# The Executor SHALL write a line to the `flintlock_prepare`
//# section of the Job log naming the Host Services that were made available
//# to the Job.

// addHostServiceEnv adds the Host Service variables of the Placement's Host
// to the Job's variables. It runs in Prepare, which the Build finishes
// before it generates the first Stage script, so prepare_script,
// get_sources and every later Stage export them. A variable the Job already
// has, from its payload, keeps the Job's value; a name outside the closed
// list is dropped, which is what keeps GOPRIVATE out whatever a resolver
// returns. The variables travel inside the Stage scripts like every other
// Job variable (EX-027).
func (e *executor) addHostServiceEnv(sec *prepareSection) {
	var env HostServiceEnv
	if e.p.deps.Env != nil && e.p.deps.Inventory != nil {
		host, _ := e.p.deps.Inventory.Host(e.handle.Allocation().Placement.Host)
		env = e.p.deps.Env.Resolve(host)
	}

	allowed := e.allowedVarNames()
	jobSets := make(map[string]bool, len(e.Build.Variables))
	for _, v := range e.Build.Variables {
		jobSets[v.Key] = true
	}
	names := make([]string, 0, len(env.Vars))
	for k := range env.Vars {
		names = append(names, k)
	}
	sort.Strings(names)
	added := false
	for _, k := range names {
		if !allowed[k] || jobSets[k] {
			continue
		}
		e.Build.Variables = append(e.Build.Variables, spec.Variable{Key: k, Value: env.Vars[k], Public: true})
		added = true
	}
	if added {
		e.Build.RefreshAllVariables()
	}

	services := slices.Clone(env.Services)
	sec.report.Services = services
	sec.printf("%s", servicesLine(services))
}
