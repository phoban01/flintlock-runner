# Scheduler {#scheduler}

This document specifies the scheduling component. The Scheduler sits between
the gitlab-runner run loop and the Executor. Its inputs are the configured
Profiles, the Inventory of Hosts, the Orchestrator and the Pool Manager. Its
outputs are Reservations (permission to ask GitLab for a Job), Allocations
(a specific MicroVM bound to a Job) and Placements (which Host runs each
MicroVM, needed by the Guest Transport). Every MicroVM a Job runs in comes
from the Pool Manager as a warm MicroVM or, when a Pool is exhausted and the
Profile allows it, is created through the Orchestrator as an Overflow
MicroVM. The Scheduler never selects a Host itself.

## Capacity {#capacity}

- **SC-001** The Scheduler SHALL maintain a count of held Reservations and
  active Allocations and SHALL treat their sum as the number of Slots in use.
- **SC-002** The Scheduler SHALL compute available capacity as the lesser of
  the number of unused Slots and the sum of the number of available warm
  MicroVMs across all Pools plus the unused overflow headroom.
- **SC-003** The Scheduler SHALL compute the unused overflow headroom as the
  configured maximum number of Overflow MicroVMs minus the number of Overflow
  MicroVMs currently allocated or being created.
- **SC-004** When a Reservation is requested and available capacity is
  greater than zero, the Scheduler SHALL grant a Reservation and count it as
  a Slot in use.
- **SC-005** When a Reservation is requested and available capacity is zero,
  the Scheduler SHALL refuse the Reservation.
- **SC-006** When a Reservation is released without an Allocation, the
  Scheduler SHALL return its Slot immediately.
- **SC-007** Where a Profile has a configured maximum concurrency, the
  Scheduler SHALL NOT hold more Allocations for that Profile than the
  maximum.
- **SC-008** The Scheduler SHALL be safe for concurrent use by the number of
  workers the run loop starts.
- **SC-009** While the Orchestrator is unhealthy, the Scheduler SHALL count
  the unused overflow headroom as zero.
- **SC-010** When starting, the Scheduler SHALL NOT grant any Reservation
  until it has successfully contacted both the Pool Manager and the
  Orchestrator at least once.

Capacity is computed from warm MicroVMs plus overflow headroom, rather than
from Slots alone, so that the Runner does not accept a Job from GitLab that
it then cannot start; a Job accepted and failed with a system error costs the
pipeline a retry, whereas a Job left in GitLab's queue costs only latency.

## Profile resolution {#profile-resolution}

- **SC-011** The Scheduler SHALL resolve a Profile for a Job by matching the
  Job Image name against Profile image names exactly, then against Profile
  image glob patterns in configuration order, then by falling back to the
  Default Profile.
- **SC-012** If a Job has no Job Image and no Default Profile is configured,
  then the Scheduler SHALL report that no Profile can be resolved.
- **SC-013** If a Job Image matches no Profile and the configuration does not
  allow falling back to the Default Profile for unknown images, then the
  Scheduler SHALL report that no Profile can be resolved.
- **SC-014** The Scheduler SHALL resolve the Profile before requesting any
  MicroVM so that a Job with an unknown image fails without consuming a warm
  MicroVM.

## Allocation {#allocation}

- **SC-020** When an Allocation is requested for a Profile, the Scheduler
  SHALL first attempt to claim a warm MicroVM from the Profile's Pool.
- **SC-021** If the Pool has no warm MicroVM available and the Profile's
  exhaustion policy is `overflow`, then the Scheduler SHALL create an Overflow
  MicroVM for the Job through the Orchestrator.
- **SC-022** If the Pool has no warm MicroVM available and the Profile's
  exhaustion policy is `wait`, then the Scheduler SHALL retry the claim with
  exponential backoff until the allocation timeout elapses.
- **SC-023** If an Overflow MicroVM is needed and the unused overflow
  headroom is zero, then the Scheduler SHALL keep retrying the claim and
  re-checking the headroom with exponential backoff until one becomes
  available or the allocation timeout elapses.
- **SC-024** If the allocation timeout elapses before a MicroVM is obtained,
  then the Scheduler SHALL return an allocation error naming the Profile, the
  Pool and whether overflow was attempted.
- **SC-025** When the Job's context is cancelled during allocation, the
  Scheduler SHALL stop the allocation and release any MicroVM that was
  obtained.
- **SC-026** The Scheduler SHALL record for every Allocation the Job id, the
  Profile name, the source (`warm` or `overflow`), the MicroVM uid, the Lease
  id where applicable and the Placement.
- **SC-027** The Scheduler SHALL log every allocation decision with the Job
  id, Profile, source, Host name, MicroVM uid and elapsed time.
- **SC-028** The Scheduler SHALL convert a Reservation into an Allocation
  without releasing the Slot in between.

Warm MicroVMs are the normal path; overflow exists so that a burst larger
than a Pool does not stall a pipeline on Pool replenishment. Both paths
end with a MicroVM placed by the Orchestrator, either because the Pool
Manager created it there or because the Scheduler did.

## Placement {#placement}

- **SC-030** The Scheduler SHALL send every `CreateMicroVM` request to the
  Orchestrator and SHALL NOT select a Host for a MicroVM itself.
- **SC-031** The Scheduler SHALL express a Profile's architecture and Host
  selector as Orchestrator scheduling labels on every `CreateMicroVM` request
  it sends for that Profile.
- **SC-032** When a MicroVM is obtained, the Scheduler SHALL resolve its
  Placement by calling `GetMicroVM` with the MicroVM's uid on each candidate
  Host until one returns it.
- **SC-033** When resolving the Placement of a warm MicroVM, the Scheduler
  SHALL limit the candidate Hosts to those the Pool Manager reports for the
  MicroVM's Pool.
- **SC-034** When resolving the Placement of an Overflow MicroVM, the
  Scheduler SHALL limit the candidate Hosts to those in the Inventory whose
  labels and architecture satisfy the Profile.
- **SC-035** The Scheduler SHALL cache the Placement of a MicroVM for its
  lifetime.
- **SC-036** If Placement cannot be resolved for a MicroVM, then the
  Scheduler SHALL release the MicroVM and report an allocation error.

Placement has to be resolved explicitly because neither the Orchestrator's
create response nor the Pool Manager's claim response names the Host, and
the Guest Transport talks to the Host directly. The fan-out in SC-032 is
bounded by the Inventory size and runs once per Job; if the Orchestrator
later exposes the placement it chose, that lookup replaces the fan-out.

## Host and Orchestrator health {#host-and-orchestrator-health}

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
  `runner_system_failure`.
- **SC-044** The Scheduler SHALL probe the Orchestrator at the configured
  health interval by calling `ListMicroVMs` on the Runner's namespace.
- **SC-045** When the Orchestrator fails the configured number of
  consecutive probes, the Scheduler SHALL mark the Orchestrator unhealthy,
  and when it passes a probe again the Scheduler SHALL mark it healthy.
- **SC-046** The Scheduler SHALL NOT abort running Jobs because the
  Orchestrator became unhealthy.

Host health is tracked even though the Runner never places MicroVMs itself,
because the Guest Transport and Placement resolution talk to Hosts directly
and a Job on a dead Host has to be failed promptly rather than left to its
timeout.

## Release and garbage collection {#release-and-garbage-collection}

- **SC-050** When an Overflow MicroVM is released, the Scheduler SHALL delete
  it with `DeleteMicroVM` through the Orchestrator.
- **SC-051** When a warm MicroVM is released, the Scheduler SHALL release its
  Lease with the Pool Manager and SHALL NOT delete the MicroVM itself.
- **SC-052** If a release operation fails, then the Scheduler SHALL retry it
  with exponential backoff up to the configured retry limit and SHALL record
  the MicroVM as pending cleanup on exhaustion.
- **SC-053** The Scheduler SHALL return the Slot of a released MicroVM as
  soon as the release has been requested rather than when it completes.
- **SC-054** The Scheduler SHALL run a garbage collection pass at the
  configured interval that lists the MicroVMs in the Runner's namespace
  through the Orchestrator.
- **SC-055** When a garbage collection pass finds a MicroVM labelled with
  this Runner's identity that is not an active Allocation and is older than
  the configured orphan grace period, the Scheduler SHALL delete it through
  the Orchestrator.
- **SC-056** When a garbage collection pass finds a MicroVM labelled with
  this Runner's identity whose `created-at` label is older than the
  configured maximum MicroVM lifetime, the Scheduler SHALL delete it even if
  it is an active Allocation and SHALL abort the associated Job.
- **SC-057** When starting, the Scheduler SHALL run a garbage collection pass
  before granting any Reservation.
- **SC-058** The Scheduler SHALL NOT delete a MicroVM that is not labelled
  with this Runner's identity.
- **SC-059** Where the keep-on-failure debug option is enabled, the Scheduler
  SHALL retain an Overflow MicroVM marked for retention until the maximum
  MicroVM lifetime elapses and SHALL then delete it.

Garbage collection only touches MicroVMs carrying the Runner's own identity
label: warm MicroVMs belong to the Pool Manager, which deletes them on
release or Lease expiry, and several Runners can share one Orchestrator and
one namespace without destroying each other's work.

## Lease keep-alive {#lease-keep-alive}

- **SC-060** While an Allocation holds a Lease, the Scheduler SHALL send a
  heartbeat to the Pool Manager at an interval no longer than half the time
  remaining until the Lease expiry it last received.
- **SC-061** If a heartbeat reports that the Lease no longer exists, then the
  Scheduler SHALL abort the Job with the failure reason
  `runner_system_failure` and drop the Allocation without a release call.
- **SC-062** If heartbeats fail for longer than the Lease expiry, then the
  Scheduler SHALL treat the Lease as lost and abort the Job.
