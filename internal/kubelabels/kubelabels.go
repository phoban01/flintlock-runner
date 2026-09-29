// Package kubelabels holds the label and annotation keys that the cluster
// fleet of docs/requirements/12-cluster-fleet.md is wired together with.
// They live in one small package because components that never import each
// other have to agree on them: the Executor (internal/executor) reads the
// Host Service annotations that the Host Agent (internal/hostservices)
// writes, and the Fleet Manifests repeat the same strings in YAML.
package kubelabels

// Prefix is the domain every key of this project lives under.
const Prefix = "gitlab-runner.flintlock.dev/"

// Host's Node annotations.
const (
	// AnnotationHostServicePrefix prefixes one annotation per enabled Host
	// Service on a Host's Node; the rest of the key is the service's name
	// and the value its `address:port`. The Host Agent publishes them
	// (KF-194), and the Executor of the claim design reads them (KF-189).
	AnnotationHostServicePrefix = "host-service." + Prefix
)

// The Host Service names: the keys of the Runner's host_services section,
// the names the Executor reads a Host's Node annotations by (KF-189), and
// the only names the Host Agent publishes under (KF-194), so that the two
// sides cannot disagree about a service without one of them refusing to
// start.
const (
	HostServiceBuildkit       = "buildkit"
	HostServiceGoProxy        = "go_proxy"
	HostServiceRegistryMirror = "registry_mirror"
	HostServiceHTTPCache      = "http_cache"
)

// HostServiceNames lists every Host Service name, sorted.
func HostServiceNames() []string {
	return []string{HostServiceBuildkit, HostServiceGoProxy, HostServiceHTTPCache, HostServiceRegistryMirror}
}

// HostServiceAnnotation is the annotation key on a Host's Node that carries
// the address of the named Host Service (KF-194).
func HostServiceAnnotation(service string) string {
	return AnnotationHostServicePrefix + service
}
