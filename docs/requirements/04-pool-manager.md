# Pool Manager integration {#pool-manager-integration}

This document specifies how the Scheduler uses the flintlock warm pool
manager, [battery](https://github.com/liquidmetal-dev/battery). The Pool
Manager is the source of every MicroVM a Job runs in: each Profile is bound
to exactly one Pool, the Runner declares those Pools from its Profiles, and
a Job starts in a MicroVM claimed from its Profile's Pool through a Lease.
The Pool Manager also decides which Host each pool MicroVM lives on and
deletes it after release. The integration targets the `poolmgr.v1alpha1`
gRPC API (`PoolAdmin`, `Lease`, `Events` services). battery's daemon gained a
working lease service, expiry sweeper, reconciler, events stream and SQLite
store on 2026-09-07 but has no tagged release yet, so the requirements here
are written against the protos on `main` and are exercised first against a
conforming fake and then against a build of `main`.

## Client {#client}

- **PL-001** The Scheduler SHALL communicate with the Pool Manager through
  the `poolmgr.v1alpha1` gRPC services using the generated Go client from the
  battery module.
- **PL-002** The Scheduler SHALL apply the configured deadline to every unary
  call to the Pool Manager.
- **PL-003** The Scheduler SHALL connect to the Pool Manager with TLS unless
  the configuration explicitly marks the endpoint insecure.
- **PL-004** If the Pool Manager is unreachable at startup, then the
  Scheduler SHALL keep retrying the connection with exponential backoff and
  the Runner SHALL NOT report itself ready until it succeeds.
- **PL-005** The Scheduler SHALL probe the Pool Manager at the configured
  health interval by calling `ListPools` and SHALL mark it unhealthy after
  the configured number of consecutive failures and healthy again after a
  successful probe.
- **PL-006** The Scheduler SHALL maintain one long-lived connection to the
  Pool Manager with keepalive enabled and SHALL reconnect with exponential
  backoff when it is lost.

## Pool declaration {#pool-declaration}

- **PL-010** When starting and on every configuration reload, the Scheduler
  SHALL create or update one Pool per Profile, deriving the Pool's MicroVM
  template from the Profile and the Pool's size, replenishment strategy,
  hooks and heartbeat settings from the Profile's pool settings.
- **PL-011** The Scheduler SHALL set each Pool's `flintlock_hosts` to the
  Hosts in the Inventory that satisfy the Profile's Host selector and
  architecture.
- **PL-012** The Scheduler SHALL set `allow_guest_agent` in every Pool's
  MicroVM template so that the `exec` Guest Transport works.
- **PL-013** The Scheduler SHALL set the Pool's replenishment strategy to
  immediate-on-lease unless the Profile's pool settings choose another.
- **PL-014** The Scheduler SHALL label every Pool's MicroVM template with
  `gitlab-runner.flintlock.dev/runner` set to the Runner name and
  `gitlab-runner.flintlock.dev/profile` set to the Profile name.
- **PL-015** When a Profile is removed by a configuration reload, the
  Scheduler SHALL NOT delete its Pool and SHALL log that the Pool is no
  longer referenced.
- **PL-016** If `CreatePool` or `UpdatePool` fails for a Profile, then the
  Scheduler SHALL log the error, treat that Pool as empty and retry the
  declaration at the configured interval.
- **PL-017** If two Runners would declare the same Pool name in the same
  namespace, the Runner SHALL use its own namespace for its Pools so that
  they do not collide.

Declaring Pools from Profiles keeps one source of truth: an operator
describes Profiles once and the Runner materialises the corresponding Pools,
instead of keeping the Pool Manager's configuration in step by hand.
Immediate-on-lease replenishment is the default because it is what lets a
Pool absorb a burst: every claim starts a replacement boot, so the Pool's
size is the amount of idle warm capacity, not a ceiling on concurrent Jobs.

## MicroVM template {#microvm-template}

- **PL-020** The Scheduler SHALL build a Pool's MicroVM template from the
  Profile: `vcpu`, `memory_in_mb`, `kernel`, `initrd` where set,
  `root_volume`, `additional_volumes`, `interfaces`, `metadata` and
  `provider` where set.
- **PL-021** The Scheduler SHALL set the template's `namespace` to the
  Runner namespace and SHALL leave its `id` empty for the Pool Manager to
  assign.
- **PL-022** The Scheduler SHALL set `add_network_config` on the template's
  kernel spec so that flintlock generates the guest network configuration
  from the interfaces.
- **PL-023** The Scheduler SHALL NOT use `eth0` as a network interface
  `device_id`.
- **PL-024** The Scheduler SHALL base64-encode every value placed in the
  template's `metadata` map and SHALL only use the keys `meta-data`,
  `user-data`, `vendor-data` and `network-config`.
- **PL-025** Where a Profile provides user-data, the Scheduler SHALL render
  it as a template with the Profile name available and place the result in
  the `user-data` entry.
- **PL-026** The Scheduler SHALL NOT place any Job-specific value in the
  template, because the template is instantiated before any Job exists.

## Claiming {#claiming}

- **PL-030** When a warm MicroVM is needed for a Profile, the Scheduler SHALL
  call `ClaimVM` with the `PoolRef` of that Profile's Pool.
- **PL-031** When `ClaimVM` succeeds, the Scheduler SHALL record the returned
  `lease_id`, `vm_uid`, `network_interfaces` and the `host` name and address
  on the Allocation.
- **PL-032** If `ClaimVM` returns `RESOURCE_EXHAUSTED`, then the Scheduler
  SHALL treat the Pool as having no warm MicroVM available.
- **PL-033** If `ClaimVM` returns `NOT_FOUND`, then the Scheduler SHALL
  re-declare the Pool from its Profile and treat the Pool as having no warm
  MicroVM available.
- **PL-034** If `ClaimVM` fails with `UNAVAILABLE` or a connection error,
  then the Scheduler SHALL mark the Pool Manager unhealthy for the configured
  backoff period.
- **PL-035** While the Pool Manager is unhealthy, the Scheduler SHALL count
  the available warm MicroVMs of every Pool as zero when computing capacity.

## Heartbeat and release {#heartbeat-and-release}

- **PL-040** While an Allocation holds a Lease, the Scheduler SHALL call
  `Heartbeat` with the `lease_id` and SHALL update the Lease expiry from the
  returned `expires_at`.
- **PL-041** When a Job holding a Lease finishes, the Scheduler SHALL call
  `ReleaseVM` with the `lease_id`.
- **PL-042** If `ReleaseVM` returns `NOT_FOUND`, then the Scheduler SHALL
  treat the release as complete.
- **PL-043** If `ReleaseVM` fails for any other reason, then the Scheduler
  SHALL retry with exponential backoff up to the configured retry limit and
  SHALL then log the Lease id and rely on Lease expiry.
- **PL-044** The Scheduler SHALL NOT call `DeleteMicroVM` on any Host.

The Pool Manager deletes a released MicroVM itself and provisions a
replacement according to the Pool's replenishment strategy; a warm MicroVM
is never returned to the Pool for reuse and the Runner never deletes one.

## Capacity tracking {#capacity-tracking}

- **PL-050** The Scheduler SHALL track the number of available warm MicroVMs
  in every Pool by subscribing to the `Events` service.
- **PL-051** When the `Events` stream is unavailable, the Scheduler SHALL fall
  back to polling `GetPool` at the configured interval until the stream can
  be re-established.
- **PL-052** When an event reports a MicroVM in a Pool becoming available,
  claimed, released or deleted, the Scheduler SHALL update that Pool's
  available count before the next capacity computation.
- **PL-053** When a `ClaimVM` call returns `RESOURCE_EXHAUSTED`, the Scheduler
  SHALL set that Pool's available count to zero until the next event or poll
  says otherwise.
- **PL-054** The Scheduler SHALL expose each Pool's available count as a
  metric.
- **PL-055** When an event reports that a Pool is below its target size, the
  Scheduler SHALL log it at `warn` level with the Pool name and the counts.
- **PL-056** When an event reports a hook failure or a quarantined MicroVM
  in a Pool, the Scheduler SHALL log it at `warn` level with the Pool name
  and the MicroVM uid.
