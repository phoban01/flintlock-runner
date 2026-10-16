#!/usr/bin/env bash
# The kind hook of flr-registry-mirror.service, installed as
# /usr/libexec/flr-guest/registry-mirror.d/20-kind in the Guest Image's kind
# image. It writes /etc/kind/kind-config.yaml from FLR_REGISTRY_MIRROR
# (guest/rootfs-docker/registry-mirror.sh), under FLR_ROOT when that is set.
# The build runs it once with no mirror, so the image has the file before
# the first boot.
#
# The kind node runs its own containerd, which does not use Docker Engine's
# mirror. With a mirror, the configuration has a containerdConfigPatches
# entry that points the node's containerd at a hosts directory, and mounts
# that directory, /etc/kind/certs.d, into the node. Its docker.io/hosts.toml
# names the mirror over plain HTTP, ahead of Docker Hub. The node image's
# containerd 2 takes mirrors only from a hosts directory.
set -euo pipefail

root=${FLR_ROOT:-}
mirror=${FLR_REGISTRY_MIRROR:-}
dir=$root/etc/kind
file=$dir/kind-config.yaml
hosts=$dir/certs.d

#= docs/requirements/13-guest-image.md#guest-kind
#/ When the guest has a default route once the wait for the network
#/ has ended, the Guest Image's kind image SHALL write
#/ `/etc/kind/kind-config.yaml` with a containerd mirror of `docker.io` over
#/ plain HTTP at port 5000 of the default route's gateway.
#
#= docs/requirements/13-guest-image.md#guest-kind
#/ If the guest has no default route once the wait for the network has
#/ ended, then the Guest Image's kind image SHALL write
#/ `/etc/kind/kind-config.yaml` with no registry mirror.
mkdir -p "$dir"
rm -rf "$hosts"
{
	echo "# Written at boot by flr-registry-mirror.service"
	echo "# (/usr/libexec/flr-guest/registry-mirror.d/20-kind); do not edit."
	if [ -n "$mirror" ]; then
		echo "# docker.io comes through the Host's registry mirror, $mirror."
	else
		echo "# The guest had no default gateway at boot, so there is no registry mirror."
	fi
	echo "kind: Cluster"
	echo "apiVersion: kind.x-k8s.io/v1alpha4"
	if [ -n "$mirror" ]; then
		cat <<-EOF
			containerdConfigPatches:
			  - |-
			    [plugins."io.containerd.grpc.v1.cri".registry]
			      config_path = "/etc/containerd/certs.d"
			nodes:
			  - role: control-plane
			    extraMounts:
			      - hostPath: /etc/kind/certs.d
			        containerPath: /etc/containerd/certs.d
			        readOnly: true
		EOF
	else
		cat <<-EOF
			nodes:
			  - role: control-plane
		EOF
	fi
} >"$file.tmp"
if [ -n "$mirror" ]; then
	mkdir -p "$hosts/docker.io"
	cat >"$hosts/docker.io/hosts.toml" <<-EOF
		# Written at boot by flr-registry-mirror.service; do not edit.
		server = "https://registry-1.docker.io"

		[host."$mirror"]
		  capabilities = ["pull", "resolve"]
	EOF
fi
mv "$file.tmp" "$file"
echo "registry-mirror: wrote $file${mirror:+ with the registry mirror $mirror}"
