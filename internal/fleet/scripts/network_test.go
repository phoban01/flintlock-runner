package scripts

import (
	"testing"

	"github.com/phoban01/flintlock-runner/internal/fleet"
)

// chains returns the input and forward chains of the rendered guest
// firewall.
func chains(t *testing.T, s string) (input, forward string) {
	t.Helper()
	return section(t, s, "chain input {", "\n\t}"), section(t, s, "chain forward {", "\n\t}")
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL create a Linux bridge on each Host
//# with the configured private guest subnet and SHALL configure `flintlockd`
//# to attach TAP interfaces to it.

func TestNetworkingCreatesBridgeOnGuestSubnet(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Fleet.GuestSubnet = "10.200.0.0/20"
	s := render(t, fleet.StepNetworking, in)
	mustContain(t, s,
		"ip link add flbr0 type bridge",
		"ExecStart=/usr/sbin/ip addr replace 10.200.0.1/20 dev flbr0",
		"ExecStart=/usr/sbin/ip link set flbr0 up",
		"WantedBy=multi-user.target",
		`ensure_service flintlock-runner-network "$reload_net"`,
	)
	f := render(t, fleet.StepFlintlockd, in)
	mustContain(t, f, "echo 'bridge-name: flbr0'")
	fl := render(t, fleet.StepFlintlock, in)
	mustContain(t, fl, "--bridge 'flbr0'")
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL install and configure a DHCP and DNS
//# service bound to the bridge so that guests obtain an address, gateway and
//# resolver without static configuration.

func TestNetworkingServesDHCPAndDNSOnBridge(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	conf := section(t, s, "echo 'interface=flbr0'", "} | write_file /etc/dnsmasq.d/flintlock-runner.conf 0644")
	mustContain(t, conf,
		"echo 'interface=flbr0'",
		"echo 'bind-dynamic'",
		"echo 'dhcp-range=172.31.0.10,172.31.255.254,255.255.0.0,12h'",
		"echo 'dhcp-option=option:router,172.31.0.1'",
		"echo 'dhcp-option=option:dns-server,172.31.0.1'",
	)
	mustContain(t, s, "ensure_packages nftables dnsmasq", `ensure_service dnsmasq "$reload_dns"`)
	input, _ := chains(t, s)
	mustContain(t, input, `iifname "flbr0" udp dport 67 accept`, `iifname "flbr0" ip daddr 172.31.0.1 udp dport 53 accept`)
	// The DHCP and DNS accepts come before the final drop.
	before(t, input, "udp dport 53 accept", "iifname \"flbr0\" drop")
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL install and configure a DHCP and DNS
//# service bound to the bridge so that guests obtain an address, gateway and
//# resolver without static configuration.

func TestNetworkingPointsDnsmasqAtTheRealResolverOnSystemdResolved(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	conf := section(t, s, "echo 'interface=flbr0'", "} | write_file /etc/dnsmasq.d/flintlock-runner.conf 0644")
	// systemd-resolved leaves /etc/resolv.conf pointing at its own loopback
	// stub, which dnsmasq refuses to use as an upstream server - guests get
	// DHCP fine but every DNS query is REFUSED. dnsmasq needs the real
	// upstream file instead, and only when it actually exists, so a Host
	// without systemd-resolved still gets dnsmasq's own default behavior.
	mustContain(t, conf,
		"if [ -e /run/systemd/resolve/resolv.conf ]; then",
		"echo 'resolv-file=/run/systemd/resolve/resolv.conf'",
	)
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL enable IPv4 forwarding persistently
//# on each Host.

func TestNetworkingEnablesForwardingPersistently(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	persist := section(t, s, "write_file /etc/sysctl.d/90-flintlock-runner.conf", "FLR_EOF\nthen")
	mustContain(t, persist, "net.ipv4.ip_forward = 1")
	mustContain(t, s, "sysctl -q -p /etc/sysctl.d/90-flintlock-runner.conf")
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL configure source NAT from the guest
//# subnet to the Host's primary interface so that guests have outbound
//# connectivity.

func TestNetworkingNATsGuestSubnetOutOfPrimaryInterface(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	mustContain(t, s,
		`primary=$(ip -o -4 route show default`,
		"type nat hook postrouting priority srcnat",
		`ip saddr 172.31.0.0/16 oifname "$primary" masquerade`,
	)
	_, forward := chains(t, s)
	mustContain(t, forward, `iifname "flbr0" oifname "$primary" accept`)
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the EC2 instance metadata service address.

//= docs/requirements/09-security.md#guest-egress
//= type=test
//# The Fleet Controller SHALL ensure that guests cannot reach the
//# EC2 instance metadata service, so that Jobs cannot obtain the Host's
//# instance role credentials.

func TestNetworkingDropsInstanceMetadata(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	_, forward := chains(t, s)
	mustContain(t, forward, `iifname "flbr0" ip daddr 169.254.169.254 drop`, `iifname "flbr0" ip6 daddr fd00:ec2::254 drop`)
	// The drop precedes the accept towards the primary interface.
	before(t, forward, "169.254.169.254 drop", `oifname "$primary" accept`)
	// Builds that jobs start in buildkitd share the Host's network, so the
	// buildkit user is kept off the metadata service too.
	hs := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, hs, `meta skuid "buildkit" ip daddr 169.254.169.254 drop`)
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the Host's own `flintlockd`, Pool Manager agent
//# and metrics ports.

func TestNetworkingDropsOwnControlPorts(t *testing.T) {
	t.Parallel()
	in := fullInput()
	in.Fleet.Flintlockd.Port = 9443
	s := render(t, fleet.StepNetworking, in)
	input, _ := chains(t, s)
	mustContain(t, input, `iifname "flbr0" tcp dport { 9443, 9091, 8090 } drop`)
}

//= docs/requirements/06-fleet.md#host-networking
//= type=test
//# The Fleet Controller SHALL configure each Host to drop traffic
//# from the guest subnet to the private addresses of other Hosts in the
//# Inventory.

func TestNetworkingDropsOtherHosts(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	// The Host itself (10.0.1.10) is not its own peer.
	mustContain(t, s, "elements = { 10.0.1.11, 10.0.2.12 }")
	_, forward := chains(t, s)
	mustContain(t, forward, `iifname "flbr0" ip daddr @peers drop`)
	before(t, forward, "@peers drop", `oifname "$primary" accept`)
}

//= docs/requirements/09-security.md#guest-egress
//= type=test
//# The Fleet Controller SHALL ensure that guests cannot reach the
//# control ports of their own Host or of any other Host.

func TestGuestsCannotReachAnyHostControlPort(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	input, forward := chains(t, s)
	// Own Host: the control ports are dropped and so is everything else
	// guests send to the Host that is not a Host Service, DHCP or DNS.
	mustContain(t, input, "tcp dport { 9090, 9091, 8090 } drop", "\t\tiifname \"flbr0\" drop")
	// Other Hosts: every address in the Inventory is dropped.
	mustContain(t, forward, "ip daddr @peers drop")
	mustContain(t, s, "elements = { 10.0.1.11, 10.0.2.12 }")
	hs := render(t, fleet.StepHostServices, fullInput())
	mustContain(t, hs, `meta skuid "buildkit" tcp dport { 9090, 9091, 8090 } drop`,
		`meta skuid "buildkit" ip daddr { 10.0.1.11, 10.0.2.12 } drop`)
}

//= docs/requirements/09-security.md#guest-egress
//= type=test
//# Where an egress allow-list is configured, the Fleet Controller
//# SHALL restrict guest outbound traffic to the listed destinations.

func TestNetworkingRestrictsEgressToAllowList(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, sparseInput())
	mustContain(t, s, "elements = { 10.20.0.0/16, 192.0.2.10 }")
	_, forward := chains(t, s)
	mustContain(t, forward, `iifname "flbr0" ip daddr != @egress_allow drop`)
	before(t, forward, "!= @egress_allow drop", `oifname "$primary" accept`)

	open := render(t, fleet.StepNetworking, fullInput())
	mustNotContain(t, open, "egress_allow")

	in := sparseInput()
	in.Fleet.EgressAllowList = []string{"example.com"}
	sc, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Render(fleet.StepNetworking, in); err == nil {
		t.Error("rendered an allow-list entry that is not an address")
	}
}

//= docs/requirements/06-fleet.md#host-services
//= type=test
//# The Fleet Controller SHALL add guest firewall rules that allow
//# traffic from the guest subnet to the bridge gateway address only on the
//# Host Service ports and the ports of the DHCP and DNS service bound to the
//# bridge, and SHALL keep every other port on the gateway closed to guests.

func TestFirewallOpensOnlyHostServicePortsOnGateway(t *testing.T) {
	t.Parallel()
	s := render(t, fleet.StepNetworking, fullInput())
	input, _ := chains(t, s)
	mustContain(t, input, `iifname "flbr0" ip daddr 172.31.0.1 tcp dport { 1234, 3000, 5000, 3128 } accept`)
	// The chain ends by dropping whatever else guests send to the Host.
	before(t, input, "tcp dport { 1234, 3000, 5000, 3128 } accept", "\t\tiifname \"flbr0\" drop")
	if n := countAccepts(input); n != 4 {
		t.Errorf("input chain has %d accept rules, want DHCP, DNS/udp, DNS/tcp and the Host Services", n)
	}
}

func countAccepts(chain string) int {
	n := 0
	for _, l := range splitLines(chain) {
		if len(l) > 7 && l[len(l)-7:] == " accept" {
			n++
		}
	}
	return n
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
