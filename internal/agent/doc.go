// Package agent is the Exec Agent of a cluster fleet
// (docs/requirements/12-cluster-fleet.md#exec-agent): `flr agent`, which
// runs in the Host Agent on each Host and relays a Runner's exec requests
// to the flintlockd on that Host, after checking that the caller holds a
// Bound claim on the MicroVM, and reports the Host's readiness.
//
// The exec API is flintlock's own `microvmexec.services.api.v1alpha1`
// service, and the ServerInfo and GetMicroVM calls of its
// `microvm.services.api.v1alpha1` service, served over TLS on the Host's
// internal address (KF-172). That is what the Runner's exec Guest
// Transport already speaks, so the `agent-exec` transport is the exec
// transport pointed at the agent with a bearer token (KF-185 to KF-188).
//
// The pieces are:
//
//   - auth.go: every request authenticated with a TokenReview of its
//     bearer token (KF-173, KF-175).
//   - claims.go and dynclaims.go: every request that names a MicroVM
//     authorized against the claims (KF-174, KF-175), through the
//     ClaimLookup interface.
//   - server.go: the relay to flintlockd, whose response ends with
//     flintlockd's exit_code or with an error status and never with a clean
//     end that has no exit code (KF-176), and whose stream to flintlockd has
//     to be open within a deadline (KF-177).
//   - flintlockd.go: the connection to the local flintlockd, refused for any
//     endpoint that is not local (KF-171).
//   - tls.go: the serving certificate, which has to name the Host's
//     internal address (KF-172).
//   - node.go: the readiness check (KF-178) and the annotations on the
//     Host's Node: the Host Services (KF-179), the readiness and the
//     address of the exec API.
//   - drain.go: the drain guard (KF-181).
//   - identity.go: the check, at start, that the agent's identity names its
//     Host, which deploy/agent/admission-policy.yaml relies on (KF-180).
//
// # The claim resource is provisional
//
// battery's MicroVMClaim is not settled, so the claim check reads claims
// through ClaimLookup, and the one implementation here, DynamicClaims,
// reads a test definition of the resource: group
// `claims.test.flintlock-runner.dev`, version `v1alpha1`, resource
// `microvmclaims`, in internal/agent/testdata/crds/microvmclaims.yaml. Each
// fact the checks turn on is one field of it, named in
// ProvisionalClaimResource:
//
//   - phase: `status.phase`, one of Pending, Bound, Expired and Released
//     (KF-174 needs Bound; KF-181 counts Bound claims);
//   - MicroVM uid: `status.microVM.uid` (KF-174);
//   - Host: `status.host.nodeName`, the node name of the Host's Node
//     (KF-174, KF-181);
//   - expiry: `status.leaseExpiresAt`, an RFC 3339 time (KF-174);
//   - creator identity: the annotation
//     `claims.test.flintlock-runner.dev/creator`, the user name of the
//     identity that created the claim (KF-174).
//
// The creator is the open point KF-174 depends on. An annotation is not a
// trustworthy record of who created an object, because whoever may update
// the claim may write it; it is used here only because battery has not yet
// said how a claim records its creator. battery's resource has to record it
// in a way its creator cannot choose, for example a field an admission
// policy sets from the request's user on create and keeps immutable. Until
// it does, the check is as strong as the RBAC on the claims.
//
// Where battery's real resource plugs in: a ClaimResource with its names
// and field paths, where each fact is one field, passed to
// NewDynamicClaims (the configuration's `claims` section names the group,
// version and resource); or, where it is not, a ClaimLookup of its own,
// passed as Options.Claims. Nothing else in the agent names a field.
package agent
