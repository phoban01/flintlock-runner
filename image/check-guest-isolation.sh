#!/usr/bin/env bash
# Runs /usr/libexec/flr/host-config and `network render` against a Host
# configuration file with protected CIDRs, and checks the guest firewall
# rules that keep a MicroVM away from the cluster: the Host's own addresses,
# the Host's other interfaces, and Services behind destination NAT (HI-075
# to HI-077). Installed as /usr/libexec/flr/check-guest-isolation-cases and
# called by the check stage; `image/check-guest-isolation.sh DIR LIBEXEC
# DEFAULTS` runs it outside the image against the sources, which
# `make image-lint` does.
#
# Where this machine has nft, ip, nsenter and unprivileged user and network
# namespaces, the ruleset is also loaded into a stand-in Host, and a stand-in
# guest connects to the outside, to the gateway, to the Host's primary
# address, to a pod on the Host and to a Service that the Host translates.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL forward traffic from the guest subnet only
#/ out of the Host's primary interface, and SHALL drop traffic from the guest
#/ subnet to every other interface of the Host.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet whose
#/ destination before any destination NAT on the Host is in a protected CIDR.
#
#= docs/requirements/11-host-image.md#image-networking
#= type=test
#/ The Host Image SHALL drop traffic from the guest subnet to every
#/ address of the Host other than the bridge gateway address.
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/flr}
defaults=${3:-/usr/share/flr/host.conf.defaults}
C=$work/guest-isolation-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

# The stand-in Host: a guest subnet, a primary address, a pod network on
# the Host and a Service range, of which the node and Service ranges are
# protected and the pod network is not.
gateway=10.200.4.1
guest=10.200.4.10
primary=192.0.2.10
outside=192.0.2.1
pod=10.244.0.2
service=10.96.0.10
printf 'GUEST_SUBNET=10.200.4.0/22\nPROTECTED_CIDRS=10.0.0.0/16,10.96.0.0/12\n' >"$C.conf"

rm -rf "$C"
mkdir -p "$C/run/not-ready.d"
env=(FLR_PATH="$PATH" FLR_LIB="$libexec/lib.sh" FLR_HOST_DEFAULTS="$defaults" FLR_HOST_CONF="$C.conf"
  FLR_RUN="$C/run" FLR_NOT_READY_DIR="$C/run/not-ready.d" FLR_HOST_ENV="$C/run/host.env")
if ! env "${env[@]}" bash "$libexec/host-config" >/dev/null 2>&1 ||
  ! env "${env[@]}" FLR_PRIMARY_INTERFACE=eth0 FLR_PRIMARY_ADDRESS=$primary bash "$libexec/network" render "$C/net" >/dev/null 2>&1; then
  fail "host-config or network render fails for $(tr '\n' ' ' <"$C.conf")"
  rm -rf "$C" "$C.conf"
  exit 1
fi
nftf=$C/net/guest-firewall.nft

# chain NAME prints the rules of a rendered chain, one per line, without
# comments or indentation.
chain() {
  awk -v c="$1" '$0 ~ "^\tchain " c " \\{" { on = 1; next } on && /^\t\}/ { exit } on' "$nftf" |
    sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e '/^$/d'
}

# HI-075: after the drops, a guest's packet leaves by the primary
# interface or not at all.
forward=$(chain forward)
want=$(printf '%s\n' 'iifname "flbr0" oifname "eth0" accept' 'iifname "flbr0" drop')
if printf '%s\n' "$forward" | grep -A1 -xF 'iifname "flbr0" oifname "eth0" accept' | diff -q - <(echo "$want") >/dev/null; then
  ok "the forward chain lets guests out of the primary interface and drops them towards every other"
else
  fail "the forward chain's rules for the bridge are: $(printf '%s\n' "$forward" | grep '^iifname "flbr0"' | tr '\n' ';')"
fi
bridge_accepts=$(printf '%s\n' "$forward" | grep '^iifname "flbr0"' | grep -c 'accept$')
if [ "$bridge_accepts" = 1 ]; then
  ok "the primary interface is the only way out the forward chain accepts for guests"
else
  fail "the forward chain has $bridge_accepts accepts for guests"
fi

# HI-076: the destination the guest asked for, before kube-proxy's DNAT.
original=$(printf '%s\n' "$forward" | grep -n -xF 'iifname "flbr0" ct original ip daddr @protected drop' | cut -d: -f1)
accept=$(printf '%s\n' "$forward" | grep -n -xF 'iifname "flbr0" oifname "eth0" accept' | cut -d: -f1)
if [ -n "$original" ] && [ -n "$accept" ] && [ "$original" -lt "$accept" ]; then
  ok "guest traffic whose original destination is protected is dropped before guests are let out"
else
  fail "the forward chain does not drop the original destination in @protected before its accept"
fi

# HI-077: every rule of the input chain that accepts from the bridge names
# the gateway, or is DHCP's broadcast, and the chain ends with a drop.
input=$(chain input)
stray=$(printf '%s\n' "$input" | grep '^iifname "flbr0".*accept$' |
  grep -vE "ip daddr ($gateway|\\{ 255\\.255\\.255\\.255, $gateway \\}) " || true)
if [ -z "$stray" ]; then
  ok "every input rule that accepts from guests names the gateway, or the broadcast address for DHCP"
else
  fail "input rules accept guests on other addresses: $stray"
fi
if [ "$(printf '%s\n' "$input" | tail -n 1)" = 'iifname "flbr0" drop' ]; then
  ok "the input chain drops everything else from guests"
else
  fail "the input chain's last rule is: $(printf '%s\n' "$input" | tail -n 1)"
fi

# Enforcement, where this machine allows it. A new user and network
# namespace is the Host, with the rendered ruleset, a bridge flbr0 at the
# gateway, eth0 with the primary address, and cni0 to a pod. kube-proxy is
# a DNAT rule that sends the Service to an address outside every protected
# range. The guest, the outside and the pod are network namespaces of their
# own. Nothing listens anywhere, so a connection that arrives is refused
# and one that is dropped times out.
# The inner script prints one line per attempt, NAME reached|dropped, and
# "pod forwarded" when the Host let a guest's packet out to the pod.
# shellcheck disable=SC2016
enforce() {
  unshare --user --map-root-user --net bash -c '
    set -u
    nftf=$1 gateway=$2 guest=$3 primary=$4 outside=$5 pod=$6 service=$7
    ns() { unshare --net sleep 30 >/dev/null 2>&1 & echo $!; }
    inns() { local p=$1; shift; nsenter -t "$p" -n "$@"; }
    {
      g=$(ns) o=$(ns) p=$(ns)
      sleep 0.3
      ip link set lo up &&
      sysctl -q -w net.ipv4.ip_forward=1 &&
      ip link add flbr0 type bridge && ip addr add $gateway/22 dev flbr0 && ip link set flbr0 up &&
      ip link add gh0 type veth peer name g0 && ip link set gh0 master flbr0 && ip link set gh0 up &&
      ip link set g0 netns "$g" &&
      ip link add eth0 type veth peer name o0 && ip addr add $primary/24 dev eth0 && ip link set eth0 up &&
      ip link set o0 netns "$o" &&
      ip link add cni0 type veth peer name p0 && ip addr add 10.244.0.1/24 dev cni0 && ip link set cni0 up &&
      ip link set p0 netns "$p" &&
      inns "$g" sh -c "ip link set lo up && ip addr add $guest/22 dev g0 && ip link set g0 up && ip route add default via $gateway" &&
      inns "$o" sh -c "ip link set lo up && ip addr add $outside/24 dev o0 && ip link set o0 up" &&
      inns "$p" sh -c "ip link set lo up && ip addr add $pod/24 dev p0 && ip link set p0 up && ip route add default via 10.244.0.1" &&
      nft -f "$nftf" &&
      nft -f - <<EOF
table ip probe {
	chain forward {
		type filter hook forward priority filter + 10; policy accept;
		oifname "cni0" counter
	}
}
table ip kube_proxy {
	chain prerouting {
		type nat hook prerouting priority dstnat; policy accept;
		ip daddr $service tcp dport 80 dnat to $outside:80
	}
}
EOF
    } >/dev/null 2>&1 || exit 3
    try() {
      local out
      out=$(inns "$g" timeout 2 bash -c "exec 3<>/dev/tcp/$2/$3" 2>&1)
      case $? in
      124) echo "$1 dropped" ;;
      *) case $out in *refused*) echo "$1 reached" ;; *) echo "$1 unknown" ;; esac ;;
      esac
    }
    try outside $outside 80
    try gateway $gateway 1234
    try primary $primary 22
    try pod $pod 80
    # The pod cannot answer a guest either way, because the Host drops
    # replies to the bridge from anywhere but the primary interface; a
    # chain after the guest firewall counts what got past it.
    n=$(nft list chain ip probe forward | sed -n "s/.*oifname \"cni0\" counter packets \([0-9]*\) .*/\1/p")
    [ "${n:-0}" = 0 ] || echo "pod forwarded"
    try service $service 80
    kill "$g" "$o" "$p" 2>/dev/null
  ' _ "$nftf" $gateway $guest $primary $outside $pod $service 2>/dev/null
}
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1 || ! command -v nsenter >/dev/null 2>&1; then
  printf 'skip  enforcement: nft, ip or nsenter is not installed\n'
else
  results=$(enforce)
  if ! printf '%s\n' "$results" | grep -qx 'outside reached'; then
    printf 'skip  enforcement: no unprivileged user and network namespaces with bridges, veth and nftables here\n'
  else
    result() { printf '%s\n' "$results" | sed -n "s/^$1 //p"; }
    ok "enforced: a guest reaches an address outside the Host through the primary interface"
    if [ "$(result gateway)" = reached ]; then ok "enforced: a guest reaches a Host Service port on the gateway"; else fail "enforced: the gateway's Host Service port is $(result gateway)"; fi
    if [ "$(result primary)" = dropped ]; then ok "enforced: a guest's traffic to the Host's primary address is dropped"; else fail "enforced: the Host's primary address is $(result primary)"; fi
    if [ "$(result pod | xargs)" = dropped ]; then ok "enforced: a guest's traffic to a pod on the Host, outside every protected range, is dropped"; else fail "enforced: a pod on the Host is $(result pod | xargs)"; fi
    if [ "$(result service)" = dropped ]; then ok "enforced: a guest's traffic to a protected Service address is dropped after kube-proxy's DNAT"; else fail "enforced: a protected Service address is $(result service)"; fi
  fi
fi

rm -rf "$C" "$C.conf"
[ "$failures" -eq 0 ]
