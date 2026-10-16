#!/usr/bin/env bash
# The Docker Engine hook of flr-registry-mirror.service, installed as
# /usr/libexec/flr-guest/registry-mirror.d/10-docker in the Guest Image's
# Docker image. It writes /etc/docker/daemon.json from FLR_REGISTRY_MIRROR
# (registry-mirror.sh), under FLR_ROOT when that is set. The build runs it
# once with no mirror, so the image has the file before the first boot.
#
# Docker Engine 29 keeps images in containerd's image store unless told
# otherwise. This configuration keeps the overlay2 storage driver instead:
# the registry mirror applies to it, and it is what `docker info` and most
# Jobs expect.
set -euo pipefail

root=${FLR_ROOT:-}
mirror=${FLR_REGISTRY_MIRROR:-}
file=$root/etc/docker/daemon.json

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL configure Docker Engine with
#/ the overlay2 storage driver.
mirrors=''
if [ -n "$mirror" ]; then
	mirrors=$(printf ',\n  "registry-mirrors": ["%s"]' "$mirror")
fi
mkdir -p "$(dirname "$file")"
#= docs/requirements/13-guest-image.md#guest-agent-protection
#/ The Guest Image's Docker image SHALL run containerd, Docker Engine
#/ and every container in a slice whose memory limit is 90 percent of the
#/ MicroVM's memory and whose CPU weight is the default of 100.
cat >"$file.tmp" <<EOF
{
  "storage-driver": "overlay2",
  "cgroup-parent": "flr-containers.slice",
  "features": { "containerd-snapshotter": false }${mirrors}
}
EOF
mv "$file.tmp" "$file"
echo "registry-mirror: wrote $file${mirror:+ with the registry mirror $mirror}"
