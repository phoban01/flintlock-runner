// Package kubelabels holds the label, annotation and taint keys that the
// cluster fleet of docs/requirements/12-cluster-fleet.md is wired together
// with. They live in one small package because two components that never
// import each other have to agree on them: the Pod Provider (internal/kubelet)
// reads what the Kubernetes pool backend (internal/poolmgr/kube) writes, and
// the Fleet Manifests repeat the same strings in YAML.
package kubelabels

import (
	"fmt"
	"time"
)

// Prefix is the domain every key of this project lives under.
const Prefix = "gitlab-runner.flintlock.dev/"

// Node labels and taints.
const (
	// LabelHost marks a Host's own Node (HI-060).
	LabelHost = Prefix + "host"
	// LabelVirtualNode is set to "true" on every Virtual Node (KF-013) and
	// is what a Pool pod's node selector asks for (KF-041).
	LabelVirtualNode = Prefix + "virtual-node"
	// LabelHostNode carries the name of the Host's Node on its Virtual Node
	// (KF-013).
	LabelHostNode = Prefix + "host-node"
	// TaintMicroVM is the key of the Virtual Node's taint, with the value
	// "true" and the effect NoSchedule (KF-014).
	TaintMicroVM = Prefix + "microvm"
	// TaintHost is the key of the taint a Host's own Node joins with
	// (KF-002); the drain guard pod tolerates it.
	TaintHost = Prefix + "host"
)

// Pod labels and annotations.
const (
	// LabelState is a Pool pod's claim state (KF-043, KF-044).
	LabelState = Prefix + "state"
	// StateIdle is the state of a warm pod nobody has claimed.
	StateIdle = "idle"
	// StateClaimed is the state of a pod a Job runs in.
	StateClaimed = "claimed"

	// AnnotationLease is the time of the claim or of the last heartbeat
	// (KF-044, KF-047), written with FormatLease. The Pod Provider deletes a
	// claimed pod whose lease is older than the lease duration (KF-032).
	AnnotationLease = Prefix + "lease"

	// AnnotationKernelImage, AnnotationKernelCmdline and AnnotationHypervisor
	// describe the MicroVM a pod stands for (KF-020). The kernel image is
	// required. The command line is written as a kernel command line,
	// space-separated `key=value` or bare `key` words.
	AnnotationKernelImage   = Prefix + "kernel-image"
	AnnotationKernelCmdline = Prefix + "kernel-cmdline"
	AnnotationHypervisor    = Prefix + "hypervisor"
	// AnnotationKernelFilename optionally names the kernel binary inside the
	// kernel image, as a Profile's kernel filename does.
	AnnotationKernelFilename = Prefix + "kernel-filename"
	// AnnotationCloudInitConfigMap names a ConfigMap in the pod's namespace
	// whose CloudInitUserDataKey entry is the guest's cloud-init user data
	// (KF-023).
	AnnotationCloudInitConfigMap = Prefix + "cloud-init-configmap"
	// CloudInitUserDataKey is the ConfigMap key read for KF-023.
	CloudInitUserDataKey = "user-data"
)

// Virtual Node annotations.
const (
	// AnnotationHostServicePrefix prefixes one annotation per enabled Host
	// Service on a Virtual Node; the rest of the key is the service's name
	// and the value its `address:port` (KF-017, KF-062).
	AnnotationHostServicePrefix = "host-service." + Prefix
)

// The Host Service names: the keys of the Runner's host_services section,
// the names the Executor reads Virtual Node annotations by (KF-062), and the
// only names the Pod Provider publishes under (KF-017), so that the two
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

// HostServiceAnnotation is the Virtual Node annotation key that carries the
// address of the named Host Service (KF-017).
func HostServiceAnnotation(service string) string {
	return AnnotationHostServicePrefix + service
}

// FormatLease renders a lease time for AnnotationLease: RFC 3339 in UTC with
// nanoseconds, so that two heartbeats in the same second still differ.
func FormatLease(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseLease reads an AnnotationLease value written by FormatLease.
func ParseLease(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("kubelabels: lease annotation %q is not an RFC 3339 time: %w", value, err)
	}
	return t, nil
}
