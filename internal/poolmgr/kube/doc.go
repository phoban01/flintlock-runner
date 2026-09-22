// Package kube is the Kubernetes pool backend: the second implementation of
// poolmgr.Client, for a fleet run from a cluster
// (docs/requirements/12-cluster-fleet.md#kube-pools).
//
// A Pool is a ReplicaSet of idle MicroVM pods in the Runner's namespace and a
// Lease is a claimed pod. The Kubernetes API does what battery does for the
// gRPC client: the ReplicaSet controller keeps a Pool at size, the scheduler
// places its pods on Virtual Nodes, and a claim is one update of a pod
// conditioned on its resource version, which takes the pod out of the
// ReplicaSet's selector so that a replacement is created at once (KF-044).
//
// Nothing above poolmgr.Client changes. The Declarer declares Pools through
// CreatePool and UpdatePool (KF-040), the Scheduler claims, heartbeats and
// releases through the Lease methods (KF-044 to KF-048), and the PoolTracker
// reads Subscribe and GetPool exactly as it reads battery's Events stream
// and GetPool: the backend turns a watch on the Pool's pods into the
// poolmgr.v1alpha1 events the Tracker already understands (KF-049). The
// sentinel errors are the battery client's, so exhaustion, an unknown Pool
// and an unreachable API server reach the Scheduler as they always have.
//
// Two things the interface does not carry are supplied on the side. A
// PoolSpec has no architecture and no Host selector, because battery takes a
// list of Host names instead, so the backend is given the Profiles and looks
// one up by the Profile label of the spec's template (KF-041). And ClaimVM
// has no Job, so the Job timeout that becomes the claimed pod's active
// deadline is read from the claim's context (WithJobTimeout) and falls back
// to the configured one (KF-044).
//
// The permissions all of this needs are the Role in deploy/runner/role.yaml
// (KF-110); permissions_test.go holds the code to it.
package kube
