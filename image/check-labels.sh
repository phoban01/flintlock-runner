#!/usr/bin/env bash
# Usage: image/check-labels.sh IMAGE
#
# The half of the check stage that cannot run inside the image: its OCI
# labels are not in its filesystem. Fails unless every component version of
# image/versions.env is a label of IMAGE with the same value, the image is
# x86_64 and the versions file inside it is this one.
#
#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL record the pinned version of each component
#/ of HI-003 as an OCI image label and in a versions file on the image's
#/ `/usr` tree.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
engine=${CONTAINER_ENGINE:-podman}
image=${1:?usage: check-labels.sh IMAGE}
# shellcheck source=image/versions.env
. "$here/versions.env"

failed=0
label() {
  local key=dev.flintlock.gitlab-runner.version.$1 want=$2 got
  got=$("$engine" image inspect --format "{{ index .Config.Labels \"$key\" }}" "$image")
  if [ "$got" = "$want" ]; then
    echo "ok    label $key=$got"
  else
    echo "FAIL  label $key is '$got', versions.env pins '$want'"
    failed=1
  fi
}
label containerd "$CONTAINERD_VERSION"
label runc "$RUNC_VERSION"
label firecracker "$FIRECRACKER_VERSION"
label cloud-hypervisor "$CLOUD_HYPERVISOR_VERSION"
label flintlock "$FLINTLOCK_VERSION"
label kubernetes "$KUBERNETES_VERSION"
label cloud-init "$CLOUD_INIT_VERSION"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be built for the `x86_64` architecture.
arch=$("$engine" image inspect --format '{{ .Architecture }}' "$image")
if [ "$arch" = amd64 ]; then
  echo "ok    the image's architecture is amd64"
else
  echo "FAIL  the image's architecture is $arch, want amd64"
  failed=1
fi

if "$engine" run --rm --platform linux/amd64 "$image" cat /usr/share/flr/versions.env | cmp -s - "$here/versions.env"; then
  echo "ok    /usr/share/flr/versions.env in the image is image/versions.env"
else
  echo "FAIL  /usr/share/flr/versions.env in the image differs from image/versions.env"
  failed=1
fi
exit "$failed"
