#!/usr/bin/bash
# Build step of the fetch stage: download every binary that does not come
# from a signed package repository into /out, verifying each one.
set -euo pipefail
# shellcheck source=image/versions.env
. "${1:-/build/versions.env}"

# This stage runs on the build machine's architecture and only downloads,
# verifies and unpacks: every artifact below is the x86_64 one by name.
mkdir -p /dl /out/usr/bin

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# fetch URL FILE SHA256: https only, and no file is used before its checksum
# matched. An empty checksum fails closed.
fetch() {
  [ -n "$3" ] || {
    echo "no checksum is recorded for $1" >&2
    exit 1
  }
  curl --proto '=https' --tlsv1.2 -fsSL --retry 5 --retry-all-errors -o "/dl/$2" "$1"
  echo "$3  /dl/$2" | sha256sum -c -
}

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image build SHALL complete on a machine that has
#/ neither `/dev/kvm` nor access to AWS.
# Everything comes from GitHub releases and package repositories over https,
# and nothing the build runs needs a hypervisor.
gh=https://github.com

fetch "$gh/containerd/containerd/releases/download/$CONTAINERD_VERSION/containerd-${CONTAINERD_VERSION#v}-linux-amd64.tar.gz" containerd.tgz "$CONTAINERD_SHA256"
tar -xzf /dl/containerd.tgz -C /out/usr --no-same-owner bin/containerd bin/containerd-shim-runc-v2 bin/ctr

fetch "$gh/opencontainers/runc/releases/download/$RUNC_VERSION/runc.amd64" runc "$RUNC_SHA256"
install -m 0755 /dl/runc /out/usr/bin/runc

fc=firecracker-$FIRECRACKER_VERSION-x86_64
fetch "$gh/firecracker-microvm/firecracker/releases/download/$FIRECRACKER_VERSION/$fc.tgz" firecracker.tgz "$FIRECRACKER_SHA256"
mkdir -p /dl/fc
tar -xzf /dl/firecracker.tgz -C /dl/fc --no-same-owner
install -m 0755 "/dl/fc/release-$FIRECRACKER_VERSION-x86_64/$fc" /out/usr/bin/firecracker
install -m 0755 "/dl/fc/release-$FIRECRACKER_VERSION-x86_64/jailer-$FIRECRACKER_VERSION-x86_64" /out/usr/bin/jailer

fetch "$gh/cloud-hypervisor/cloud-hypervisor/releases/download/$CLOUD_HYPERVISOR_VERSION/cloud-hypervisor-static" cloud-hypervisor-static "$CLOUD_HYPERVISOR_SHA256"
install -m 0755 /dl/cloud-hypervisor-static /out/usr/bin/cloud-hypervisor-static

fetch "$gh/liquidmetal-dev/flintlock/releases/download/$FLINTLOCK_VERSION/flintlockd_amd64" flintlockd "$FLINTLOCK_SHA256"
install -m 0755 /dl/flintlockd /out/usr/bin/flintlockd

# The signing key of the Kubernetes package repository. dnf verifies the
# kubelet and kubeadm packages against it, so it is the download to pin.
k8s_minor=$(echo "$KUBERNETES_VERSION" | cut -d. -f1,2)
fetch "https://pkgs.k8s.io/core:/stable:/$k8s_minor/rpm/repodata/repomd.xml.key" kubernetes.key "$KUBERNETES_REPO_KEY_SHA256"
install -D -m 0644 /dl/kubernetes.key /out/etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes
