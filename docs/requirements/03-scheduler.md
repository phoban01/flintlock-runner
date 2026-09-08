# Scheduler {#scheduler}

This document specifies the scheduling component. The Scheduler sits between
the gitlab-runner run loop and the Executor. Its inputs are the configured
Profiles, the Inventory of Hosts and the Pool Manager. Its outputs are
Reservations (permission to ask GitLab for a Job), Allocations (a specific
MicroVM bound to a Job) and Placements (which Host runs each MicroVM, needed
by the Guest Transport). Every MicroVM a Job runs in is a warm MicroVM
leased from the Pool Manager; the Scheduler never creates, deletes or places
a MicroVM itself.

## Capacity {#capacity}

- **SC-001** The Scheduler SHALL maintain a count of held Reservations and
  active Allocations and SHALL treat their sum as the number of Slots in use.
- **SC-002** The Scheduler SHALL compute available capacity as the lesser of
  the number of unused Slots and the sum of the number of available warm
  MicroVMs across all Pools.
- **SC-003** When a Reservation is requested and available capacity is
  greater than zero, the Scheduler SHALL grant a Reservation and count it as
  a Slot in use.
- **SC-004** When a Reservation is requested and available capacity is zero,
  the Scheduler SHALL refuse the Reservation.
- **SC-005** When a Reservation is released without an Allocation, the
  Scheduler SHALL return its Slot immediately.
- **SC-006** Where a Profile has a configured maximum concurrency, the
  Scheduler SHALL NOT hold more Allocations for that Profile than the
  maximum.
- **SC-007** The Scheduler SHALL be safe for concurrent use by the number of
  workers the run loop starts.
- **SC-008** When starting, the Scheduler SHALL NOT grant any Reservation
  until it has successfully contacted the Pool Manager at least once.
- **SC-009** The Scheduler SHALL count a Pool's MicroVMs that are being
  provisioned or running create hooks as unavailable when computing
  capacity.

Capacity is computed from warm MicroVMs rather than from Slots alone, so
that the Runner does not accept a Job from GitLab that it then cannot start;
a Job accepted and failed with a system error costs the pipeline a retry,
whereas a Job left in GitLab's queue costs only latency. Bursts larger than
a Pool are absorbed by the Pool Manager's replenishment: with the
immediate-on-lease strategy a replacement starts booting the moment a
MicroVM is claimed, so the next Job waits one boot time rather than being
refused.

## Profile resolution {#profile-resolution}

- **SC-010** The Scheduler SHALL resolve a Profile for a Job by matching the
  Job Image name against Profile image names exactly, then against Profile
  image glob patterns in configuration order, then by falling back to the
  Default Profile.
- **SC-011** If a Job has no Job Image and no Default Profile is configured,
  then the Scheduler SHALL report that no Profile can be resolved.
- **SC-012** If a Job Image matches no Profile and the configuration does not
  allow falling back to the Default Profile for unknown images, then the
  Scheduler SHALL report that no Profile can be resolved.
- **SC-013** The Scheduler SHALL resolve the Profile before claiming any
  MicroVM so that a Job with an unknown image fails without consuming a warm
  MicroVM.

## Allocation {#allocation}

- **SC-020** When an Allocation is requested for a Profile, the Scheduler
  SHALL claim a warm MicroVM from the Profile's Pool.
- **SC-021** If the Pool has no warm MicroVM available, then the Scheduler
  SHALL retry the claim with exponential backoff, waking early on a Pool
  Manager event that reports a MicroVM in that Pool becoming available,
  until the allocation timeout elapses.
- **SC-022** If the allocation timeout elapses before a MicroVM is claimed,
  then the Scheduler SHALL return an allocation error naming the Profile and
  the Pool.
- **SC-023** When the Job's context is cancelled during allocation, the
  Scheduler SHALL stop the allocation and release any Lease that was
  obtained.
- **SC-024** The Scheduler SHALL record for every Allocation the Job id, the
  Profile name, the MicroVM uid, the Lease id and the Placement.
- **SC-025** The Scheduler SHALL log every allocation with the Job id,
  Profile, Host name, MicroVM uid and elapsed time from claim to Placement.
- **SC-026** The Scheduler SHALL convert a Reservation into an Allocation
  without releasing the Slot in between.

## Placement {#placement}

- **SC-030** When a claim succeeds and the response carries a `host` with a
  name, the Scheduler SHALL record that name as the Placement, and if the
  `host.address` differs from the Inventory entry of that name, then the
  Scheduler SHALL log a warning with both values, count the mismatch and
  continue using the Inventory endpoint.
- **SC-031** When a claim succeeds and the response does not name the Host,
  the Scheduler SHALL resolve the Placement by calling `GetMicroVM` with the
  MicroVM's uid on each Host the Pool Manager reports for that Pool until
  one returns it.
- **SC-032** The Scheduler SHALL cache the Placement of a MicroVM for the
  lifetime of its Lease.
- **SC-033** If Placement cannot be resolved for a MicroVM, then the
  Scheduler SHALL release its Lease and report an allocation error.
- **SC-034** If the Host named in a Placement is not in the Inventory, then
  the Scheduler SHALL release the Lease and report an allocation error
  naming the Host.

SC-031 is a fallback for a Pool Manager older than battery PR #45, which
added `HostInfo{name, address}` to the claim response on 2026-09-07. With a
current Pool Manager SC-030 is the only path taken; the fan-out stays so
that the Runner degrades rather than fails against an older server.

## Host health {#host-health}

- **SC-040** The Scheduler SHALL probe every Host in the Inventory at the
  configured health interval by calling `ServerInfo`, falling back to
  `ListMicroVMs` on the Runner's namespace when `ServerInfo` is not
  implemented by the Host.
- **SC-041** When a Host fails the configured number of consecutive probes,
  the Scheduler SHALL mark the Host unhealthy.
- **SC-042** When an unhealthy Host passes a probe, the Scheduler SHALL mark
  the Host healthy.
- **SC-043** When a Host becomes unhealthy, the Scheduler SHALL abort every
  Job whose MicroVM is placed on that Host with the failure reason
  `runner_system_failure` and release their Leases.
- **SC-044** The Scheduler SHALL NOT refuse Reservations because a Host is
  unhealthy, because the Pool Manager decides where warm MicroVMs live.

Host health is tracked even though the Runner never places MicroVMs,
because the Guest Transport talks to Hosts directly and a Job on a dead Host
has to be failed promptly rather than left to its timeout.

## Release {#release}

- **SC-050** When a Job finishes, the Scheduler SHALL release its Lease with
  the Pool Manager and SHALL NOT delete the MicroVM itself.
- **SC-051** If a release fails, then the Scheduler SHALL retry it with
  exponential backoff up to the configured retry limit and SHALL then log
  the Lease id and rely on Lease expiry.
- **SC-052** The Scheduler SHALL return the Slot of a released MicroVM as
  soon as the release has been requested rather than when it completes.
- **SC-053** When starting, the Scheduler SHALL NOT attempt to find or clean
  up MicroVMs from a previous incarnation, because Leases it no longer
  heartbeats expire and the Pool Manager deletes them.
- **SC-054** Where the keep-on-failure debug option is enabled, the Scheduler
  SHALL keep heartbeating the Lease of a failed Job for the configured keep
  duration before releasing it, and SHALL log the MicroVM uid and Host name.

## Lease keep-alive {#lease-keep-alive}

- **SC-060** While an Allocation holds a Lease, the Scheduler SHALL send a
  heartbeat to the Pool Manager at an interval no longer than half the time
  remaining until the Lease expiry it last received.
- **SC-061** If a heartbeat reports that the Lease no longer exists, then the
  Scheduler SHALL abort the Job with the failure reason
  `runner_system_failure` and drop the Allocation without a release call.
- **SC-062** If heartbeats fail for longer than the Lease expiry, then the
  Scheduler SHALL treat the Lease as lost and abort the Job.
