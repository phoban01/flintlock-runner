#!/bin/sh
# prewarm.sh go-proxy|registry-mirror: pre-warm one Host Service with the
# modules or images listed in its ConfigMap entry, through the service itself
# so that they land in this Host's cache (KF-076, ported from
# internal/fleet/scripts/templates/prewarm.sh.tmpl). Fetching what is already
# cached is a cache hit and changes nothing, so a restart of the container
# costs nothing.
#
# It runs as a container of the Host Agent next to the service it warms,
# waits for the service to answer on the bridge gateway, fetches the list and
# then sleeps, because a DaemonSet's containers cannot exit. A fetch that
# fails is retried a few times and then reported and skipped: a module or an
# image that is gone upstream must not keep the Host Agent crash-looping.
#
# Environment, for the tests in deploy/tests/checks.yaml:
#   FLR_TEMPLATES  the ConfigMap's mount (default /etc/flr/templates)
#   FLR_RENDERED   render.sh's output (default /etc/flr/rendered)
#   FLR_PREWARM_ONCE  exit instead of sleeping when set
set -eu

templates=${FLR_TEMPLATES:-/etc/flr/templates}
rendered=${FLR_RENDERED:-/etc/flr/rendered}
service=${1:?usage: prewarm.sh go-proxy|registry-mirror}
gateway=$(cat "$rendered/gateway")

log() { echo "prewarm $service: $*" >&2; }

# items FILE prints the non-empty, non-comment lines of FILE.
items() {
  [ -r "$1" ] || return 0
  sed -e 's/#.*//' -e 's/[[:space:]]//g' "$1" | grep -v '^$' || true
}

# fetch URL [HEADER] fetches URL and discards the body, with retries.
fetch() {
  n=0
  while :; do
    if [ -n "${2:-}" ]; then
      wget -q -T 60 -O /dev/null --header "$2" "$1" && return 0
    else
      wget -q -T 60 -O /dev/null "$1" && return 0
    fi
    n=$((n + 1))
    [ "$n" -lt 3 ] || return 1
    sleep 5
  done
}

# wait_for URL waits until the service answers at all, with any status.
wait_for() {
  until wget -S -T 5 -O /dev/null "$1" 2>&1 | grep -q 'HTTP/'; do
    log "waiting for $1"
    sleep 5
  done
}

# escape applies the module proxy protocol's case encoding: an upper-case
# letter becomes '!' followed by its lower case. Module paths are ASCII.
# shellcheck disable=SC2018,SC2019
escape() {
  printf '%s' "$1" | sed -e 's/[A-Z]/!&/g' | tr 'A-Z' 'a-z'
}

failed=0
case $service in
go-proxy)
  proxy="http://$gateway:3000"
  wait_for "$proxy/"
  for m in $(items "$templates/prewarm.go-proxy.modules"); do
    mod=${m%@*}
    ver=${m##*@}
    if [ "$mod" = "$m" ] || [ -z "$mod" ] || [ -z "$ver" ]; then
      log "skipping '$m': not module@version"
      failed=$((failed + 1))
      continue
    fi
    path="$(escape "$mod")/@v/$(escape "$ver")"
    for ext in info mod zip; do
      if ! fetch "$proxy/$path.$ext"; then
        log "fetching $path.$ext through $proxy failed"
        failed=$((failed + 1))
      fi
    done
  done
  ;;
registry-mirror)
  mirror="http://$gateway:5000"
  wait_for "$mirror/v2/"
  accept='Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
  for ref in $(items "$templates/prewarm.registry-mirror.images"); do
    # The mirror serves every upstream under the repository path alone.
    repo=$ref
    case ${repo%%/*} in
    *.* | *:* | localhost) [ "${repo#*/}" != "$repo" ] && repo=${repo#*/} ;;
    esac
    case $repo in
    *@*) reference=${repo#*@} repo=${repo%%@*} ;;
    *:*) reference=${repo##*:} repo=${repo%:*} ;;
    *) reference=latest ;;
    esac
    case $repo in
    */*) ;;
    *) repo=library/$repo ;;
    esac
    # A manifest request makes the mirror sync the image from upstream on
    # demand.
    if ! fetch "$mirror/v2/$repo/manifests/$reference" "$accept"; then
      log "pulling $ref through $mirror failed"
      failed=$((failed + 1))
    fi
  done
  ;;
*)
  echo "usage: prewarm.sh go-proxy|registry-mirror" >&2
  exit 2
  ;;
esac

log "done, $failed failed"
[ -z "${FLR_PREWARM_ONCE:-}" ] || exit 0
exec sleep 2147483647
