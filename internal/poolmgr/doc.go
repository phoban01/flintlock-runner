// Package poolmgr is the Runner's client for the Pool Manager, battery
// (docs/requirements/04-pool-manager.md), and the home of the fake Pool
// Manager (docs/requirements/10-test-doubles.md#fake-pool-manager), which
// lives in the subpackage fake.
//
// Client mirrors the three poolmgr.v1alpha1 services one RPC to one method
// (PL-001) and translates status codes into the sentinel errors in
// interfaces.go. The PL policy that sits above the wire, declaring Pools
// from Profiles (PL-010 to PL-026), tracking capacity from events (PL-050 to
// PL-056) and Pool Manager health (PL-004, PL-005, PL-034, PL-035), is owned
// by this package too, behind the SpecBuilder, HostSelector, Declarer,
// Tracker and Health interfaces, which is the split docs/PLAN.md's work
// packages assume. The Scheduler composes them and tests against in-memory
// implementations of each, with no gRPC.
//
// Messages are represented by plain Go structs so that Scheduler tests build
// them as literals. Enums are aliases of the generated proto enums so that
// the fake, the client and the Scheduler cannot drift from the proto (TD-006).
package poolmgr
