#!/usr/bin/env bash
# Runs /usr/libexec/flr/host-config and `network render` against Host
# configuration files that set, omit and mistype the Pod Provider's user id,
# and checks the firewall rule that admits only that user id to flintlockd's
# local endpoint (HI-063). Installed as
# /usr/libexec/flr/check-flintlockd-access-cases and called by the check
# stage; `image/check-flintlockd-access.sh DIR LIBEXEC DEFAULTS UNIT` runs it
# outside the image against the sources, which `make image-lint` does.
#
# What this shows is the rule the Host would load. Whether the kernel then
# resets a connection from another user needs a network namespace and
# netlink, which neither a build container nor this check has: the rule is
# checked, its enforcement is nftables'.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL admit connections to the local
#/ `flintlockd` endpoint only from the Pod Provider's user id, which it reads
#/ from the Host configuration file with a default when none is set, and
#/ SHALL refuse them from every other process on the Host.
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
  env "${env[@]}" FLR_PRIMARY_INTERFACE=eth0 bash "$libexec/network" render "$C/net" >/dev/null 2>&1
}
# output_chain prints the rules of the rendered output chain, one per line,
# without comments or indentation, and without the rules for the Host
# Services' user ids, which check-host-service-egress.sh checks (HI-064).
output_chain() {
  awk '/^\tchain output \{/ { on = 1; next } on && /^\t\}/ { exit } on' "$C/net/guest-firewall.nft" |
    sed -e 's/#.*//' -e 's/^[[:space:]]*//' -e '/^$/d' | grep -v '^meta skuid {'
}
rule_for() { printf 'oifname "lo" tcp dport %s meta skuid != %s counter reject with tcp reset' "$1" "$2"; }

# The port the rule guards is the one flintlockd listens on.
endpoint=$(grep -Eo -- '--grpc-endpoint [^ ]+' "$unit" | awk '{print $2}')
port=${endpoint##*:}
if [ "$endpoint" = "127.0.0.1:$port" ] && grep -qx "FLR_FLINTLOCKD_PORT=$port" "$libexec/lib.sh"; then
  ok "the rule guards flintlockd's endpoint $endpoint"
else
  fail "flintlockd listens on '$endpoint', and lib.sh's FLR_FLINTLOCKD_PORT does not match it"
fi

# The default, with no Host configuration file at all.
default_uid=$(sed -n 's/^POD_PROVIDER_UID=//p' "$defaults")
if [ "$default_uid" = 10250 ]; then
  ok "the default Pod Provider user id is 10250"
else
  fail "the default Pod Provider user id is '$default_uid', not the 10250 image/README.md promises the Fleet Manifests"
fi
if render "$C.absent.conf"; then
  chain=$(output_chain)
  if [ "$chain" = "$(printf 'type filter hook output priority filter - 10; policy accept;\n%s' "$(rule_for "$port" 10250)")" ]; then
    ok "without a Host configuration file only user id 10250 reaches flintlockd; root and everyone else are reset"
  else
    fail "the output chain for the default user id is: $chain"
  fi
  if grep -qx 'FLR_POD_PROVIDER_UID=10250' "$C/run/host.env"; then ok "host.env carries the default user id"; else fail "host.env lacks FLR_POD_PROVIDER_UID=10250"; fi
else
  fail "host-config or network render fails without a Host configuration file"
fi

# A configured user id replaces the default, whatever else the file sets.
printf 'GUEST_SUBNET=10.200.4.0/22\nPOD_PROVIDER_UID = "4242"\n' >"$C.conf"
if render "$C.conf"; then
  chain=$(output_chain)
  if printf '%s\n' "$chain" | grep -qxF "$(rule_for "$port" 4242)" && ! printf '%s\n' "$chain" | grep -q 10250; then
    ok "a configured POD_PROVIDER_UID=4242 is the only user id that reaches flintlockd"
  else
    fail "the output chain for POD_PROVIDER_UID=4242 is: $chain"
  fi
else
  fail "host-config refuses POD_PROVIDER_UID=4242"
fi

# Root is never the Pod Provider, and a value that is not a user id stops
# the Host rather than opening flintlockd to everyone. The command
# substitution is a literal value: the file is parsed, never sourced.
# shellcheck disable=SC2016
for bad in 0 000 root -5 4294967295 99999999999 '10250;reboot' '$(id -u)' ''; do
  printf 'POD_PROVIDER_UID=%s\n' "$bad" >"$C.conf"
  if render "$C.conf"; then
    fail "host-config accepts POD_PROVIDER_UID='$bad'"
  elif grep -q '^host configuration invalid: POD_PROVIDER_UID' "$C/run/not-ready.d/flr-host-config" 2>/dev/null; then
    ok "host-config refuses POD_PROVIDER_UID='$bad' and says why"
  else
    fail "host-config refuses POD_PROVIDER_UID='$bad' without recording why"
  fi
done

# Enforcement, where this machine allows it: the rendered ruleset is loaded
# into a new user and network namespace, where this process is a chosen
# user, and a connection to the flintlockd port is attempted. Nothing
# listens there, so the connection fails either way; the rule's own counter
# says whether it was the firewall that refused it. That needs unprivileged
# user namespaces, nft and ip, which a build container and many CI runners
# lack, and then the section is skipped.
# attempt UID_IN_NAMESPACE: prints the number of packets the rule refused.
# The inner script expands its own arguments.
# shellcheck disable=SC2016
attempt() {
  unshare --user --map-user="$1" --map-group="$1" --net --keep-caps bash -c '
    ip link set lo up && nft -f "$1" || exit 3
    { exec 3<>/dev/tcp/127.0.0.1/'"$port"'; } 2>/dev/null
    nft list chain inet flr output | sed -n "s/.*skuid != .* counter packets \([0-9]*\) .*/\1/p"
  ' _ "$C/net/guest-firewall.nft" 2>/dev/null
}
printf 'POD_PROVIDER_UID=4242\n' >"$C.conf"
if ! command -v nft >/dev/null 2>&1 || ! command -v ip >/dev/null 2>&1; then
  printf 'skip  enforcement: nft or ip is not installed\n'
elif ! render "$C.conf" || ! unshare --user --map-user=4242 --net --keep-caps true 2>/dev/null ||
  [ -z "$(attempt 4242)" ]; then
  printf 'skip  enforcement: no unprivileged user and network namespace with nftables here\n'
else
  if [ "$(attempt 4242)" = 0 ]; then ok "enforced: user 4242, configured, is let through to the flintlockd port"; else fail "enforced: the configured user 4242 was refused"; fi
  if [ "$(attempt 0)" -ge 1 ]; then ok "enforced: root is refused"; else fail "enforced: root was let through"; fi
  if [ "$(attempt 4243)" -ge 1 ]; then ok "enforced: another user is refused"; else fail "enforced: another user was let through"; fi
fi

rm -rf "$C" "$C.conf"
[ "$failures" -eq 0 ]
