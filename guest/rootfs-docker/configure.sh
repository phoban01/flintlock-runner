#!/usr/bin/env bash
# Configure the Guest Image's Docker image, in the docker stage of
# guest/rootfs-docker/Containerfile, on the root filesystem image for the
# image's platform.
#
# TARGETARCH, DOCKER_VERSION, DOCKER_APT_VERSION, CONTAINERD_APT_VERSION,
# BUILDX_APT_VERSION, DOCKER_APT_KEY_SHA256, GO_VERSION, GO_SHA256_AMD64 and
# GO_SHA256_ARM64 come from the Containerfile's build arguments.
set -euo pipefail
: "${TARGETARCH:?}" "${DOCKER_VERSION:?}" "${DOCKER_APT_VERSION:?}" \
	"${CONTAINERD_APT_VERSION:?}" "${BUILDX_APT_VERSION:?}" "${DOCKER_APT_KEY_SHA256:?}" \
	"${GO_VERSION:?}" "${GO_SHA256_AMD64:?}" "${GO_SHA256_ARM64:?}"
export DEBIAN_FRONTEND=noninteractive

case $TARGETARCH in
arm64) go_sum=$GO_SHA256_ARM64 ;;
amd64) go_sum=$GO_SHA256_AMD64 ;;
*) echo "configure: no Docker image for $TARGETARCH" >&2; exit 1 ;;
esac

#= docs/requirements/13-guest-image.md#guest-build
#/ The Guest Image build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the Guest Image's sources and
#/ SHALL fail when one does not match.
#
# Docker's apt signing key, then Go. apt checks each Docker package against
# the index that key signs.
key=/etc/apt/keyrings/docker.asc
install -d -m 0755 /etc/apt/keyrings
curl -fsSL -o "$key" https://download.docker.com/linux/ubuntu/gpg
go_tgz=/tmp/go.tar.gz
curl -fsSL -o "$go_tgz" "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz"
echo "$DOCKER_APT_KEY_SHA256  $key" | sha256sum -c -
echo "$go_sum  $go_tgz" | sha256sum -c -
chmod 0644 "$key"

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL contain Docker Engine,
#/ containerd, runc and the Docker buildx plugin, each at the version pinned
#/ in its sources, and SHALL start Docker Engine at boot.
#
# containerd.io carries runc. iptables comes in as a dependency of
# docker-ce.
printf 'deb [arch=%s signed-by=%s] https://download.docker.com/linux/ubuntu noble stable\n' \
	"$TARGETARCH" "$key" >/etc/apt/sources.list.d/docker.list
apt-get update
apt-get install -y --no-install-recommends \
	"docker-ce=$DOCKER_APT_VERSION" "docker-ce-cli=$DOCKER_APT_VERSION" \
	"containerd.io=$CONTAINERD_APT_VERSION" "docker-buildx-plugin=$BUILDX_APT_VERSION" \
	iptables
got=$(dockerd --version | sed -n 's/^Docker version \([^,]*\),.*/\1/p')
if [ "$got" != "$DOCKER_VERSION" ]; then
	echo "configure: dockerd is $got, not $DOCKER_VERSION" >&2
	exit 1
fi
systemctl enable containerd.service docker.service docker.socket

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL set iptables and ip6tables to
#/ their legacy backend.
#
# Firecracker's CI kernel has no nf_tables, which the nft backend needs.
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL run Docker Engine without
#/ the iptables `raw` table, which the kernel does not have.
#
# Docker Engine 28 and later drop packets to a container's address that
# come from outside its bridge, with rules in the raw table. Without that
# table every `docker run` with a network fails, unless
# DOCKER_INSECURE_NO_IPTABLES_RAW=1 tells it to skip those rules. The
# guest's only network is the Host's guest bridge, so what those rules
# would stop is another guest on that bridge that routes to a container's
# address in this guest.
mkdir -p /etc/systemd/system/docker.service.d
printf '[Service]\nEnvironment=DOCKER_INSECURE_NO_IPTABLES_RAW=1\n' \
	>/etc/systemd/system/docker.service.d/20-no-iptables-raw.conf

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL contain Go at the version
#/ pinned in its sources, on the `PATH`, with `GOTOOLCHAIN` set to `auto`.
#
# Go's own go.env sets GOTOOLCHAIN=auto; /etc/environment says so as well,
# for a Job that reads it. With auto, a module whose go.mod asks for a newer
# Go fetches that toolchain through GOPROXY, which the Executor points at the
# Host's Go module proxy.
rm -rf /usr/local/go
tar -C /usr/local -xzf "$go_tgz"
rm -f "$go_tgz"
ln -sf /usr/local/go/bin/go /usr/local/bin/go
ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
if ! grep -q '^GOTOOLCHAIN=' /etc/environment 2>/dev/null; then
	echo 'GOTOOLCHAIN=auto' >>/etc/environment
fi

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL start Docker Engine only after
#/ the boot unit that writes its configuration has finished.
chmod 0755 /usr/libexec/flr-guest/registry-mirror /usr/libexec/flr-guest/registry-mirror.d/*
systemctl enable flr-registry-mirror.service
mkdir -p /etc/systemd/system/docker.service.d
printf '[Unit]\nWants=flr-registry-mirror.service\nAfter=flr-registry-mirror.service\n' \
	>/etc/systemd/system/docker.service.d/10-flr-registry-mirror.conf
# The configuration with no mirror, until the boot unit writes it.
FLR_REGISTRY_MIRROR='' /usr/libexec/flr-guest/registry-mirror.d/10-docker

# The versions file gains the Docker image's versions. runc reports its own.
runc_version=$(runc --version | sed -n 's/^runc version //p')
containerd_version=$(containerd --version | awk '{print $3}')
buildx_version=$(docker buildx version | awk '{print $2}')
cat >>/usr/share/flr-guest/versions.env <<EOF
DOCKER_VERSION=$DOCKER_VERSION
CONTAINERD_VERSION=${containerd_version#v}
RUNC_VERSION=$runc_version
BUILDX_VERSION=${buildx_version#v}
GO_VERSION=$GO_VERSION
EOF

apt-get clean
rm -rf /var/lib/apt/lists/*
