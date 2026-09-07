# Architecture

This document is the non-normative companion to the requirements in
`docs/requirements/`. It explains what the pieces are, why they are arranged
this way, and what was found about the upstream projects when the design was
made (September 2026).

## What it is

`flintlock-runner` is a GitLab CI runner that executes every job in its own
Firecracker or Cloud Hypervisor microVM on a fleet of bare-metal EC2 hosts.
It is built on two liquidmetal components:

- [flintlock](https://github.com/liquidmetal-dev/flintlock) runs microVMs on
  each host and exposes a guest-agent exec API over vsock.
- [battery](https://github.com/liquidmetal-dev/battery), the Pool Manager,
  keeps pools of pre-booted microVMs on those hosts, decides which host each
  one lives on, leases them out, and deletes and replaces them after use.
  Every job runs in a leased pool microVM. The runner declares the pools
  from its own profile configuration and never creates, deletes or places a
  microVM itself.

The runner is a single Go binary built from the gitlab-runner Go packages,
with a scheduling component inserted where the stock runner would talk to a
container runtime.

```
                 ┌──────────────────────── Control Node ─────────────────────────┐
                 │  flintlock-runner                                              │
   GitLab  ◄─────┼─►  gitlab-runner run loop (commands.RunCommand)                │
  jobs/request   │        │ Acquire / Release                                     │
  trace, artifacts        ▼                                                       │
                 │   Executor "flintlock" ──► Scheduler ──► capacity, profiles,   │
                 │        │                       │        leases, placement      │
                 │        │ Guest Transport        │                              │
                 │        │ (MicroVMExec)          └──► battery poolmgrd          │
                 │        │                               ClaimVM / Heartbeat /   │
                 │        │                               ReleaseVM · PoolAdmin   │
                 └────────┼─────────────────────────────────┬─────────────────────┘
                          │ exec over vsock, GetMicroVM      │ creates / deletes pool VMs
          ┌───────────────▼─────────────┐    ┌───────────────▼─────────────┐
          │ Host A (m7g.metal)          │    │ Host B (m7g.metal)          │
          │  flintlockd :9090           │    │  flintlockd :9090           │
          │  poolmgr-hostagent          │    │  poolmgr-hostagent          │
          │  containerd + thinpool      │    │  containerd + thinpool      │
          │  bridge + dnsmasq + NAT     │    │  bridge + dnsmasq + NAT     │
          │  buildkitd, go proxy, mirror│    │  buildkitd, go proxy, mirror│
          │   ┌────┐ ┌────┐ ┌────┐      │    │   ┌────┐ ┌────┐             │
          │   │job │ │job │ │warm│      │    │   │job │ │warm│             │
          │   └────┘ └────┘ └────┘      │    │   └────┘ └────┘             │
          └─────────────────────────────┘    └─────────────────────────────┘
```

## Life of a job

1. The run loop calls `Executor.Acquire`. The Scheduler grants a reservation
   only if a slot is free *and* some pool has a warm microVM available.
   Otherwise no job is requested from GitLab and it waits in GitLab's queue.
2. A job arrives. Its `image:` keyword names a Profile (kernel, rootfs,
   sizing, transport, pool). Unknown image, no default profile: the job
   fails immediately without touching a pool.
3. The Scheduler calls battery `ClaimVM` on the profile's pool. On success it
   holds a lease and heartbeats it for the life of the job. battery starts a
   replacement boot at once (immediate-on-lease replenishment), which is how
   bursts larger than the pool are absorbed: the next job waits one boot.
4. The Scheduler resolves *placement*: which host runs the microVM. The
   claim response should carry it (an upstream request asks for that); until
   it does, the Scheduler asks each of the pool's hosts with `GetMicroVM`.
5. The Executor waits for the guest to answer over the Guest Transport,
   injects the host-service environment for that host, then streams each
   generated bash stage script into the guest via flintlock's
   `MicroVMExec.ExecCommand` on that host, with the script on stdin.
6. On completion the lease is released. battery deletes the microVM and the
   slot is returned. The runner has nothing to garbage-collect.

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
lease back.

**Scheduler.** The component the design exists to insert. It owns:

- *Capacity*: how many jobs may be requested from GitLab right now,
  computed from warm pool availability.
- *Profile resolution*: `image:` names a profile, never a container image.
- *Claims*: one `ClaimVM` per job, with backoff while a pool is exhausted.
- *Placement resolution*: learn which host got the microVM, because the
  exec transport talks to that host directly.
- *Pool declaration*: create or update one battery pool per profile at
  startup and on reload, so profiles are the single source of truth.
- *Lease lifecycle*: heartbeats, release, host health probing.

It never picks a host and never deletes a microVM. battery does both.

**Guest Transport.** Default is flintlock's `MicroVMExec.ExecCommand`
streaming RPC, served by `flintlockd` over the microVM's vsock via the
guest-agent. It supports stdin streaming, cwd, env, user, a timeout and
returns an exit code, which is everything a stage needs. An SSH transport
(via flintlock's `MicroVMSSHProxy` or direct TCP) is specified as an
alternative for images without the guest agent.

**Host services.** Every host also runs rootless `buildkitd`, a Go module
proxy (Athens) that serves private modules with a read-only fleet
credential as well as public ones, a pull-through registry mirror and a
generic HTTP cache for configured upstreams such as npm and PyPI, all bound
to the guest bridge gateway so only that host's guests can reach them. The
executor injects `BUILDKIT_HOST`, `GOPROXY` and friends into every job once
it knows which host the job landed on, so a fresh microVM starts with warm
layers and modules. Build outputs such as `GOCACHE` cross hosts through
GitLab's distributed cache on S3, using pre-signed URLs generated on the
control node so no AWS credentials enter a guest.

**Fleet Controller.** `flintlock-runner fleet {provision,verify,drain,
teardown,emit-userdata}`. Discovers `*.metal` instances by tag, provisions
them over SSM Run Command with `flintlock-provision all`, sets up a NAT'd
guest bridge with dnsmasq, installs the battery host agent and the host
services on every host and battery's daemon on the control node, pre-pulls
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

**battery only, no brigade.** An earlier draft put
[brigade](https://github.com/liquidmetal-dev/brigade), the flintlock
orchestrator, in front of the fleet so the runner could create "overflow"
microVMs when a pool was empty. It was dropped because battery already
covers that case: with immediate-on-lease replenishment every claim starts
a replacement boot, so a burst waits one boot time either way, and pool
size becomes idle headroom rather than a concurrency ceiling. Removing
brigade deleted the runner's microVM spec builder, create polling, garbage
collection, orphan labels, orchestrator health and an Erlang cluster to
operate, and left exactly one component deciding where microVMs live.
brigade remains the right tool if something other than this runner needs a
fleet-wide flintlock API; it is just not in the runner's path.

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
- **battery** (created 2026-09-05, no releases). Go, module
  `github.com/liquidmetal-dev/battery`. Protos for `PoolAdmin`, `Lease`
  (`ClaimVM`/`Heartbeat`/`ReleaseVM`), `Events` and a per-host `Hostagent`
  are defined; the daemons are stubs. A released VM is deleted and replaced,
  never reused. Hosts are a static JSON list and the host agent is reached
  through each VM's vsock path. Placement in v1 is round-robin or
  least-VMs across the pool's hosts. TLS only; no basic-auth token support
  in its client yet. `ClaimVMResponse` does not yet name the host.
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

## Development environment

This phase has no EC2 account, no KVM on the development machine and no
battery daemon upstream. The requirements in `10-test-doubles.md` specify
the fakes that stand in for all three, and `docs/PLAN.md` sets out how the
work is split and coordinated around them. EC2 remains the production
target; only the order of verification changes, with the fake stack first,
real hosts second and EC2 last.

## Delivery order

battery is load-bearing and its daemon does not exist yet, which sets the
order:

1. **Clients and fakes.** flintlock and battery gRPC clients with a
   conforming in-process fake for battery, driven by the requirements in
   `04-pool-manager.md` and `05-hosts.md`. The fake is what the Scheduler's
   tests run against, and it is also what the first real fleet runs on
   until battery lands: a fake that creates directly on hosts is a working
   pool manager, just not a production one.
2. **Scheduler and Executor** against the fake, then against real flintlock
   hosts with the fake standing in for battery.
3. **Fleet Controller** for EC2: provisioning, host services, battery agent
   installation, verification.
4. **Real battery** as soon as its lease and reconciliation logic lands
   upstream, with the host field on `ClaimVMResponse` if accepted.
5. **Launch-template mode** for self-provisioning auto scaling groups.

## Known risks

- battery has no working daemon and this design has no other source of
  microVMs. The in-process fake covers development and early fleets, and
  everything the runner needs from battery is in the published protos, but
  production depends on upstream landing.
- battery is a single-author project a few weeks old, single-instance with
  SQLite and HA deferred. The runner is also single-instance, so this does
  not lower availability below the runner's own, but a battery restart
  pauses new claims until it is back.
- Placement resolution is a fan-out of `GetMicroVM` over a pool's hosts
  until battery reports the host on `ClaimVMResponse` (upstream request
  filed). Fine for tens of hosts.
- battery's v1 placement does not account for vCPU or memory; pool sizes
  have to be chosen so that the sum over pools fits the hosts. Capacity-aware
  placement is a natural upstream follow-up.
- gitlab-runner's exported API churns between minors. Pin a commit and
  re-vet `common.Network`, `ExecutorProvider` and `spec.Job` on every bump.
- Job images need `gitlab-runner-helper` (artifacts/cache), `buildctl` and
  the flintlock guest agent baked in. An image-building recipe is out of
  scope for the requirements but is a prerequisite for the first real job.
