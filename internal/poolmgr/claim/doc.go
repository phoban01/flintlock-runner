// Package claim is the claim pool backend: an implementation of
// poolmgr.Client over battery-operator's Pool and MicroVMClaim resources
// (docs/requirements/12-cluster-fleet.md#battery-claims).
//
// A Pool is a battery-operator Pool resource, and a Lease is a MicroVMClaim.
// battery-operator's Client Library, pkg/claimclient, does the work on the
// claim: it creates the claim and its Secret, waits for the claim to bind,
// renews it, and deletes it. The backend maps the Lease calls onto it:
//
//   - ClaimVM makes one claim for the Pool and the configured Holder. It
//     returns once the claim is Bound, with the lease id, the MicroVM's uid
//     and the Host taken from the claim's status (KF-150, KF-151). The lease
//     id is the claim's name. A claim that waits because the Pool is
//     exhausted is deleted at once, and ClaimVM returns
//     poolmgr.ErrExhausted, which the Scheduler waits out (KF-155, SC-021).
//   - Heartbeat does not renew anything: the Client Library renews the claim
//     at the Pool's heartbeat interval (KF-152). Heartbeat reads the claim and
//     returns its lease expiry, or poolmgr.ErrNotFound once the claim is
//     Expired or gone (KF-154, SC-061).
//   - ReleaseVM deletes the claim, and a claim that is gone already counts
//     as released (KF-153).
//
// The Declarer declares Pools through CreatePool and UpdatePool, which
// write Pool resources, and the PoolTracker reads Subscribe and GetPool. A
// Pool resource carries battery's counts in its status, and a watch on the
// Pools turns a change in the available count into the poolmgr.v1alpha1
// events the Tracker already counts from. The sentinel errors are the
// battery client's, so exhaustion, an unknown Pool and an unreachable API
// server reach the Scheduler as they always have (KF-156).
//
// A PoolSpec names its Hosts, but a Pool resource selects Nodes by label. So
// the backend is given the Profiles, and makes the node selector from the
// architecture and the Host selector of the Profile the spec's template is
// labelled with.
package claim
