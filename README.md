# flintlock-runner

A GitLab CI runner that runs every job in its own Firecracker/Cloud Hypervisor
microVM on a fleet of bare-metal EC2 hosts. Built on two liquidmetal
components: [flintlock](https://github.com/liquidmetal-dev/flintlock) runs
the microVMs and [battery](https://github.com/liquidmetal-dev/battery) keeps
warm pools of them on the hosts and leases them out. The runner itself is
built from the gitlab-runner Go packages with a scheduling component
inserted at the executor boundary; it never creates, places or deletes a
microVM, it only claims and releases them.

The project is currently at the specification stage. There is no Go code
yet; what exists is:

| Path | Contents |
|------|----------|
| `docs/architecture.md` | What the system is, why it is shaped this way, what was found upstream, delivery order, risks |
| `docs/requirements/` | The normative specification: EARS requirements in nine documents, traced with duvet |
| `.duvet/config.toml` | duvet configuration (sources, specifications, reports, snapshot) |
| `Makefile` | `make duvet` / `make duvet-ci` |

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

## Layout the requirements assume

```
cmd/flintlock-runner/   main: run, config show, fleet {provision,verify,drain,teardown,emit-userdata}
internal/executor/      common.ExecutorProvider + common.Executor ("flintlock")
internal/scheduler/     capacity, profiles, claims, placement resolution, lease keep-alive
internal/transport/     Guest Transport: exec (MicroVMExec), ssh (MicroVMSSHProxy / TCP)
internal/flintlock/     gRPC client for hosts (exec, ssh proxy, GetMicroVM, ServerInfo)
internal/poolmgr/       gRPC client for battery, plus an in-process fake that is itself a minimal pool manager
internal/fleet/         EC2 discovery, SSM/SSH execution, host provisioning, host services, inventory
internal/config/        YAML schema, validation, RunnerConfig translation
```
