#!/usr/bin/env bash
# Configure the Guest Image's baker image, in the baker stage of
# guest/baker/Containerfile, for the image's platform.
#
# TARGETARCH, HELM_VERSION, KUSTOMIZE_VERSION and the HELM_SHA256_* and
# KUSTOMIZE_SHA256_* sums come from the Containerfile's build arguments.
set -euo pipefail
: "${TARGETARCH:?}" "${HELM_VERSION:?}" "${KUSTOMIZE_VERSION:?}" \
	"${HELM_SHA256_AMD64:?}" "${HELM_SHA256_ARM64:?}" \
	"${KUSTOMIZE_SHA256_AMD64:?}" "${KUSTOMIZE_SHA256_ARM64:?}"

case $TARGETARCH in
amd64) helm_sum=$HELM_SHA256_AMD64 kustomize_sum=$KUSTOMIZE_SHA256_AMD64 ;;
arm64) helm_sum=$HELM_SHA256_ARM64 kustomize_sum=$KUSTOMIZE_SHA256_ARM64 ;;
*) echo "configure: no baker image for $TARGETARCH" >&2; exit 1 ;;
esac

# skopeo copies images, and curl is for a project's own steps. Ubuntu's
# archive is signed, so its packages need no checksum here.
apt-get update
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl skopeo
rm -rf /var/lib/apt/lists/*

#= docs/requirements/13-guest-image.md#guest-build
#/ The Guest Image build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the Guest Image's sources and
#/ SHALL fail when one does not match.
work=$(mktemp -d)
curl -fsSL -o "$work/helm.tar.gz" "https://get.helm.sh/helm-v${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"
curl -fsSL -o "$work/kustomize.tar.gz" \
	"https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_${TARGETARCH}.tar.gz"
printf '%s  %s\n' "$helm_sum" "$work/helm.tar.gz" "$kustomize_sum" "$work/kustomize.tar.gz" | sha256sum -c -
tar -xzf "$work/helm.tar.gz" -C "$work" "linux-${TARGETARCH}/helm"
install -m 0755 "$work/linux-${TARGETARCH}/helm" /usr/local/bin/helm
tar -xzf "$work/kustomize.tar.gz" -C "$work" kustomize
install -m 0755 "$work/kustomize" /usr/local/bin/kustomize
rm -rf "$work"

mkdir -p /usr/share/flr-guest
cat >/usr/share/flr-guest/versions.env <<VERSIONS
HELM_VERSION=$HELM_VERSION
KUSTOMIZE_VERSION=$KUSTOMIZE_VERSION
SKOPEO_VERSION=$(skopeo --version | awk '{print $3}')
VERSIONS
