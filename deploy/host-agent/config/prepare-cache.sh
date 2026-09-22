#!/bin/sh
# prepare-cache.sh: the Host Agent's second init container. It creates each
# enabled Host Service's directory under the Host Service cache directory of
# HI-024 and gives it to the user id that service runs as (KF-072).
#
# Each Host Service component lists its directories in a ConfigMap entry
# named cache-dirs.<service>, one "path uid" per line, relative to the cache
# directory. The cache directory itself is the Host Image's (flr-cache mounts
# the cache volume there, root-owned); nothing outside it is touched.
#
# This is the only container of the Host Agent that runs as root, and it
# holds CAP_CHOWN alone: root owns the cache directory, so creating entries
# and changing their mode needs no capability, and handing a directory to a
# service's user id needs exactly that one. Each directory is first taken
# back to root so that its mode can be set without CAP_FOWNER.
#
# Environment, for the tests in deploy/tests/checks.yaml:
#   FLR_TEMPLATES  the ConfigMap's mount (default /etc/flr/templates)
#   FLR_CACHE      the cache directory (default /var/lib/flintlock-runner/cache)
set -eu

templates=${FLR_TEMPLATES:-/etc/flr/templates}
cache=${FLR_CACHE:-/var/lib/flintlock-runner/cache}

die() {
  echo "prepare-cache: $*" >&2
  exit 1
}

[ -d "$cache" ] || die "$cache is not a directory: the Host Image's flr-cache unit has not run"

for list in "$templates"/cache-dirs.*; do
  [ -e "$list" ] || continue
  sed -e 's/#.*//' "$list" | grep -v '^[[:space:]]*$' | while read -r path uid extra; do
    case $path in
    '' | /* | *..* | *[!A-Za-z0-9_./-]*) die "$(basename "$list"): '$path' is not a relative path under the cache" ;;
    esac
    case $uid in
    '' | *[!0-9]*) die "$(basename "$list"): uid '$uid' for $path is not a number" ;;
    esac
    [ "$uid" != 0 ] || die "$(basename "$list"): $path would stay root's"
    [ -z "$extra" ] || die "$(basename "$list"): unexpected '$extra'"
    dir=$cache/$path
    mkdir -p "$dir"
    chown 0:0 "$dir"
    chmod 0700 "$dir"
    chown "$uid:$uid" "$dir"
    echo "prepare-cache: $dir for $uid"
  done
done
