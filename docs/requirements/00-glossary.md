# Glossary

This document defines the terms used by the normative documents in this
directory. It contains no requirements.

## System names {#system-names}

- **Runner** — the `flintlock-runner` process as a whole: the GitLab-facing
  loop, the Executor, the Scheduler and their supporting clients. Built from
  the gitlab-runner Go packages (`common`, `network`, `shells`, `executors`).
- **Executor** — the component implementing the `common.ExecutorProvider` and
  `common.Executor` interfaces of gitlab-runner under the executor name
  `flintlock`. It prepares a MicroVM for a Job, runs each build stage in it
  and cleans up.
- **Scheduler** — the scheduling component inserted between the Runner loop
  and the Executor. It decides whether the Runner may ask GitLab for a Job,
  which Profile a Job needs, claims a warm MicroVM from that Profile's Pool,
  keeps the Lease alive and releases it. It never creates, deletes or places
  a MicroVM.
- **Guest Transport** — the mechanism by which the Executor runs a command
  inside a MicroVM and streams its input and output. The default transport is
  the flintlock `MicroVMExec.ExecCommand` streaming RPC served by the Host
  that runs the MicroVM; an alternative transport is SSH.
- **Fleet Controller** — the `flintlock-runner fleet` subcommands that
  discover, provision, verify and tear down Hosts on EC2, install the Pool
  Manager and Host Services, and generate the Runner configuration from the
  resulting inventory.

## External systems {#external-systems}

- **GitLab** — the GitLab instance the Runner takes Jobs from, via the Runner
  REST API (`/api/v4/jobs/request`, `/api/v4/jobs/:id`,
  `/api/v4/jobs/:id/trace`, artifact endpoints).
- **Host** — a bare-metal machine (on EC2, a `*.metal` instance) running
  `flintlockd`, containerd with a devicemapper thin pool, Firecracker and/or
  Cloud Hypervisor, the Pool Manager's host agent and the Host Services.
  Identified by a name and a gRPC endpoint.
- **Pool Manager** — [battery](https://github.com/liquidmetal-dev/battery), the
  service that keeps warm MicroVMs in named Pools, places them on Hosts,
  hands them out through its `Lease` gRPC service (`ClaimVM`, `Heartbeat`,
  `ReleaseVM`), deletes them on release and provisions replacements. It is
  the source of every MicroVM a Job runs in; the Runner declares its Pools
  from the Profiles.
- **Host Services** — the per-Host daemons the Fleet Controller installs for
  jobs to use: rootless `buildkitd`, a Go module proxy for public and
  private modules, a pull-through registry mirror and an HTTP cache for
  configured upstreams, all bound to the guest bridge gateway address.
- **Distributed cache** — the GitLab `cache:` mechanism, backed by an S3
  bucket, through which build outputs move between Jobs on different Hosts.
- **Control Node** — the machine running the Runner process and the Pool
  Manager daemon. It can be a Host or a separate, non-metal instance with
  network reachability to every Host.

## Domain terms {#domain-terms}

- **Job** — a single GitLab CI job as returned by `POST /api/v4/jobs/request`
  and represented by `spec.Job` in the gitlab-runner packages.
- **Job Image** — the value of the `image:` keyword of a Job, if any. The
  Runner treats it as the name of a Profile rather than as a container image.
- **Profile** — a named, configured description of the MicroVM a class of
  Jobs runs in: architecture, vCPU and memory, kernel image and command line,
  root filesystem image, extra volumes, hypervisor provider, cloud-init data,
  shell path, builds and cache directories, and the settings of the Pool it
  is bound to.
- **Default Profile** — the Profile used for Jobs that do not specify a Job
  Image.
- **Pool** — a named set of pre-booted MicroVMs, all created from one
  template, managed and placed by the Pool Manager.
- **Lease** — the Pool Manager's grant of one warm MicroVM to the Runner,
  identified by a lease id and kept alive by heartbeats.
- **MicroVM** — a Firecracker or Cloud Hypervisor virtual machine created via
  flintlock, identified by its flintlock `uid`.
- **Warm MicroVM** — a MicroVM obtained from a Pool through a Lease.
- **Reservation** — the Scheduler's promise of capacity for one not-yet-known
  Job, taken before the Runner asks GitLab for work and released either when
  no Job arrives or when the Job's Lease is released.
- **Allocation** — the binding of a specific leased MicroVM to a specific Job.
- **Placement** — the Host that runs a given MicroVM, as reported by the Pool
  Manager or resolved by asking the Pool's Hosts.
- **Slot** — one unit of Runner concurrency; the number of Slots equals the
  configured maximum number of concurrent Jobs.
- **Inventory** — the Fleet Controller's record of Hosts: name, endpoint,
  architecture, capacity, labels, Host Service addresses, installed versions.
- **Stage** — one of the gitlab-runner build stages (`prepare_script`,
  `get_sources`, `restore_cache`, `download_artifacts`, `step_*`,
  `after_script`, `archive_cache`, `upload_artifacts_on_*`,
  `cleanup_file_variables`), each delivered to the Executor as a generated
  shell script.
