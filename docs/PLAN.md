# Working plan

How flintlock-runner gets built from the specification in
`docs/requirements/`, by several agents in parallel, without an EC2 account,
without KVM on the development machine and without a working battery
upstream. This document is the coordination contract; GitHub issues carry
the live state.

## Constraints this phase

- **No EC2.** EC2 remains the production target, but no instances exist for
  development or testing. The EC2 and Systems Manager code paths are built
  behind narrow interfaces and tested against fakes (`TD-040` to `TD-043`).
  A static discovery provider over SSH (`TD-044`) is the path that can be
  exercised against any Linux machine.
- **No KVM locally.** The development machine (an arm64 OrbStack VM) has no
  `/dev/kvm`, so no real `flintlockd` runs here. The fake Host (`TD-020` to
  `TD-026`) stands in. A hardware tier (`TD-052`) runs the same scenarios
  against real hosts whenever someone has them.
- **No battery release yet.** battery `main` gained a working daemon on
  2026-09-07 (see `docs/architecture.md`), but there is no tagged release and
  no host to run it against here. The fake Pool Manager (`TD-001` to `TD-010`) is
  a minimal but real pool manager on the battery protos; early fleets can
  run on it as a standalone binary (`TD-009`).

Consequence: the end-to-end harness (`TD-050`, `TD-051`) is the definition
of "works". Every work package is done when its scenarios pass there.

## Coordination

- **Unit of assignment is the requirement ID.** Each GitHub issue lists the
  IDs it owns. No two open issues claim the same ID. The issue assignee is
  the agent; the PR is the status.
- **One branch, one worktree, one PR per work package.** Branch names are
  `wp/<key>`. Agents do not push to `main`.
- **Definition of done for a PR:** every owned ID has an implementation
  citation and a test citation in duvet's JSON report; `go vet`,
  `golangci-lint` and `go test ./...` pass; the end-to-end scenarios the
  issue names pass; no new `// TODO` without a `type=todo` duvet annotation
  and a tracking issue.
- **Per-PR coverage gate, not the snapshot.** CI runs `duvet report` and a
  script that reads `.duvet/reports/report.json` and fails if any ID listed
  in the PR body lacks citations. `.duvet/snapshot.txt` is regenerated and
  committed only at milestones, by the lead, to avoid every parallel PR
  conflicting on it.
- **Interfaces change through one PR.** The Go interfaces in
  `internal/*/interfaces.go` are owned by the lead. An agent that needs a
  change opens a small PR against those files first; nobody widens an
  interface inside a feature PR.
- **Human review points.** The interfaces PR (Phase 0), the first executor
  PR, the first fleet provisioning PR, and every milestone snapshot.

## Phases and work packages

### Phase 0: skeleton (serial, lead)

`go.mod` pinned to a gitlab-runner commit; package layout from the README;
Go interfaces for `Scheduler`, `PoolManager`, `HostClient`,
`GuestTransport`, `Executor`, `Discovery`, `Remote`; empty fakes that
compile; `Makefile` (`build`, `test`, `lint`, `duvet`, `e2e`); GitHub Actions
workflow; `CONTRIBUTING.md` with the citation rules; the per-PR coverage
script; six issues opened from the table below.

### Phase 1: parallel work packages

| Key | Package | Owns | Depends on | E2E scenarios |
|-----|---------|------|------------|---------------|
| `fakes` | `internal/poolmgr/fake`, `internal/flintlock/fake`, `internal/testing/fakegitlab`, `internal/testing/fakes3`, `cmd/fake-poolmgr` | TD-001..TD-034 | interfaces | harness boots |
| `poolmgr` | `internal/poolmgr` | PL | interfaces, `fakes` (poolmgr fake) | claim, heartbeat, release, exhaustion wait, lease expiry |
| `hosts` | `internal/flintlock`, `internal/transport` | HO, EX-040..EX-051 | interfaces, `fakes` (host fake) | exec stream, host unhealthy, ServerInfo variants |
| `scheduler` | `internal/scheduler` | SC | interfaces | capacity refusal, unknown image, placement both paths |
| `config` | `internal/config` | CF | nothing | validation cases, defaults, reload |
| `executor` | `internal/executor`, `cmd/flr run` | EX-001..EX-033, EX-060..EX-066, GL, OB, SE (runner-side) | all of the above | successful job, script failure, cancel, timeout, artifacts, cache, env injection |
| `fleet` | `internal/fleet`, `cmd/flr fleet` | FL, TD-040..TD-044, SE (fleet-side) | `config` | static-provider provision against a local SSH target; script lint |
| `harness` | `internal/testing/harness`, `make e2e` | TD-050..TD-054 | `fakes` to boot, `executor` to run a job | every TD-051 scenario; hardware tier gated by env var |

`fakes` and `config` start immediately after Phase 0. `poolmgr`, `hosts` and
`scheduler` start as soon as the corresponding fake exists, which is days
not weeks since the fakes are the first thing `fakes` delivers. `executor`
starts against interfaces and mocks and integrates as the others land.
`fleet` is independent throughout.

OB and SE requirements are cross-cutting; they are owned by the package
that owns the behaviour (listed above as runner-side or fleet-side) rather
than by a separate agent.

### Phase 2: integration

The harness against the fake stack in CI on every PR. Then, when a KVM
machine is available to anyone on the project, the hardware tier: static
inventory, SSH provisioning of a real host, real `flintlockd`, fake Pool
Manager as the standalone binary. Then a build of battery `main` (its daemon works as of 2026-09-07), and
a tagged release when one exists; PR #45 already returns the host on the claim.

### Phase 3: EC2

Only when an account exists. The `ec2` discovery provider and the `ssm`
remote against real services, the launch-template path, and a metal
instance in the hardware tier.

### Phase 4: cluster fleet

Decided 2026-09-21: the fleet moves to Kubernetes, specified in
`11-host-image.md` and `12-cluster-fleet.md`. Hosts are nodes of an existing
workload cluster made by a Cluster API MachineDeployment from a bootc Host
Image. Every MicroVM is a pod on a per-Host Virtual Node served by a virtual
kubelet Pod Provider in front of the local `flintlockd`; a Pool is a
ReplicaSet and a claim is a label update, so battery, the Inventory and Host
mutual TLS drop out of a cluster fleet. The constraints of this phase still
hold, so every package below is done against fakes: a container build for
the image, an API server test environment and the fake Host for the rest.

| Key | Package | Owns | Depends on | Done when |
|-----|---------|------|------------|-----------|
| `image` | `image/` (Containerfile, units, versions file, check stage) | HI-001..HI-062 | nothing | PR #52 |
| `kube-pool` | `internal/poolmgr/kube` | KF-040..KF-052, KF-110, KF-121, KF-123 | nothing | PR #51 |
| `provider` | `internal/kubelet`, `cmd/flr kubelet` | KF-010..KF-018, KF-020..KF-032, KF-090..KF-094, KF-111, KF-120, KF-124, KF-125 | nothing | PR #53 |
| `kube-alloc` (lead) | `internal/scheduler`, `internal/scheduler/interfaces.go` | KF-126, KF-127 | `kube-pool` | a Job allocates on the Kubernetes pool backend with no Inventory, and its pod's active deadline is the Job's timeout |
| `kube-exec` | `internal/transport`, `internal/executor`, `internal/config` | KF-060..KF-063, KF-128 | `provider` (exec endpoint), `kube-alloc` | the `exec` transport's test suite passes over `kube-exec` |
| `provider-hardening` | `internal/kubelet`, `deploy/host-agent`, `image/` (firewall) | KF-130..KF-134, KF-136, KF-137, HI-063 | `provider`, `image` | an identity the review refuses runs nothing; one Host's identity cannot touch another Host's objects |
| `manifests` | `deploy/` | KF-001..KF-005, KF-070..KF-076, KF-080..KF-082, KF-112, KF-113, KF-135, KF-144 | `provider`, `provider-hardening` (RBAC and policy files) | manifests render and pass a schema check |
| `packaging` | `.goreleaser.yaml`, release workflow, `docs/RELEASING.md` | KF-140..KF-143, KF-145 | `manifests` for KF-143 | a packaging dry run on the PR builds every image and the manifests asset |
| `hardening-2` | `image/` (firewall, SELinux), `deploy/host-agent/admission-policy.yaml`, `internal/kubelet` (default listen) | HI-064, HI-065, HI-066, KF-139 | `provider-hardening`, `manifests` (the Host Service user ids) | the Host Service user ids cannot reach the metadata service or the control ports; the Host Agent can read the Host environment file and write the cache under enforcing SELinux; a Pod Provider cannot touch another Host's Lease |
| `kube-verify` | `internal/fleet/verify` | KF-100..KF-102 | `kube-exec` | verification passes in the harness |
| `kube-harness` | `internal/testing/harness` | KF-122 | `kube-pool`, `provider`, `kube-exec` | every TD-051 scenario green over the cluster stack |

`image`, `kube-pool` and `provider` are done. `kube-alloc` is the lead's
change to the Scheduler and its interfaces; `kube-exec` and
`provider-hardening` start as soon as their dependencies are on `main`, then
`manifests`, then `packaging`, `kube-verify` and `kube-harness`. The battery
client, the Host client and push provisioning keep working throughout; they
are retired, with their requirements, only after a cluster fleet has passed
the hardware tier.

#### Release readiness

A cluster fleet is releasable when every KF and HI requirement has an
implementation and a test citation, the harness passes over the cluster
stack, and a release candidate's images and manifests have been built by the
release workflow. The version it ships in is decided when the candidate is
cut. Nothing in this phase has run on a real Host or a real kubelet; a
release candidate is where the hardware tier (`TD-052`) first applies to it.

### Phase 5: battery claim resources

Decided 2026-09-22: the cluster fleet moves from the Virtual Node design to
battery's claim resources (`12-cluster-fleet.md` from `#battery-claims`).
battery stays the scheduler and the authority over Pools; the Runner claims
a MicroVM with a `MicroVMClaim` and runs each Stage through a per-Host Exec
Agent.

Settled 2026-09-28: the resources ship in
[battery-operator v0.1.0](https://github.com/phoban01/battery-operator/releases/tag/v0.1.0),
as `Pool` and `MicroVMClaim` in `battery.liquidmetal-x.dev/v1alpha1`,
in front of an unmodified battery v0.3.3. battery-operator also owns two
parts that this phase first planned here: the Exec Agent (its EA
requirements) and the Inventory Controller (its IN requirements). Its
Client Library, `pkg/claimclient`, claims, renews and releases a claim,
and dials the claim's Exec Agent with a claim token: a token of the claim's
Holder, bound to that one claim. So the Runner builds on
`pkg/claimclient` and `api/v1alpha1` at v0.1.0, and nothing waits for a
field name any more.

What carries over unchanged: the Host Image, the Cluster API host pool, the
Host Services, packaging and release, and the Runner's Scheduler and
Executor above `poolmgr.Client`. The Pod Provider's relay, TLS front,
readiness checks and drain guard live on in battery-operator's Exec Agent.

| Key | Package | Owns | Depends on | Issue | Done when |
|-----|---------|------|------------|-------|-----------|
| `exec-agent` | done in battery-operator v0.1.0; here, removes `internal/agent`, `cmd/flr agent`, `deploy/agent` and the Exec Agent parts of `deploy/host-agent` | withdraws KF-170..KF-182, KF-018, KF-135 | `claim-interfaces` | [#74](https://github.com/phoban01/flintlock-runner/issues/74) | nothing on `main` cites a withdrawn requirement, and `deploy/README.md` says the Exec Agent comes from battery-operator's Manifests |
| `inventory` | done in battery-operator v0.1.0 | withdraws KF-160, KF-161 | nothing | [#75](https://github.com/phoban01/flintlock-runner/issues/75) | withdrawn in `12-cluster-fleet.md` |
| `claim-interfaces` (lead) | `internal/config/types.go`, `internal/executor/provider.go` (`AgentHosts`) | none | nothing | [#76](https://github.com/phoban01/flintlock-runner/issues/76) | the configuration has a `claim` pool backend and its settings, and `AgentHosts` looks a client up per claim; no behaviour changes |
| `agent-exec` | `internal/transport`, `internal/executor` | KF-185..KF-189, KF-190 | `claim-interfaces` | [#74](https://github.com/phoban01/flintlock-runner/issues/74) | the `exec` transport's suite passes over `agent-exec` with a claim token for each claim, against battery-operator's Exec Agent or a test double of its protocol |
| `claim-backend` | `internal/poolmgr/claim`, on battery-operator's `pkg/claimclient` and `api/v1alpha1` at v0.1.0 | KF-150..KF-156, KF-191 | `claim-interfaces` | [#77](https://github.com/phoban01/flintlock-runner/issues/77) | the Scheduler's scenarios pass against the fake battery |
| `manifests-2` | `cmd/flr` (the claim backend and `agent-exec` wiring), `deploy/runner`, `deploy/README.md` (reworks the held `pr/manifests`) | the KF IDs of KF-001..KF-005, KF-070..KF-076, KF-080..KF-082, KF-112, KF-113, KF-144 that still stand | `agent-exec`, `claim-backend` | [#78](https://github.com/phoban01/flintlock-runner/issues/78) | `flr` runs on the claim backend and `agent-exec`, and the manifests render and pass the schema check next to battery-operator v0.1.0's Manifests |
| `claim-harness` | `internal/testing/harness` (reworks the held `wp/kube-harness`), a scripted run on a real Host | KF-192, KF-193 | `manifests-2` | [#79](https://github.com/phoban01/flintlock-runner/issues/79) | every TD-051 scenario green over the claim stack, and a Job from the fake GitLab runs end to end on battery-operator's real hosts trial |
| `withdraw-vk` | removes `internal/kubelet`, `internal/poolmgr/kube`, `kube-exec` and their manifests | withdraws KF-010..KF-032, KF-040..KF-052, KF-060..KF-063, KF-090..KF-094, KF-100..KF-102, KF-110, KF-111, KF-120..KF-128, KF-130..KF-137, less KF-018 and KF-135, which `exec-agent` withdraws | `claim-harness` | none yet | nothing on `main` cites a withdrawn requirement |

`inventory` is done with this plan. `claim-interfaces` (#76) goes first,
because it changes lead-owned interfaces. Then `exec-agent` with
`agent-exec` (#74), and `claim-backend` (#77), in parallel. Then
`manifests-2` (#78), and last `claim-harness` with the real Host run
(#79). The held branches of Phase 4 are not merged as they stand:
`pr/manifests` becomes `manifests-2`, `wp/kube-harness` becomes
`claim-harness`, and `wp/kube-verify` is dropped until verification is
specified for the claim design. `wp/hardening-2` (HI-064..HI-066, KF-139)
carries over.

## Running agents

From a Claude Code session: one agent per work package, each with
`isolation: worktree`, prompted with the issue number, the owned IDs, the
package boundaries and the definition of done. The lead session reviews
each PR with `/code-review`, runs the harness, and merges. Five to six
agents in flight is the practical ceiling; more than that and the
interfaces PR becomes the bottleneck.

## Milestones

1. **M1 Skeleton.** Phase 0 merged; `make e2e` starts the fake stack and
   runs a trivial job through a stub executor.
2. **M2 First job.** A real job from fake GitLab runs in a fake Host via
   the real scheduler, poolmgr client and exec transport. Snapshot
   committed.
3. **M3 Full harness.** Every TD-051 scenario green in CI. Snapshot
   committed.
4. **M4 Fleet.** Static-provider provisioning of a real Linux host over
   SSH, host services included, verified by `fleet verify` against the
   fake Pool Manager binary. Snapshot committed.
5. **M5 Hardware.** Hardware tier green against at least one KVM host.
6. **M6 EC2.** Deferred until an account exists.
