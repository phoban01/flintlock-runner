# flintlock-runner

A GitLab CI runner that runs every job in its own Firecracker/Cloud Hypervisor
microVM on a fleet of bare-metal EC2 hosts. Built on two liquidmetal
components: [flintlock](https://github.com/liquidmetal-dev/flintlock) runs
the microVMs and [battery](https://github.com/liquidmetal-dev/battery) keeps
warm pools of them on the hosts and leases them out. The runner itself is
built from the gitlab-runner Go packages with a scheduling component
inserted at the executor boundary; it never creates, places or deletes a
microVM, it only claims and releases them.

The project is at the skeleton stage (Phase 0 of `docs/PLAN.md`): the
specification is complete, the Go module pins its upstream dependencies, the
package interfaces are defined and the fakes compile as empty types. What
exists is:

| Path | Contents |
|------|----------|
| `docs/architecture.md` | What the system is, why it is shaped this way, what was found upstream, delivery order, risks |
| `docs/requirements/` | The normative specification: EARS requirements in ten documents, traced with duvet |
| `docs/PLAN.md` | How the work is split into work packages and what "done" means |
| `cmd/`, `internal/` | The package skeleton below: interfaces, configuration schema, empty fakes |
| `.duvet/config.toml` | duvet configuration (sources, specifications, reports, snapshot) |
| `hack/duvet-coverage.sh` | The per-PR requirement coverage gate |
| `Makefile` | `make build`, `test`, `lint`, `duvet`, `coverage-gate`, `e2e` |
| `CONTRIBUTING.md` | Citation rules, branch naming, definition of done |

## Requirements tracing

Requirements are written in EARS form with uppercase `SHALL` so that
[duvet](https://github.com/awslabs/duvet) extracts one requirement per
sentence. Code and tests cite them with `//=` / `//#` comments; `duvet
report` produces an HTML coverage report and a text snapshot that CI diffs.
See `docs/requirements/README.md` for the authoring rules and annotation
syntax.

```sh
cargo install duvet --locked
make duvet            # .duvet/reports/report.html
make duvet-ci         # fails if .duvet/snapshot.txt is stale
```

## Layout

```
cmd/flintlock-runner/   main: run, config show, fleet {provision,verify,drain,teardown,emit-userdata}
internal/clock/         Clock and Backoff interfaces shared by the scheduler, poolmgr and the fakes
internal/executor/      common.ExecutorProvider + common.Executor ("flintlock")
internal/scheduler/     capacity, profiles, claims, placement resolution, lease keep-alive
internal/transport/     Guest Transport: exec (MicroVMExec), ssh (MicroVMSSHProxy / TCP)
internal/flintlock/     gRPC client for hosts (exec, ssh proxy, GetMicroVM, ServerInfo); fake/ is the fake host
internal/poolmgr/       gRPC client for battery and the PL policy units; fake/ is itself a minimal pool manager
internal/fleet/         EC2 discovery, SSM/SSH execution, host provisioning, host services, inventory
internal/config/        YAML schema, validation, RunnerConfig translation
internal/testing/       fakegitlab and fakes3 for the end-to-end harness
hack/                   CI helper scripts
```
