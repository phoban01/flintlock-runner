// Package flintlock is the Runner's client for flintlock Hosts
// (docs/requirements/05-hosts.md) and the home of the fake Host
// (docs/requirements/10-test-doubles.md#fake-host), which lives in the
// subpackage fake.
//
// The interfaces here are deliberately split by privilege. HostClient is
// everything the Runner is allowed to do to a Host: read a MicroVM, probe the
// Host and open guest-agent streams. HostAdminClient adds CreateMicroVM and
// DeleteMicroVM, which the Runner never calls (HO-007, PL-044) but the fake
// Pool Manager has to (TD-002). Dialer hands out HostClients and AdminDialer
// hands out PoolHostClients, so nothing on the Runner side can obtain a value
// with create or delete on it: HO-007 is a property of the type system
// rather than of code review.
//
// Every method takes a context; the implementation applies the configured
// deadline to unary calls (HO-003) and adds the basic auth header (HO-004).
// Errors from Hosts are mapped onto the sentinel errors in interfaces.go so
// that callers and their tests never inspect gRPC status codes.
//
// The package does not import internal/config. Callers build an Endpoint
// from a config.HostEntry, which keeps the fake Host dialable from a literal
// and this package free of the configuration schema.
package flintlock
