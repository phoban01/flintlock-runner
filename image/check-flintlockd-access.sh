#!/usr/bin/env bash
# Runs /usr/libexec/flr/host-config and `network render` against Host
# configuration files that set, omit and mistype the Exec Agent's user id and
# the Operator's pod network, and checks where flintlockd serves and the
# firewall rules that decide who may connect to it (HI-067, HI-069,
# HI-070). Installed as /usr/libexec/flr/check-flintlockd-access-cases and
# called by the check stage; `image/check-flintlockd-access.sh DIR LIBEXEC
# DEFAULTS UNIT` runs it outside the image against the sources, which
# `make image-lint` does.
#
# What this shows is the rules the Host would load. Where this machine has
# nft, ip and unprivileged user and network namespaces, both chains are
# also loaded and connections are made as chosen users and from chosen
# addresses in a second network namespace.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL refuse connections to `flintlockd`'s port
#/ from the Host's own processes unless they belong to the Exec Agent's user
#/ id, which it reads from the Host configuration file with a default when
#/ none is set.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL drop every connection to `flintlockd`'s
#/ port that arrives from outside the Host unless its source address is in
#/ the Operator's pod network, which it reads from the Host configuration
#/ file.
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/flr}
defaults=${3:-/usr/share/flr/host.conf.defaults}
unit=${4:-/usr/lib/systemd/system/flintlockd.service}
C=$work/flintlockd-access-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}

# render CONF: host-config with CONF as the Host configuration file, then
# the firewall into $C/net. Fails when host-config refuses CONF.
render() {
  rm -rf "$C"
  mkdir -p "$C/run/not-ready.d"
  local env=(FLR_PATH="$PATH" FLR_LIB="$libexec/lib.sh" FLR_HOST_DEFAULTS="$defaults" FLR_HOST_CONF="$1"
    FLR_RUN="$C/run" FLR_NOT_READY_DIR="$C/run/not-ready.d" FLR_HOST_ENV="$C/run/host.env")
  env "${env[@]}" bash "$libexec/host-config" >/dev/null 2>&1 || return 1
  env "${env[@]}" FLR_PRIMARY_INTERFACE=eth0 FLR_PRIMARY_ADDRESS=192.0.2.10 bash "$libexec/network" render "$C/net" >/dev/null 2>&1
}
# chain NAME prints the rules of a rendered chain, one per line, without
# comments or indentation.
chain() {
  awk -v c="$1" '$0 ~ "^\tchain " c " \\{" { on = 1; next } on && /^\t\}/ { exit } on' "$C/net/guest-firewall.nft" |
    sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e '/^$/d'
}
# output_chain is the output chain without the rules for the Host Services'
# user ids, which check-host-service-egress.sh checks (HI-064).
output_chain() { chain output | grep -v '^meta skuid {'; }
# client_rules prints the input chain's rules for flintlockd's port.
client_rules() { chain input | grep -F "tcp dport $port "; }
# client_set prints the elements line of the flintlockd_clients set, or
# nothing when it has none.
client_set() {
  awk '/^\tset flintlockd_clients \{/ { on = 1; next } on && /^\t\}/ { exit } on && /elements/' "$C/net/guest-firewall.nft" |
    sed -e 's/^[[:space:]]*//'
}
rule_for() { printf 'oifname "lo" tcp dport %s meta skuid != %s counter reject with tcp reset' "$1" "$2"; }

port=$(sed -n 's/^FLR_FLINTLOCKD_PORT=//p' "$libexec/lib.sh")

#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL configure `flintlockd` to serve its gRPC
#/ API only with TLS, on port 9090 of the Host's internal address
# flintlockd listens where flr-network says: the primary interface's address
# and the port the rules guard, which is the one battery-operator's Exec
# Agent is given.
if grep -Eq -- '--grpc-endpoint \$\{FLR_FLINTLOCKD_ENDPOINT\} ' "$unit" && grep -qx 'EnvironmentFile=/run/flr/flintlockd.env' "$unit"; then
  ok "flintlockd's endpoint is FLR_FLINTLOCKD_ENDPOINT from /run/flr/flintlockd.env"
else
  fail "flintlockd.service does not take its endpoint from /run/flr/flintlockd.env"
fi
if [ "$port" = 9090 ]; then ok "flintlockd's port is 9090, the Exec Agent's --flintlockd=\$(HOST_IP):9090"; else fail "flintlockd's port is '$port'"; fi

# The defaults, with no Host configuration file at all.
default_uid=$(sed -n 's/^EXEC_AGENT_UID=//p' "$defaults")
if [ "$default_uid" = 65532 ]; then
  ok "the default Exec Agent user id is 65532, the user of battery-operator's Exec Agent image"
else
  fail "the default Exec Agent user id is '$default_uid', not 65532"
fi
if render "$C.absent.conf"; then
  if grep -qx "FLR_FLINTLOCKD_ENDPOINT=192.0.2.10:$port" "$C/net/flintlockd.env"; then
    ok "flintlockd serves the primary interface's address, 192.0.2.10:$port"
  else
    fail "flintlockd.env is: $(cat "$C/net/flintlockd.env")"
  fi
  if [ "$(output_chain)" = "$(printf 'type filter hook output priority filter - 10; policy accept;\n%s' "$(rule_for "$port" 65532)")" ]; then
    ok "without a Host configuration file only user id 65532 reaches flintlockd from the Host; root and everyone else are reset"
  else
    fail "the output chain for the default user id is: $(output_chain)"
  fi
  if grep -qx 'FLR_EXEC_AGENT_UID=65532' "$C/run/host.env"; then ok "host.env carries the default user id"; else fail "host.env lacks FLR_EXEC_AGENT_UID=65532"; fi
  want=$(printf '%s\n' "iifname != { \"lo\", \"flbr0\" } tcp dport $port ip saddr @flintlockd_clients counter accept" \
    "iifname != { \"lo\", \"flbr0\" } tcp dport $port counter drop")
  if [ "$(client_rules)" = "$want" ]; then
    ok "connections to flintlockd from off the Host are accepted from the Operator's pod network and dropped from everywhere else"
  else
    fail "the input chain's rules for flintlockd's port are: $(client_rules)"
  fi
  if [ -z "$(client_set)" ]; then
    ok "without a Host configuration file the Operator's pod network is empty, so nothing off the Host reaches flintlockd"
  else
    fail "the default flintlockd_clients set is: $(client_set)"
  fi
  first=$(chain input | sed -n 2p)
  if [ "$first" = "${want%%$'\n'*}" ]; then
    ok "the flintlockd rules come first in the input chain, before anything accepts"
  else
    fail "the input chain's first rule is: $first"
  fi
  if grep -qx 'FLR_FLINTLOCKD_CLIENT_CIDRS=' "$C/run/host.env"; then ok "host.env carries the empty default"; else fail "host.env lacks FLR_FLINTLOCKD_CLIENT_CIDRS="; fi
else
  fail "host-config or network render fails without a Host configuration file"
fi

# Configured values replace the defaults, whatever else the file sets.
printf 'GUEST_SUBNET=10.200.4.0/22\nEXEC_AGENT_UID = "4242"\nFLINTLOCKD_CLIENT_CIDRS="10.244.0.0/16, 10.96.8.0/24"\n' >"$C.conf"
if render "$C.conf"; then
  chain=$(output_chain)
  if printf '%s\n' "$chain" | grep -qxF "$(rule_for "$port" 4242)" && ! printf '%s\n' "$chain" | grep -q 65532; then
    ok "a configured EXEC_AGENT_UID=4242 is the only user id that reaches flintlockd from the Host"
  else
    fail "the output chain for EXEC_AGENT_UID=4242 is: $chain"
  fi
  if [ "$(client_set)" = 'elements = { 10.244.0.0/16, 10.96.8.0/24 }' ]; then
    ok "a configured FLINTLOCKD_CLIENT_CIDRS is the Operator's pod network flintlockd admits"
  else
    fail "the flintlockd_clients set for FLINTLOCKD_CLIENT_CIDRS=10.244.0.0/16,10.96.8.0/24 is: $(client_set)"
  fi
  if grep -qx 'FLR_FLINTLOCKD_CLIENT_CIDRS=10.244.0.0/16,10.96.8.0/24' "$C/run/host.env"; then
    ok "host.env carries the configured CIDRs"
  else
    fail "host.env's FLR_FLINTLOCKD_CLIENT_CIDRS is: $(grep FLINTLOCKD_CLIENT "$C/run/host.env")"
  fi
else
  fail "host-config refuses EXEC_AGENT_UID=4242 with FLINTLOCKD_CLIENT_CIDRS=10.244.0.0/16,10.96.8.0/24"
fi

# Root is never the Exec Agent, and a value that is not a user id stops the
# Host rather than opening flintlockd to everyone. The command substitution
# is a literal value: the file is parsed, never sourced.
# shellcheck disable=SC2016
for bad in 0 000 root -5 4294967295 99999999999 '65532;reboot' '$(id -u)' ''; do
  printf 'EXEC_AGENT_UID=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts EXEC_AGENT_UID='$bad'"
  elif grep -q '^host configuration invalid: EXEC_AGENT_UID' "$C/run/not-ready.d/flr-host-config" 2>/dev/null; then
    ok "host-config refuses EXEC_AGENT_UID='$bad' and says why"
  else
    fail "host-config refuses EXEC_AGENT_UID='$bad' without recording why"
  fi
done
# The Exec Agent's id cannot be a Host Service's, whose traffic to
# flintlockd is dropped (HI-064).
printf 'EXEC_AGENT_UID=1000\n' >"$C.conf"
if render "$C.conf"; then
  fail "host-config accepts an Exec Agent user id that is a Host Service's"
elif grep -q "^host configuration invalid: HOST_SERVICE_UIDS entry 1000 holds the Exec Agent's user id" "$C/run/not-ready.d/flr-host-config" 2>/dev/null; then
  ok "host-config refuses an Exec Agent user id that is a Host Service's and says why"
else
  fail "host-config refuses an Exec Agent user id that is a Host Service's without recording why"
fi
# shellcheck disable=SC2016
for bad in 10.0.0.0 10.0.0.0/33 256.0.0.0/8 a/8 '10.0.0.0/16;reboot' '10.0.0.0/16,,10.1.0.0/16' '10.0.0.0/16,' '$(id -u)' 'fd00::/8'; do
  printf 'FLINTLOCKD_CLIENT_CIDRS=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts FLINTLOCKD_CLIENT_CIDRS='$bad'"
  elif grep -q '^host configuration invalid: FLINTLOCKD_CLIENT_CIDRS' "$C/run/not-ready.d/flr-host-config" 2>/dev/null; then
    ok "host-config refuses FLINTLOCKD_CLIENT_CIDRS='$bad' and says why"
  else
    fail "host-config refuses FLINTLOCKD_CLIENT_CIDRS='$bad' without recording why"
  fi
done

# Enforcement of the output chain, where this machine allows it: the
# rendered ruleset is loaded into a new user and network namespace, where
# this process is a chosen user and the Host's internal address is on a
# dummy interface, and a connection to the flintlockd port is attempted,
# on that address and on loopback. Nothing listens there, so the
# connection fails either way; the rule's own counter says whether it was
# the firewall that refused it. That needs unprivileged user namespaces,
# nft and ip, which a build container and many CI runners lack, and then
# the section is skipped.
# attempt UID ADDRESS: prints the number of packets the rule refused. The
# inner script expands its own arguments.
# shellcheck disable=SC2016
attempt() {
  unshare --user --map-user="$1" --map-group="$1" --net --keep-caps bash -c '
    { ip link set lo up && ip link add d0 type dummy && ip link set d0 up &&
      ip addr add 192.0.2.10/24 dev d0 && nft -f "$1"; } >/dev/null 2>&1 || exit 3
    { exec 3<>/dev/tcp/$2/'"$port"'; } 2>/dev/null
    nft list chain inet flr output | sed -n "s/.*skuid != [0-9]* counter packets \([0-9]*\) .*/\1/p"
  ' _ "$C/net/guest-firewall.nft" "$2" 2>/dev/null
}
printf 'EXEC_AGENT_UID=4242\n' >"$C.conf"
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1; then
  printf 'skip  enforcement: nft or ip is not installed\n'
elif ! render "$C.conf" || ! unshare --user --map-user=4242 --net --keep-caps true 2>/dev/null ||
  [ -z "$(attempt 4242 127.0.0.1)" ]; then
  printf 'skip  enforcement: no unprivileged user and network namespace with nftables here\n'
else
  for a in 192.0.2.10 127.0.0.1; do
    if [ "$(attempt 4242 $a)" = 0 ]; then ok "enforced: user 4242, configured, is let through to $a:$port"; else fail "enforced: the configured user 4242 was refused on $a"; fi
    if [ "$(attempt 0 $a)" -ge 1 ]; then ok "enforced: root is refused on $a:$port"; else fail "enforced: root was let through on $a"; fi
    if [ "$(attempt 4243 $a)" -ge 1 ]; then ok "enforced: another user is refused on $a:$port"; else fail "enforced: another user was let through on $a"; fi
  done
fi


# Enforcement of the input chain, where this machine allows it: the Host is
# a new user and network namespace with the ruleset loaded and the internal
# address on one end of a veth pair, and a client is a second network
# namespace on the other end, with an address inside or outside the
# Operator's pod network. The counters of the two rules say which one the
# connection met.
# attempt_from ADDRESS: prints "ACCEPTED DROPPED", in packets.
# shellcheck disable=SC2016
attempt_from() {
  unshare --user --map-root-user --net bash -c '
    { ip link set lo up && nft -f "$1" && ip link add v0 type veth peer name v1 &&
      ip addr add 192.0.2.10/24 dev v0 && ip link set v0 up; } >/dev/null 2>&1 || exit 3
    unshare --net sleep 5 & client=$!
    sleep 0.3
    { ip link set v1 netns "$client" &&
      nsenter -t "$client" -n sh -c "ip link set lo up && ip addr add $2/24 dev v1 && ip link set v1 up"; } >/dev/null 2>&1 || exit 3
    nsenter -t "$client" -n timeout 2 bash -c "exec 3<>/dev/tcp/192.0.2.10/'"$port"'" 2>/dev/null
    kill "$client" 2>/dev/null
    nft list chain inet flr input | awk "
      /@flintlockd_clients counter/ { for (i = 1; i < NF; i++) if (\$i == \"packets\") a += \$(i + 1) }
      /tcp dport '"$port"' counter packets/ && !/@flintlockd_clients/ { for (i = 1; i < NF; i++) if (\$i == \"packets\") d += \$(i + 1) }
      END { print a + 0, d + 0 }"
  ' _ "$C/net/guest-firewall.nft" "$1" 2>/dev/null
}
printf 'FLINTLOCKD_CLIENT_CIDRS=192.0.2.16/28\n' >"$C.conf"
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1 || ! command -v nsenter >/dev/null 2>&1; then
  printf 'skip  enforcement from off the Host: nft, ip or nsenter is not installed\n'
elif ! render "$C.conf" || [ -z "$(attempt_from 192.0.2.20)" ]; then
  printf 'skip  enforcement from off the Host: no unprivileged user and network namespaces with veth and nftables here\n'
else
  n=$(attempt_from 192.0.2.20)
  if [ "${n% *}" -ge 1 ] && [ "${n#* }" = 0 ]; then ok "enforced: a client in the Operator's pod network, 192.0.2.20, is let through to flintlockd"; else fail "enforced: 192.0.2.20 in 192.0.2.16/28 was not let through (accepted, dropped: $n)"; fi
  n=$(attempt_from 192.0.2.40)
  if [ "${n% *}" = 0 ] && [ "${n#* }" -ge 1 ]; then ok "enforced: a client outside it, 192.0.2.40, is dropped"; else fail "enforced: 192.0.2.40 outside 192.0.2.16/28 was not dropped (accepted, dropped: $n)"; fi
fi

rm -rf "$C" "$C.conf" "$C.absent.conf"
[ "$failures" -eq 0 ]
