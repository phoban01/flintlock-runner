# Hosts {#hosts}

This document specifies how the Runner talks to individual flintlock Hosts.
The Runner does not create, delete or list MicroVMs on Hosts; the Pool
Manager owns that. The Runner reaches a Host for two things only: running
commands in a guest through `MicroVMExec` or `MicroVMSSHProxy`, and reading
a MicroVM's status with `GetMicroVM` when it has to resolve which Host
holds a leased MicroVM.

## Client {#flintlock-client}

- **HO-001** The Runner SHALL communicate with Hosts through the flintlock
  `microvm.services.api.v1alpha1`, `microvmexec.services.api.v1alpha1` and
  `microvmsshproxy.services.api.v1alpha1` gRPC APIs using the generated
  clients from the flintlock `api` module.
- **HO-002** The Runner SHALL maintain one long-lived gRPC connection per
  Host, with keepalive enabled, and SHALL reconnect with exponential backoff
  when a connection is lost.
- **HO-003** The Runner SHALL apply the configured deadline to every unary
  flintlock call.
- **HO-004** Where a Host is configured with a basic auth token, the Runner
  SHALL send an `authorization` header with the value `Basic` followed by the
  base64 encoding of the token on every call to that Host.
- **HO-005** The Runner SHALL connect to Hosts with TLS, verifying the server
  certificate against the configured certificate authority, unless the Host
  is explicitly marked insecure.
- **HO-006** Where client certificate and key files are configured for a
  Host, the Runner SHALL present them for mutual TLS.
- **HO-007** The Runner SHALL NOT call `CreateMicroVM` or `DeleteMicroVM` on
  any Host.
- **HO-008** The Runner SHALL only call `ListMicroVMs` with its own namespace
  and only as a health probe fallback.

## Inventory {#inventory}

- **HO-010** The Runner SHALL load the set of Hosts from the Inventory
  section of its configuration, keyed by the Host names the Pool Manager
  uses in `flintlock_hosts`.
- **HO-011** When starting, the Runner SHALL call `ServerInfo` on every Host
  and SHALL log the Host's flintlock version and whether the exec and SSH
  proxy services are enabled.
- **HO-012** If a Host reports that the exec service is disabled, then the
  Runner SHALL log a warning naming the Host, because Jobs the Pool Manager
  places there cannot be run over the `exec` Guest Transport.
- **HO-013** If `ServerInfo` is not implemented by a Host, then the Runner
  SHALL treat the Host's version as unknown and continue.
- **HO-014** When the configuration is reloaded, the Runner SHALL add new
  Hosts, stop probing removed Hosts and keep serving Jobs on removed Hosts
  until they finish.
- **HO-015** When starting, the Runner SHALL compare the Inventory with the
  Hosts the Pool Manager reports for each Pool and SHALL log a warning for
  any Pool Host missing from the Inventory, because a MicroVM placed there
  could not be reached.
