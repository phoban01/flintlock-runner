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

Decided 2026-09-21: the fleet moves to Kubernetes as its management plane,
specified in `11-host-image.md` and `12-cluster-fleet.md`. Hosts are nodes
of an existing workload cluster made by a Cluster API MachineDeployment from
a bootc Host Image; jobs stay on the battery path and are never pods. The
constraints of this phase still hold, so every package below is done against
fakes: a container build for the image, an API server test environment for
the operator.

| Key | Package | Owns | Depends on | Done when |
|-----|---------|------|------------|-----------|
| `image` | `image/` (Containerfile, units, versions file, check stage) | HI | nothing | the check stage of HI-008 passes in CI |
| `inventory-watch` | `internal/config` | KF-027 | nothing | Runner reloads a changed Inventory file in the harness |
| `operator` | `internal/fleet/operator`, `cmd/flr fleet operator` | KF-012, KF-020..KF-026, KF-060..KF-065, KF-080, KF-090, KF-091 | `inventory-watch` | KF-091 scenario passes |
| `host-agent` | `internal/fleet/hostagent`, `deploy/host-agent` | KF-010, KF-011, KF-031, KF-040..KF-047, KF-083 | `image` | agent reports ready against the fake Host |
| `manifests` | `deploy/` | KF-001..KF-005, KF-028, KF-030, KF-032..KF-034, KF-050..KF-054, KF-070, KF-071, KF-081, KF-082 | `operator`, `host-agent` | manifests render and pass a schema check |

`image` and `inventory-watch` start immediately and do not touch each other.
Push provisioning (`06-fleet.md`) keeps working throughout; its packages are
retired, with their requirements, only after a cluster fleet has passed the
hardware tier.

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
