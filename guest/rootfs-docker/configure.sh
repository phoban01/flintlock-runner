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

# A fast start. containerd 2 migrates the package's old configuration
# on every start; this one is in containerd's own version, 4,
# with the CRI plugins off as the package had them.
# Docker Engine starts beside containerd instead of after it: it waits for
# containerd's socket itself, and reads its own binary from disk meanwhile.
# Nor does it wait for network-online.target: flr-registry-mirror.service,
# which it does wait for, waits for the network.
cat >/etc/containerd/config.toml <<'EOF'
# containerd for Docker Engine in the Guest Image's Docker image
# (guest/rootfs-docker/configure.sh): no CRI, and no migration at start.
version = 4
disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.images", "io.containerd.cri.v1.runtime"]
EOF
sed -E -e '/^After=/{s/(=| )network-online\.target/\1/;s/ containerd\.service//;s/= +/=/}' \
	-e '/^Wants=/{s/(=| )network-online\.target/\1/;s/= +/=/}' \
	/usr/lib/systemd/system/docker.service >/etc/systemd/system/docker.service
if grep -E '^(After|Wants)=' /etc/systemd/system/docker.service | grep -qE 'network-online|After=.*containerd'; then
	echo "configure: docker.service still waits for the network or containerd" >&2
	exit 1
fi

#= docs/requirements/13-guest-image.md#guest-docker
#/ The Guest Image's Docker image SHALL set iptables and ip6tables to
#/ their legacy backend.
#
# The legacy backend works on every kernel; Firecracker's CI kernels of
# earlier releases had no nf_tables, which the nft backend needs.
update-alternatives --set iptables /usr/sbin/iptables-legacy
update-alternatives --set ip6tables /usr/sbin/ip6tables-legacy

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

#= docs/requirements/13-guest-image.md#guest-agent-protection
#/ The Guest Image's Docker image SHALL run the guest agent with an OOM
#/ score adjustment of -999, in a slice of its own with at least 128 MiB of
#/ memory protected and a CPU weight of 1000.
#
# The guest agent starts every Stage script, which inherits its OOM score
# adjustment. At -999 the kernel kills a Stage's process before the guest
# agent, and still can when nothing else is left; at -1000 a Stage that
# took all the memory would leave the kernel nothing to kill.
mkdir -p /etc/systemd/system/guest-agent.service.d
cat >/etc/systemd/system/guest-agent.service.d/50-flr-protect.conf <<'EOF'
# The guest agent keeps memory and CPU when containers take the rest
# (agent.slice, containers.slice).
[Service]
Slice=agent.slice
OOMScoreAdjust=-999
EOF

#= docs/requirements/13-guest-image.md#guest-agent-protection
#/ The Guest Image's Docker image SHALL run containerd, Docker Engine
#/ and every container in a slice whose memory limit is 90 percent of the
#/ MicroVM's memory and whose CPU weight is the default of 100.
#
# Docker Engine puts each container under its cgroup-parent, which
# mirror-docker.sh writes into daemon.json.
for unit in containerd.service docker.service; do
	mkdir -p "/etc/systemd/system/$unit.d"
	printf '[Service]\nSlice=containers.slice\n' >"/etc/systemd/system/$unit.d/30-flr-slice.conf"
done

#= docs/requirements/13-guest-image.md#guest-agent-protection
#/ The Guest Image's Docker image SHALL limit the block I/O of the
#/ containers' slice on the root filesystem's block device to 1500 read and
#/ 1500 write operations per second and to 60 MB/s of reads and 60 MB/s of
#/ writes, or to the limits that `/etc/default/flr-io` sets in their place.
#
# The defaults are the IO directives of containers.slice. The flr-io
# generator (io-generator.sh) writes a drop-in from /etc/default/flr-io at
# boot, for a derived image on a faster Host. systemd applies a slice's
# limits when it starts the first unit in it; the check stage reads them
# back from the unit as systemd would.
chmod 0755 /usr/lib/systemd/system-generators/flr-io
if ! grep -q '^IOWriteIOPSMax=/ ' /etc/systemd/system/containers.slice; then
	echo "configure: containers.slice sets no IOWriteIOPSMax" >&2
	exit 1
fi

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
