# Proposal: Pool and MicroVMClaim resources for battery

**Status:** draft, for discussion with battery upstream (liquidmetal-dev/battery#46)
**From:** flintlock-runner
**Date:** 2026-09-22

## Summary

battery stays the scheduler of MicroVMs and the authority over Pools. Its
Kubernetes surface is two namespaced resources:

- **`Pool`**: what battery keeps warm. It is `PoolSpec` as a resource.
- **`MicroVMClaim`**: one client's hold on one warm MicroVM from a Pool. It
  is `ClaimVM`, `Heartbeat` and `ReleaseVM` as a resource.

There is deliberately no `MicroVM` resource. VMs stay internal to battery,
as they are today, and the counts on `Pool.status` and the claims give the
visibility a client needs. A `MicroVM` resource can be added later, for
debugging or snapshots, without changing the claim contract.

The naming follows Kubernetes' own: a PersistentVolumeClaim names what it
gets (a volume) and where it comes from (a storage class). A `MicroVMClaim`
gets a MicroVM from a Pool. `PoolResourceClaim` was considered and rejected,
because it says little about what you receive and is easily confused with
DRA's `resource.k8s.io/ResourceClaim`.

## How flintlock-runner uses it

flintlock-runner is a GitLab runner that runs each CI job in a warm flintlock
MicroVM. With these resources:

1. The Runner declares one `Pool` per job profile.
2. For each job it creates a `MicroVMClaim` and waits for `Bound`.
3. It renews the claim while the job runs.
4. It runs the job's commands through a per-Host **Exec Agent**, the
   address in the claim's status. The agent relays to `MicroVMExec` on the
   local flintlockd, which listens on loopback only. The agent runs a
   command only for the identity that holds a Bound, unexpired claim on
   that MicroVM on that Host.
5. It deletes the claim when the job ends.

The Exec Agent's per-claim check is why `spec.serviceAccountName` matters
(see [Choices](#choices)).

## Pool

```yaml
apiVersion: battery.liquidmetal.dev/v1alpha1   # group is a suggestion
kind: Pool
metadata:
  name: builders
  namespace: ci-runners
spec:
  size: 4                                  # PoolSpec.size
  template:                                # PoolSpec.microvm_template (flintlock MicroVMSpec)
    provider: firecracker
    vcpu: 2
    memoryInMb: 4096
    kernel:
      image: ghcr.io/example/kernel:6.6
      cmdline: {console: ttyS0, reboot: k}
      filename: boot/vmlinux
    rootVolume:
      containerSource: ghcr.io/example/ubuntu-ci:24.04
    metadata:                              # cloud-init
      user-data: |
        #cloud-config
        ...
    labels:
      gitlab-runner.flintlock.dev/profile: builders
  placement:                               # replaces PoolSpec.flintlock_hosts
    nodeSelector:                          # Hosts come from battery's inventory
      gitlab-runner.flintlock.dev/host: "true"
      kubernetes.io/arch: amd64
    strategy: LeastLoaded                  # LeastLoaded | RoundRobin
  replenishment:                           # PoolSpec.replenishment_strategy
    type: ImmediateOnLease                 # ImmediateOnLease | MinSizeThreshold | ReplaceOnDelete
    # minSize: 2                           # MinSizeThreshold only
  hooks:
    create: ["systemctl is-active --quiet flr-guest-ready"]   # PoolSpec.create_commands
    preLease: []                                              # PoolSpec.pre_lease_commands
    failurePolicy: DeleteAndReplace        # DeleteAndReplace | Quarantine
  lease:
    heartbeatInterval: 10s                 # PoolSpec.heartbeat_interval
    expiryThreshold: 30s                   # PoolSpec.heartbeat_expiry_threshold
status:                                    # written by battery only
  observedGeneration: 3
  available: 3                             # PoolStatus.available_count
  leased: 1                                # PoolStatus.leased_count
  provisioning: 0                          # PoolStatus.provisioning_count
  quarantined: 0                           # PoolStatus.quarantined_count
  conditions:
    - {type: Ready, status: "True", reason: AtTargetSize}
    - {type: Exhausted, status: "False", reason: VMsAvailable}
```

## MicroVMClaim

```yaml
apiVersion: battery.liquidmetal.dev/v1alpha1
kind: MicroVMClaim
metadata:
  name: job-8812345-7f3a                   # named by the client; this is the lease id
  namespace: ci-runners
  finalizers:
    - battery.liquidmetal.dev/release      # battery deletes the VM, then removes this
spec:
  poolRef:
    name: builders                         # ClaimVMRequest.pool; same namespace
  renewTime: "2026-09-22T17:04:10.123456Z" # Heartbeat: the holder patches this
  serviceAccountName: flintlock-runner
    # The ServiceAccount, in this namespace, allowed to use the MicroVM, as a
    # Pod's serviceAccountName. Immutable. See Choices.
status:                                    # written by battery only
  phase: Bound                             # Pending | Bound | Expired | Released
  microVM:
    uid: 01JB7ZK4QH3M8V6R2N5T9X0C1D        # ClaimVMResponse.vm_uid
  host:                                    # ClaimVMResponse.host (HostInfo)
    nodeName: ip-10-0-1-23.eu-west-1.compute.internal
    agentAddress: 10.0.1.23:10270          # new: where the client reaches the Exec Agent
  boundTime: "2026-09-22T17:03:58Z"
  leaseExpiresAt: "2026-09-22T17:04:40Z"   # HeartbeatResponse.expires_at
  conditions:
    - type: Bound
      status: "True"
      reason: Bound
      lastTransitionTime: "2026-09-22T17:03:58Z"
```

The same claim while the Pool has nothing to give. The client waits on it:

```yaml
status:
  phase: Pending
  conditions:
    - type: Bound
      status: "False"
      reason: PoolExhausted                # or PoolNotFound / NoEligibleHost
      message: "pool ci-runners/builders has 0 available of 4"
```

And after the holder stopped renewing:

```yaml
status:
  phase: Expired
  microVM: {uid: 01JB7ZK4QH3M8V6R2N5T9X0C1D}
  host: {nodeName: ip-10-0-1-23.eu-west-1.compute.internal, agentAddress: 10.0.1.23:10270}
  leaseExpiresAt: "2026-09-22T17:04:40Z"
  conditions:
    - {type: Bound, status: "False", reason: LeaseExpired}
```

## Lifecycle

| Step | Client | battery |
|---|---|---|
| Claim | creates a `MicroVMClaim` naming the Pool | binds a warm VM, fills `status.microVM` and `status.host`, sets `phase: Bound`, and replenishes the Pool as its strategy says |
| No VM to give | waits while `Bound` is False with `PoolExhausted`; gives up by deleting the claim | keeps the claim `Pending` and binds it as soon as a VM is available |
| Renew | patches `spec.renewTime` every `heartbeatInterval` | moves `status.leaseExpiresAt` to `renewTime + expiryThreshold` |
| Lease lapses | sees `phase: Expired` and treats the lease as lost | deletes the VM and sets `phase: Expired` |
| Release | deletes the claim | its finalizer deletes the VM, then lets the claim go |

## Mapping to battery's gRPC API

| gRPC today | Resource |
|---|---|
| `PoolAdmin.CreatePool` / `UpdatePool(PoolSpec)` | create or update a `Pool` |
| `PoolSpec.flintlock_hosts` | `Pool.spec.placement.nodeSelector` over battery's inventory |
| `PoolStatus` counts | `Pool.status` |
| `Lease.ClaimVM(pool)` | create a `MicroVMClaim` with `spec.poolRef` |
| `ClaimVMResponse.lease_id` | the claim's name |
| `ClaimVMResponse.vm_uid`, `.host` | `status.microVM.uid`, `status.host` |
| `Lease.Heartbeat(lease_id)` returns `expires_at` | patch `spec.renewTime`, read `status.leaseExpiresAt` |
| `Lease.ReleaseVM(lease_id)` | delete the claim |
| `Lease.ListLeases` | list `MicroVMClaim`s |
| `Events.Subscribe` | watch `Pool` and `MicroVMClaim` |

## Choices

### The claim names the ServiceAccount that may use its MicroVM

A claim names a ServiceAccount in its own namespace, as a Pod does. The Exec
Agent runs a command in the claim's MicroVM only for a caller whose token
reviews as `system:serviceaccount:<claim's namespace>:<spec.serviceAccountName>`.

No admission policy or webhook is needed for this to be safe, because
naming an account grants nothing unless you can also get a token for it:

- **A claim naming someone else's account** gets a MicroVM that only that
  account can use. Its creator gains nothing and has wasted a VM.
- **Changing the account on an existing claim** is the one real attack: it
  would hand another client's MicroVM to a new account. The CRD schema
  forbids it, and the API server enforces that itself:

```yaml
serviceAccountName:
  type: string
  x-kubernetes-validations:
    - rule: self == oldSelf
      message: spec.serviceAccountName is immutable
```

This replaces an earlier idea of recording the claim's creator, which would
have needed an admission policy to set the creator and keep it honest.

### Each claim has its own token, which dies with the claim

The client uses a separate token for each claim, and Kubernetes itself signs
it, so battery needs no keys:

1. The client creates the `MicroVMClaim`.
2. It creates an empty Secret, `<claim name>-exec`, whose `ownerReference` is
   the claim. The Secret holds no data; it only anchors the token.
3. It asks for a token for its ServiceAccount with `TokenRequest`:
   - `boundObjectRef` is that Secret;
   - `audiences` is the Exec Agent, for example `battery.liquidmetal.dev/exec`;
   - `expirationSeconds` is about the lease. The minimum is ten minutes, and a
     longer job asks for a fresh token.
4. It calls the Exec Agent with that token for that claim's MicroVM and
   nothing else.

Releasing the claim deletes it. The garbage collector then deletes the
Secret, and a token bound to a deleted object no longer passes a
TokenReview. The token also expires on its own.

For each exec the Exec Agent checks, in order:

1. The token passes a TokenReview with the agent's audience.
2. It belongs to the claim's ServiceAccount (`spec.serviceAccountName`, in
   the claim's namespace). The binding alone would not be enough, because
   anyone who may request tokens for any ServiceAccount in the namespace
   could bind one to the claim's Secret.
3. It is bound to this claim's Secret: the name is the claim's name with
   `-exec`, and the UID matches. The API server has verified the token's
   signature in step 1, so the agent reads the binding from the token's own
   claims (`kubernetes.io.secret.name`, `.uid`). If TokenReview reports the
   bound object directly, that is simpler. This needs confirming.
4. The claim is Bound, has not expired, and names this MicroVM and this
   Host.

What this buys:

- A leaked token opens one MicroVM for one lease.
- Clients that share a ServiceAccount can't use each other's claims.
- A token is useless against the API server, and a general-purpose API token
  is useless against the agent.

What it costs the client is permission to create Secrets and
`serviceaccounts/token` for its own account, both ordinary. The garbage
collector takes a few seconds after a release, so for that time the token
still passes step 3. Step 4 fails the moment the claim is released, so the
token opens nothing in that gap.

What it does not do is protect against a compromised client. A client holds
every token it has requested, just as it holds its ServiceAccount's token
today.

### Renewal lives in `spec`, status belongs to battery

The client renews by patching `spec.renewTime`, as a `coordination.k8s.io`
Lease holder does. battery alone writes `status`. So the client needs no
permission on the status subresource, and the two writers never collide.

### Hosts come from the inventory, filtered by `placement`

`flintlock_hosts` goes away. battery keeps an inventory of registered Hosts
and a Pool narrows it with a `nodeSelector`, so adding a Host to the fleet
needs no change to any Pool. How Hosts get into the inventory is a separate
question (see [Open questions](#open-questions)).

### `status.host.agentAddress` is new

A client reaches the Exec Agent on the Host, not flintlockd, which listens
on loopback only. battery learns the agent's address when the Host
registers, and passes it on in the claim.

### Exhaustion is a reason on a Pending claim

A claim that cannot bind stays `Pending` with `Bound=False,
reason=PoolExhausted`. The client waits on that, which matches how a gRPC
client waits on `ErrExhausted` today. The client stops waiting by deleting
the claim.

### Deleting the claim releases the VM

The `battery.liquidmetal.dev/release` finalizer keeps the claim until
battery has deleted its VM. A released MicroVM is never reused.

## Open questions

1. **Group and version.** `battery.liquidmetal.dev/v1alpha1` is a
   placeholder.
2. **Who creates Pools?** The client, from its own configuration, as
   flintlock-runner declares Pools over gRPC today, or an operator?
3. **gRPC fast path.** Does gRPC stay alongside the resources, with the
   resources as the record? Or do the resources replace it?
4. **Inventory registration.** Where does the controller that registers
   Hosts live, in battery as its node component or in the client? It needs:
   - the Host's readiness. flintlock-runner's Exec Agent publishes this on
     the Host's Node as annotations: `gitlab-runner.flintlock.dev/exec-agent-ready`,
     `-reason`, `-message` and `-address`;
   - the Node's schedulability, so a cordoned Host takes no new claims.
5. **Drain.** A cordoned Host should get no new claims, and should hold an
   eviction drain open while claims are Bound on it. flintlock-runner's
   Exec Agent does the second part with a guard pod. Should battery do the
   first part?
6. **Snapshots.** A later `Pool.spec.template.source` naming a snapshot to
   restore from would make warm pools cheaper to hold. Nothing above
   depends on it.
7. **Who creates the token's Secret.** Above, the client does. battery
   could create it when it binds the claim, which would put the convention
   in one place, but it would then need permission to create Secrets in
   every client namespace.

## What flintlock-runner already has against this shape

- **The Exec Agent** (merged). It reads claims through an interface. Today
  it uses a provisional test definition with the fields `status.phase`,
  `status.microVM.uid`, `status.host.nodeName` and `status.leaseExpiresAt`,
  plus a creator annotation. This proposal replaces the annotation with the
  four checks above: an audience on the TokenReview, the ServiceAccount
  against `spec.serviceAccountName`, the token's binding to the claim's
  Secret, and the claim itself.
- **The claim backend** on the Runner side: create the claim, its Secret and
  its token, wait for `Bound`, renew, delete. It is not built yet, and waits
  on this shape.
