package scripts

// The template behind each Step. The requirement each script implements is
// cited here, on the constant naming its template; the template carries a
// comment with the identifier at the place that implements it.

// detectTemplate reports installed versions and the thin pool (FL-029,
// FL-030).
const detectTemplate = "detect.sh.tmpl"

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL create the containerd devicemapper
//# thin pool on the block device named in the configuration.

//= docs/requirements/06-fleet.md#host-provisioning
//# If the configured thin pool already exists on an instance, then
//# the Fleet Controller SHALL NOT recreate it or wipe its device.

// thinPoolTemplate creates the thin pool on a blank device and leaves an
// existing one alone.
const thinPoolTemplate = "thin_pool.sh.tmpl"

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL provision each instance by running
//# the flintlock host provisioner in unattended mode, installing containerd,
//# Firecracker, Cloud Hypervisor and `flintlockd` at the versions pinned in the
//# configuration.

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL use the `flintlock-provision` binary
//# when the pinned flintlock release ships it and SHALL fall back to the
//# `provision.sh` script from the same release otherwise.

// flintlockTemplate runs the flintlock host provisioner.
const flintlockTemplate = "flintlock.sh.tmpl"

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL create a Linux bridge on each Host
//# with the configured private guest subnet and SHALL configure `flintlockd`
//# to attach TAP interfaces to it.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL install and configure a DHCP and DNS
//# service bound to the bridge so that guests obtain an address, gateway and
//# resolver without static configuration.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL enable IPv4 forwarding persistently
//# on each Host.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL configure source NAT from the guest
//# subnet to the Host's primary interface so that guests have outbound
//# connectivity.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the EC2 instance metadata service address.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the Host's own `flintlockd`, Pool Manager agent
//# and metrics ports.

//= docs/requirements/06-fleet.md#host-networking
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the private addresses of other Hosts in the
//# Inventory.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL add guest firewall rules that allow
//# traffic from the guest subnet to the bridge gateway address only on the
//# Host Service ports and SHALL keep every other port on the gateway closed
//# to guests.

//= docs/requirements/09-security.md#guest-egress
//# The Fleet Controller SHALL ensure that guests cannot reach the
//# EC2 instance metadata service, so that Jobs cannot obtain the Host's
//# instance role credentials.

//= docs/requirements/09-security.md#guest-egress
//# The Fleet Controller SHALL ensure that guests cannot reach the
//# control ports of their own Host or of any other Host.

//= docs/requirements/09-security.md#guest-egress
//# Where an egress allow-list is configured, the Fleet Controller
//# SHALL restrict guest outbound traffic to the listed destinations.

// networkingTemplate sets up the guest bridge, DHCP and DNS, forwarding,
// NAT and the guest firewall.
const networkingTemplate = "networking.sh.tmpl"

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL configure `flintlockd` to listen on
//# the instance's private address on the configured port, to require the
//# configured basic auth token and to serve TLS with the configured
//# certificates.

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL NOT start `flintlockd` in insecure
//# mode unless the configuration explicitly requests it.

// flintlockdTemplate writes flintlockd's configuration and runs it; the
// configuration itself is in flintlockd_lib.sh.tmpl, shared with the
// flintlock step so that the provisioner never starts flintlockd without it.
const flintlockdTemplate = "flintlockd.sh.tmpl"

//= docs/requirements/06-fleet.md#pool-manager-install
//# The Fleet Controller SHALL install the pinned Pool Manager host
//# agent on every Host as a systemd service.

// poolAgentTemplate installs the Pool Manager host agent.
const poolAgentTemplate = "pool_agent.sh.tmpl"

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL install on every Host a `buildkitd`
//# service, a Go module proxy service, a pull-through container registry
//# mirror service and an HTTP cache service for the configured upstreams,
//# collectively the Host Services.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL bind every Host Service only to the
//# guest bridge gateway address so that it is reachable from guests on that
//# Host and from nothing else.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL run `buildkitd` in rootless mode
//# with the overlayfs snapshotter and SHALL configure its garbage collection
//# with the storage limit from the configuration.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure `buildkitd` to resolve
//# images through the Host's registry mirror before reaching upstream
//# registries.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure the registry mirror as a
//# pull-through cache for each upstream registry named in the configuration,
//# with credentials for upstreams that need them supplied from Systems
//# Manager parameters.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure the Go module proxy to
//# store modules on the Host's cache volume and to fetch modules it does not
//# have from the public proxy for public paths and from the configured
//# version control host for the configured private module patterns.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL supply the Go module proxy with a
//# read-only credential for the private module patterns, read from the
//# configured Systems Manager parameter and held on the Host side only.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure the Go module proxy to
//# skip checksum database verification for the private module patterns and
//# to verify every other module against the public checksum database.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure the Go module proxy to
//# serve private modules from its cache without contacting the version
//# control host again until the configured private module revalidation
//# interval elapses.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL configure the HTTP cache with one
//# cache path per configured upstream, each with its own size limit and
//# time-to-live.

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL place all Host Service storage on a
//# dedicated cache volume or directory whose total size is capped by the
//# configuration.

//= docs/requirements/06-fleet.md#host-services
//# Where a Host Service is disabled in the configuration, the Fleet
//# Controller SHALL NOT install it and SHALL NOT open its port.

//= docs/requirements/09-security.md#host-services-security
//# The Fleet Controller SHALL run `buildkitd` as an unprivileged
//# user so that a build escaping its sandbox does not gain root on the Host.

//= docs/requirements/09-security.md#host-services-security
//# Every Host Service SHALL be reachable only from the guest
//# subnet of its own Host and from the Host itself, and SHALL NOT be
//# reachable from other Hosts or from outside the Host.

//= docs/requirements/09-security.md#host-services-security
//# The Fleet Controller SHALL NOT place upstream registry or proxy
//# credentials where a guest can read them, and SHALL configure the registry
//# mirror and Go module proxy to hold them on the Host side only.

//= docs/requirements/09-security.md#host-services-security
//# The credential the Go module proxy uses for private modules
//# SHALL be read-only and SHALL grant access to no groups or projects beyond
//# those covered by the configured private module patterns.

//= docs/requirements/09-security.md#host-services-security
//# The Go module proxy SHALL NOT use a Job's token to fetch
//# modules.

// hostServicesTemplate installs the Host Services and their cache volume.
const hostServicesTemplate = "host_services.sh.tmpl"

//= docs/requirements/06-fleet.md#host-provisioning
//# The Fleet Controller SHALL pre-pull the kernel and root
//# filesystem images of every Profile into the flintlock containerd namespace
//# on each Host after provisioning.

// prepullTemplate pulls every Profile image.
const prepullTemplate = "prepull.sh.tmpl"

//= docs/requirements/06-fleet.md#host-services
//# The Fleet Controller SHALL pre-warm the Go module proxy and the
//# registry mirror on each Host with the modules and images listed in the
//# configuration after provisioning.

// prewarmTemplate fetches the configured modules and images through the
// Host Services.
const prewarmTemplate = "prewarm.sh.tmpl"

// verifyActiveTemplate checks every installed unit is active (FL-053).
const verifyActiveTemplate = "verify_active.sh.tmpl"

//= docs/requirements/06-fleet.md#pool-manager-install
//# The Fleet Controller SHALL name each Host identically in the
//# Pool Manager's host list and in the Inventory, because the Runner joins
//# the two by name when resolving Placement.

// controlNodeTemplate installs the Pool Manager daemon with a host list
// generated from the Inventory entries, by their names (FL-051, FL-055).
const controlNodeTemplate = "control_node.sh.tmpl"

// drainTemplate, teardownTemplate and userDataTemplate serve `fleet drain`,
// `fleet teardown` and `fleet emit-userdata`.
const (
	drainTemplate    = "drain.sh.tmpl"
	teardownTemplate = "teardown.sh.tmpl"
	userDataTemplate = "user_data.sh.tmpl"
)

//= docs/requirements/06-fleet.md#host-services
//# The verification command SHALL, from inside a verification
//# MicroVM on each Host, build a trivial image with `buildctl`, fetch a
//# module through the Go module proxy, pull an image through the registry
//# mirror and fetch one object through the HTTP cache, and SHALL report the
//# outcome per service per Host.

// guestVerifyTemplate exercises each Host Service from inside a guest and
// reports one line per service, which ParseOutput reads.
const guestVerifyTemplate = "guest_verify.sh.tmpl"

//= docs/requirements/02-executor.md#guest-transport
//= type=todo
//= tracking-issue=8
//# A Profile's root filesystem image SHALL carry a guest agent
//# that emits liveness heartbeats on the exec control channel, because
//# `flintlockd` ends an exec session whose control channel has been idle
//# for longer than its deadline.

// EX-052 is not checkable from the Host: the heartbeat is exchanged between
// flintlockd and the guest agent on the vsock control channel, the exec
// API (flintlock api 12b8cade) exposes no agent version or capability, and
// neither flintlock v0.14.0 nor the guest-agent project publishes a version
// floor or an image label that marks a heartbeat-capable agent. The prepull
// step therefore cannot tell a heartbeat-emitting agent from an old one.
