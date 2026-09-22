// Package kubelet is the Pod Provider of a cluster fleet
// (docs/requirements/12-cluster-fleet.md): `flr kubelet`, a virtual kubelet
// that runs on each Host, registers the Host's Virtual Node and realises
// every pod bound to it as one MicroVM on the Host's own flintlockd.
//
// The pieces are:
//
//   - node.go: the Virtual Node, its capacity, labels, taint, Host Service
//     annotations and Ready condition (KF-010 to KF-017), and the
//     not-ready-reason contract with the Host Image (KF-016, HI-011).
//   - spec.go and provider.go: pods to MicroVMs and back, adoption on start
//     and lease expiry (KF-020 to KF-029, KF-032).
//   - exec.go: the kubelet API, served over mutual TLS, with the pod exec
//     endpoint relayed to MicroVMExec (KF-030, KF-031).
//   - authz.go: every kubelet API request authorized with a
//     SubjectAccessReview of the client certificate's identity (KF-130,
//     KF-131).
//   - identity.go: the check, at start, that the provider's own identity
//     names its Host, which the admission policy of
//     deploy/host-agent/admission-policy.yaml relies on (KF-133, KF-134).
//   - drain.go: the Virtual Node following its Host's Node through a drain,
//     held open by a guard pod while Jobs run (KF-090 to KF-094).
//   - flintlockd.go: the connection to the local flintlockd, refused for any
//     endpoint that is not local (KF-018).
//
// Run ties them together. Its permissions are deploy/host-agent/rbac.yaml
// (KF-111), and the tests run the provider under exactly that manifest
// against a real API server and the fake Host (KF-120).
package kubelet
