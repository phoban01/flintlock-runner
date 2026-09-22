# shellcheck shell=bash disable=SC2034
# Shared by every /usr/libexec/flr script of the Host Image. Sourced, never
# executed. The logic is ported from internal/fleet/scripts/templates, which
# did the same work over SSH on a running instance.

set -euo pipefail
umask 022
export PATH=${FLR_PATH:-/usr/sbin:/usr/bin}

FLR_UNIT=${FLR_UNIT:-$(basename "$0")}
FLR_RUN=${FLR_RUN:-/run/flr}
FLR_NOT_READY_DIR=${FLR_NOT_READY_DIR:-$FLR_RUN/not-ready.d}
# The Host configuration file that cloud-init writes (HI-050), the image's
# defaults for it (HI-051) and the validated result the other units read.
FLR_HOST_CONF=${FLR_HOST_CONF:-/etc/flintlock-runner/host.conf}
FLR_HOST_DEFAULTS=${FLR_HOST_DEFAULTS:-/usr/share/flr/host.conf.defaults}
FLR_HOST_ENV=${FLR_HOST_ENV:-$FLR_RUN/host.env}

# Fixed names, the same ones internal/fleet/scripts uses. The scripts that
# source this file use them.
FLR_BRIDGE=flbr0
FLR_VG=flintlock
FLR_CACHE_DIR=/var/lib/flintlock-runner/cache
FLR_METADATA_V4=169.254.169.254
FLR_METADATA_V6=fd00:ec2::254
# flintlockd's local endpoint, as flintlockd.service passes it to
# --grpc-endpoint (HI-042).
FLR_FLINTLOCKD_ADDR=127.0.0.1
FLR_FLINTLOCKD_PORT=9090

log() { printf '%s: %s\n' "$FLR_UNIT" "$*" >&2; }
die() {
  log "error: $*"
  exit 1
}

# The not-ready contract (image/README.md): one file per unit under
# /run/flr/not-ready.d, holding a one-line reason, present exactly while the
# unit's condition holds. The Pod Provider reports each as the Virtual
# Node's not ready message (KF-016).
not_ready() {
  local tmp
  mkdir -p "$FLR_NOT_READY_DIR"
  tmp=$(mktemp "$FLR_NOT_READY_DIR/.$FLR_UNIT.XXXXXX")
  printf '%s\n' "$(printf '%s' "$*" | tr '\n' ' ')" >"$tmp"
  chmod 0644 "$tmp"
  mv -f "$tmp" "$FLR_NOT_READY_DIR/$FLR_UNIT"
  log "not ready: $*"
}
ready() { rm -f "$FLR_NOT_READY_DIR/$FLR_UNIT"; }
# refuse REASON records the reason and fails the unit, which keeps every
# unit that requires it, flintlockd above all, from starting.
refuse() {
  not_ready "$*"
  exit 1
}

# ip4_to_int A.B.C.D and int_to_ip4 N convert dotted quads.
ip4_to_int() {
  local a b c d
  IFS=. read -r a b c d <<<"$1"
  echo $(((a << 24) | (b << 16) | (c << 8) | d))
}
int_to_ip4() {
  echo "$((($1 >> 24) & 255)).$((($1 >> 16) & 255)).$((($1 >> 8) & 255)).$(($1 & 255))"
}

# valid_cidr4 CIDR succeeds for an IPv4 prefix with in-range octets.
valid_cidr4() {
  local a b c d p
  [[ $1 =~ ^([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})\.([0-9]{1,3})/([0-9]{1,2})$ ]] || return 1
  a=${BASH_REMATCH[1]} b=${BASH_REMATCH[2]} c=${BASH_REMATCH[3]} d=${BASH_REMATCH[4]} p=${BASH_REMATCH[5]}
  [ "$((10#$a))" -le 255 ] && [ "$((10#$b))" -le 255 ] && [ "$((10#$c))" -le 255 ] &&
    [ "$((10#$d))" -le 255 ] && [ "$((10#$p))" -le 32 ]
}

# valid_ports LIST succeeds for a comma-separated list of TCP ports, or "".
valid_ports() {
  local p
  [ -z "$1" ] && return 0
  [[ $1 =~ ^[0-9]+(,[0-9]+)*$ ]] || return 1
  for p in ${1//,/ }; do
    if [ "$((10#$p))" -lt 1 ] || [ "$((10#$p))" -gt 65535 ]; then return 1; fi
  done
}

# The seams check-thin-pool-cases uses to run thin-pool against stand-ins:
# where device nodes live and how one is recognised. A Host sets neither.
FLR_DEV=${FLR_DEV:-/dev}
is_block() {
  if [ -n "${FLR_BLOCK_TEST:-}" ]; then
    $FLR_BLOCK_TEST "$1"
  else
    [ -b "$1" ]
  fi
}

# load_host_env reads the validated settings flr-host-config wrote.
load_host_env() {
  [ -r "$FLR_HOST_ENV" ] || die "$FLR_HOST_ENV is missing; flr-host-config.service has not run"
  # shellcheck disable=SC1090
  . "$FLR_HOST_ENV"
}
