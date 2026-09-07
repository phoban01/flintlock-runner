# Architecture

This document is the non-normative companion to the requirements in
`docs/requirements/`. It explains what the pieces are, why they are arranged
this way, and what was found about the upstream projects when the design was
made (September 2026).

## What it is

`flintlock-runner` is a GitLab CI runner that executes every job in its own
Firecracker or Cloud Hypervisor microVM on a fleet of bare-metal EC2 hosts.
It is built on three liquidmetal components:

- [flintlock](https://github.com/liquidmetal-dev/flintlock) runs microVMs on
  each host.
- [brigade](https://github.com/liquidmetal-dev/brigade), the Orchestrator,
  fronts the fleet: it speaks flintlock's `MicroVM` gRPC API unchanged and
  decides which host each microVM lands on. The runner never talks to a
  host to create or delete a microVM.
- [battery](https://github.com/liquidmetal-dev/battery), the Pool Manager,
  keeps pools of pre-booted microVMs and leases them out. Every job normally
  starts in a leased warm microVM; the runner declares the pools from its
  own profile configuration.

The runner itself is a single Go binary built from the gitlab-runner Go
packages, with a scheduling component inserted where the stock runner would
talk to a container runtime.

```
                 ┌──────────────────────── Control Node ─────────────────────────┐
                 │  flintlock-runner                                              │
   GitLab  ◄─────┼─►  gitlab-runner run loop (commands.RunCommand)                │
  jobs/request   │        │ Acquire / Release                                     │
  trace, artifacts        ▼                                                       │
                 │   Executor "flintlock" ──► Scheduler ──► capacity, profiles,   │
                 │        │                       │        leases, overflow, GC   │
                 │        │ Guest Transport        │                              │
                 │        │ (MicroVMExec)          ├──► battery poolmgrd          │
                 │        │                        │      ClaimVM / Heartbeat /   │
                 │        │                        │      ReleaseVM  (warm path)  │
                 │        │                        └──► brigade                   │
                 │        │                               CreateMicroVM           │
                 │        │                               (overflow path, GC)     │
                 └────────┼─────────────────────────────────┬─────────────────────┘
                          │ exec over vsock, GetMicroVM      │ MicroVM gRPC (drop-in)
          ┌───────────────▼─────────────┐    ┌───────────────▼─────────────┐
          │ Host A (m7g.metal)          │    │ Host B (m7g.metal)          │
          │  flintlockd :9090           │    │  flintlockd :9090           │
          │  brigade node :9091 ◄──gossip──► brigade node :9091           │
          │  poolmgr-hostagent          │    │  poolmgr-hostagent          │
          │  containerd + thinpool      │    │  containerd + thinpool      │
          │  bridge + dnsmasq + NAT     │    │  bridge + dnsmasq + NAT     │
          │   ┌────┐ ┌────┐ ┌────┐      │    │   ┌────┐ ┌────┐             │
          │   │job │ │job │ │warm│      │    │   │job │ │warm│             │
          │   └────┘ └────┘ └────┘      │    │   └────┘ └────┘             │
          └─────────────────────────────┘    └─────────────────────────────┘
```

## Life of a job

1. The run loop calls `Executor.Acquire`. The Scheduler grants a reservation
   only if a slot is free *and* either a pool has a warm microVM or overflow
   headroom remains. Otherwise no job is requested from GitLab.
2. A job arrives. Its `image:` keyword names a Profile (kernel, rootfs,
   sizing, transport, pool). Unknown image, no default profile: the job
   fails immediately without touching a pool.
3. The Scheduler calls battery `ClaimVM` on the profile's pool. On success it
   holds a lease and heartbeats it for the life of the job.
4. If the pool is exhausted and the profile's exhaustion policy is
   `overflow`, the Scheduler sends `CreateMicroVM` to brigade with the
   profile's architecture and host selector as `brigade.scheduling/` labels.
   Policy `wait` retries the claim until the allocation timeout instead.
5. Either way the Scheduler resolves *placement*: which host runs the
   microVM. Neither brigade nor battery returns that, so it fans out
   `GetMicroVM` across the candidate hosts (the pool's hosts, or the
   inventory filtered by the profile) and caches the answer.
6. The Executor waits for the guest to answer over the Guest Transport, then
   streams each generated bash stage script into the guest via flintlock's
   `MicroVMExec.ExecCommand` on that host, with the script on stdin.
7. On completion the lease is released (battery deletes and replenishes) or
   the overflow microVM is deleted through brigade. The slot is returned.

## Components

**Runner loop.** The stock gitlab-runner run loop (`commands.NewRunCommand`)
is reused as a library. It handles long polling, trace streaming, artifact
upload, cancellation, graceful shutdown and token rotation. The project
supplies only a provider registry containing the flintlock executor. This
is deliberate: the loop calls `ExecutorProvider.Acquire` *before* it asks
GitLab for a job, which is exactly the hook a scheduler needs to gate job
intake on real capacity.

**Executor.** Implements gitlab-runner's `ExecutorProvider` and `Executor`
interfaces. `Acquire` asks the Scheduler for a reservation; `Prepare` asks
it for a microVM matching the job's profile and waits for the guest to
answer; `Run` sends each stage script into the guest; `Cleanup` hands the
microVM back.

**Scheduler.** The component the design exists to insert. It owns:

- *Capacity*: how many jobs may be requested from GitLab right now,
  computed from warm pool availability plus overflow headroom.
- *Profile resolution*: `image:` names a profile, never a container image.
- *Allocation*: claim from the profile's pool first; overflow through
  brigade only when the pool is exhausted and the profile allows it.
- *Placement resolution*: learn which host got the microVM, because the
  exec transport talks to that host directly.
- *Pool declaration*: create or update one battery pool per profile at
  startup and on reload, so profiles are the single source of truth.
- *Lifecycle*: lease heartbeats, deletion of overflow microVMs through
  brigade, garbage collection of orphans by label, host and orchestrator
  health probing.

It never picks a host. brigade places overflow microVMs; battery places warm
ones across the hosts eligible for the pool.

**Guest Transport.** Default is flintlock's `MicroVMExec.ExecCommand`
streaming RPC, served by `flintlockd` over the microVM's vsock via the
guest-agent. It supports stdin streaming, cwd, env, user, a timeout and
returns an exit code, which is everything a stage needs. An SSH transport
(via flintlock's `MicroVMSSHProxy` or direct TCP) is specified as an
alternative for images without the guest agent.

**Fleet Controller.** `flintlock-runner fleet {provision,verify,drain,
teardown,emit-userdata}`. Discovers `*.metal` instances by tag, provisions
them over SSM Run Command with `flintlock-provision all`, sets up a NAT'd
guest bridge with dnsmasq, installs a brigade node and the battery host
agent on every host and battery's daemon on the control node, pre-pulls
images, writes an inventory and generates the runner config. No part of the
liquidmetal ecosystem knows about EC2, so this layer is entirely ours.

## Why these choices

**Libraries, not the binary, not a custom-executor driver.** Three options
were weighed:

| Option | Per-job image | Pre-request capacity gating | Cost |
|---|---|---|---|
| Library-based executor (chosen) | yes | yes, at `Acquire` | ~35 MB binary, pin to a commit |
| Custom-executor driver | yes | no (only `limit`/`concurrent`) | stock binary supervises |
| Fleeting plugin | no (static per `[[runners]]`) | via taskscaler, before job known | least code |

The fleeting/taskscaler path is GitLab's own autoscaling abstraction, but
`Increase(n)` receives no job context, so per-job images need one runner
block per image, and the runner side of it lives under `internal/` in
gitlab-runner. The custom executor gives per-job images cheaply but has no
hook before the job request, so it cannot avoid accepting jobs it cannot
start. The library route gives both.

**Reuse `RunCommand` instead of writing our own poll loop.** Owning the loop
would buy nothing: GitLab assigns a job the moment it is requested, so no
loop design can look at a job before committing to it. The `Acquire` hook
already lets us decline to request. The cost is that the runner reads a
`RunnerConfig` structure; we generate it from our own YAML.

**brigade as the only control-plane endpoint.** The runner has exactly one
place to create, list and delete microVMs, and placement policy lives in
one component that every consumer of the fleet shares. The runner's host
inventory exists only for the two things brigade does not proxy: the exec
transport and placement lookup.

**battery as the only source of warm microVMs, with overflow through
brigade.** Warm pools give sub-second job start; overflow keeps a burst
larger than a pool from stalling on replenishment. Both paths produce a
microVM from the same specification builder, so a job cannot tell them
apart. Pools are declared from profiles so that adding a profile is one
edit.

**Exec over vsock as the default transport.** EC2 does not deliver frames to
MAC addresses it has not assigned, so microVMs on a host bridge are not
reachable from the VPC. Guests live on a host-local NAT'd subnet with
outbound-only access and the runner reaches them through `flintlockd` on the
host. No sshd or guest network configuration is required in job images
beyond the guest agent.

**One job per microVM, always destroyed.** No reuse, no snapshots restored
across jobs. Warm pools provide the latency win instead.

## The upstream pieces, as found

- **flintlock** v0.12.1 (2026-09-03). gRPC `MicroVM` service:
  Create/Delete/Get/List/ListStream and, on `main` only, `ServerInfo`. Guest
  agent APIs `MicroVMExec` and `MicroVMSSHProxy` are opt-in flags on
  `flintlockd` and need `allow_guest_agent` on the spec. Auth is a basic
  token in the `authorization` header; TLS and mTLS supported.
  `flintlock-provision` (the Go replacement for `provision.sh`) merged
  2026-09-05 and is not yet in a tagged release.
- **brigade** v0.2.0 (2026-07-24). Elixir/OTP, not Go. Runs on every host,
  meshes with distributed Erlang, exposes flintlock's `MicroVM` service
  verbatim on :9091 and places each create on a host via a least-loaded
  strategy with `brigade.scheduling/` label constraints. Protos pinned to
  flintlock v0.11.0, so `cpu_config` and `ServerInfo` do not pass through
  it. Does not proxy exec or SSH proxy. Host capacity is static config, not
  probed. Only "topology A" (a node per host) is implemented.
- **battery** (created 2026-09-05, no releases). Go, module
  `github.com/liquidmetal-dev/battery`. Protos for `PoolAdmin`, `Lease`
  (`ClaimVM`/`Heartbeat`/`ReleaseVM`), `Events` and a per-host `Hostagent`
  are defined; the daemons are stubs. A released VM is deleted and replaced,
  never reused. Hosts are a static JSON list and the host agent is reached
  through each VM's vsock path, so battery has to be pointed at physical
  hosts rather than at brigade. TLS only, no basic-auth token support in
  its client yet.
- **gitlab-runner** 19.4.0 (HEAD). Importable via pseudo-version only
  (`go get gitlab.com/gitlab-org/gitlab-runner@<commit>`); `go 1.26`.
  `common.JobResponse` is gone in favour of `common/spec.Job`; executor
  registration is now an explicit provider registry passed to
  `commands.NewRunCommand`. Registration tokens still exist but are disabled
  by default since 17.0; the supported path is a `glrt-` token created in
  the UI or via `POST /api/v4/user/runners`.

None of these has been integrated with GitLab before; the closest prior art
is Collabora's Rust `gitlab-runner` crate (raw REST) and a handful of
Firecracker projects that run the stock runner binary inside the VM.

## Delivery order

brigade and battery are load-bearing, so the runner is developed against
them from the start. battery's daemon does not exist yet, which sets the
order:

1. **Clients and fakes.** flintlock, brigade and battery gRPC clients with a
   conforming in-process fake for each, driven by the requirements in
   `04-pool-manager.md` and `05-orchestrator.md`. The fakes are what the
   Scheduler's tests run against.
2. **Scheduler and Executor** against the fakes, then against a real
   flintlock host and a real brigade cluster with the battery fake standing
   in for the pool manager.
3. **Fleet Controller** for EC2: provisioning, brigade and battery agent
   installation, verification.
4. **Real battery** as soon as its lease and reconciliation logic lands
   upstream; contribute there where the runner needs something (a host
   field on `ClaimVMResponse` would remove the placement fan-out).
5. **Launch-template mode** for self-provisioning auto scaling groups.

## Known risks

- battery has no working daemon. Until it does, warm pools are exercised
  only against the fake, and a deployed fleet runs every job as overflow
  through brigade. That path is fully specified and is the fallback the
  design already needs for bursts, so nothing is wasted, but the latency
  benefit arrives only with battery.
- brigade and battery are single-author projects a few weeks old. Their
  APIs are pinned in this spec by proto package name; bumps are expected.
- brigade pins flintlock v0.11.0 protos; `cpu_config` is excluded from
  Profiles until brigade is rebuilt against a newer API.
- battery cannot be fronted by brigade because its host agent is addressed
  per physical host. Warm-pool placement is therefore battery's, overflow
  placement is brigade's, and the Fleet Controller gives both the same host
  list and labels so the two agree on what is eligible.
- Placement resolution is a fan-out of `GetMicroVM` over candidate hosts.
  Fine for tens of hosts; a placement field in either upstream response
  would replace it.
- gitlab-runner's exported API churns between minors. Pin a commit and
  re-vet `common.Network`, `ExecutorProvider` and `spec.Job` on every bump.
- Job images need `gitlab-runner-helper` (artifacts/cache) and the flintlock
  guest agent baked in. An image-building recipe is out of scope for the
  requirements but is a prerequisite for the first real job.
