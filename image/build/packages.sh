#!/usr/bin/bash
# Build step of the image stage: check the build arguments against the
# versions file, then install the packaged components at their pinned
# versions.
set -euo pipefail

# `make image` passes the versions as build arguments so that they can
# become OCI labels, and a build argument is an environment variable here.
# One that disagrees with the file fails the build, so the file stays the
# only place a version is decided.
names="CONTAINERD_VERSION RUNC_VERSION FIRECRACKER_VERSION CLOUD_HYPERVISOR_VERSION FLINTLOCK_VERSION KUBERNETES_VERSION CLOUD_INIT_VERSION"
declare -A args=()
for v in $names; do
  args[$v]=${!v:-}
done
# shellcheck source=image/versions.env
. /usr/share/flr/versions.env
for v in $names; do
  file=${!v:-}
  arg=${args[$v]}
  [ -n "$file" ] || {
    echo "versions.env pins no $v" >&2
    exit 1
  }
  [ "$arg" = "$file" ] || {
    echo "build argument $v='$arg' is not versions.env's '$file'; build with 'make image'" >&2
    exit 1
  }
done

#= docs/requirements/11-host-image.md#image-build
#/ The Host Image SHALL contain containerd, Firecracker with its
#/ jailer, Cloud Hypervisor, `flintlockd`, the kubelet, `kubeadm` and
#/ cloud-init at the versions pinned in one versions file that the
#/ Containerfile reads.
k8s_minor=$(echo "$KUBERNETES_VERSION" | cut -d. -f1,2)
# enabled=0: a Host never installs packages, and a new kubelet is a new image.
cat >/etc/yum.repos.d/kubernetes.repo <<EOF
[kubernetes]
name=Kubernetes $k8s_minor
baseurl=https://pkgs.k8s.io/core:/stable:/$k8s_minor/rpm/
enabled=0
gpgcheck=1
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-kubernetes
EOF

# The kubernetes-cni package installs into /opt, a link to /var/opt.
mkdir -p /var/opt
dnf -y --enablerepo=kubernetes --setopt=install_weak_deps=False install \
  "cloud-init-$CLOUD_INIT_VERSION" \
  "kubelet-${KUBERNETES_VERSION#v}" "kubeadm-${KUBERNETES_VERSION#v}" \
  kubernetes-cni cri-tools \
  dnsmasq nftables iproute lvm2 device-mapper-persistent-data device-mapper-event e2fsprogs \
  conntrack-tools socat ethtool iptables-nft jq util-linux \
  container-selinux policycoreutils

# The CNI plugins move to /usr, which an upgrade replaces; tmpfiles.d copies
# them to /opt/cni/bin at boot.
mkdir -p /usr/libexec/cni
cp -a /opt/cni/bin/. /usr/libexec/cni/
rm -rf /var/opt/cni
dnf clean all
