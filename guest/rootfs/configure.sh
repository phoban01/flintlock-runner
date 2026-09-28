#!/usr/bin/env bash
# Configure the Guest Image's root filesystem, in the rootfs stage of
# guest/rootfs/Containerfile, on Ubuntu 24.04 for the image's platform.
#
# TARGETARCH, GUEST_AGENT_VERSION, GUEST_AGENT_SHA256_AMD64,
# GUEST_AGENT_SHA256_ARM64 and GITLAB_RUNNER_VERSION come from the
# Containerfile's build arguments.
#
# The network setup and the empty machine ID follow battery-operator's real
# hosts trial, hack/real-hosts/images/rootfs/Containerfile
# (phoban01/battery-operator#163). systemd-resolved adds about 1.5 seconds
# to boot there.
set -euo pipefail
: "${TARGETARCH:?}" "${GUEST_AGENT_VERSION:?}" "${GUEST_AGENT_SHA256_AMD64:?}" \
	"${GUEST_AGENT_SHA256_ARM64:?}" "${GITLAB_RUNNER_VERSION:?}"
export DEBIAN_FRONTEND=noninteractive

#= docs/requirements/13-guest-image.md#guest-rootfs
#/ The Guest Image SHALL contain bash at `/bin/bash`, git, curl
#/ and the certificate authority certificates of the Ubuntu archive.
#
# systemd and udev boot the guest; systemd-resolved takes the DNS servers of
# the DHCP lease; iproute2, kmod and procps are for whoever debugs a guest.
apt-get update
apt-get install -y --no-install-recommends \
	bash ca-certificates curl git \
	systemd systemd-sysv systemd-resolved udev \
	iproute2 kmod procps

#= docs/requirements/13-guest-image.md#guest-build
#/ The Guest Image build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the Guest Image's sources and
#/ SHALL fail when one does not match.
case $TARGETARCH in
arm64) sum=$GUEST_AGENT_SHA256_ARM64 ;;
amd64) sum=$GUEST_AGENT_SHA256_AMD64 ;;
*) echo "configure: no guest agent for $TARGETARCH" >&2; exit 1 ;;
esac
deb=/tmp/guest-agent.deb
curl -fsSL -o "$deb" \
	"https://github.com/liquidmetal-dev/guest-agent/releases/download/v${GUEST_AGENT_VERSION}/guest-agent_${GUEST_AGENT_VERSION}_linux_${TARGETARCH}.deb"
echo "$sum  $deb" | sha256sum -c -

#= docs/requirements/13-guest-image.md#guest-rootfs
#/ The Guest Image SHALL contain flintlock's guest agent at the
#/ version pinned in its sources and SHALL start it at boot.
dpkg -i "$deb"
rm -f "$deb"
systemctl enable guest-agent.service systemd-networkd.service systemd-resolved.service

#= docs/requirements/13-guest-image.md#guest-network
#/ The Guest Image SHALL NOT start the guest agent after the wait
#/ for the network.
#
# The Executor reaches the guest only through the guest agent, over vsock,
# so the agent must not wait for a network a Job may not need. The unit of
# guest-agent 0.4.0 orders it after network.target alone; a drop-in cannot
# take an ordering away, so a release that adds one fails the build here.
if grep -hE '^(After|Wants|Requires)=' /usr/lib/systemd/system/guest-agent.service | grep -qw network-online.target; then
	echo "configure: guest-agent.service waits for network-online.target" >&2
	exit 1
fi

#= docs/requirements/13-guest-image.md#guest-network
#/ If no interface has an IPv4 address 10 seconds after the guest
#/ starts to wait for the network, then the Guest Image SHALL end the wait
#/ and continue to boot.
#
# The wait ends when any one interface has an IPv4 address: eth0 never
# gets one, and a Pool may have more than one interface.
mkdir -p /etc/systemd/system/systemd-networkd-wait-online.service.d
printf '[Service]\nExecStart=\nExecStart=/usr/lib/systemd/systemd-networkd-wait-online --any --ipv4 --timeout=10\n' \
	>/etc/systemd/system/systemd-networkd-wait-online.service.d/timeout.conf

# The build owns /etc/resolv.conf, so the link to systemd-resolved's stub
# is made at boot.
printf 'L+ /etc/resolv.conf - - - - ../run/systemd/resolve/stub-resolv.conf\n' \
	>/etc/tmpfiles.d/resolv.conf

# timesyncd stays off: the guest has its clock from the Host, through the
# hypervisor.
systemctl mask systemd-timesyncd.service

# A console on the serial port, logged in as root, for debugging from the
# Host. Only the Host can reach it.
mkdir -p /etc/systemd/system/serial-getty@ttyS0.service.d
printf '[Service]\nExecStart=\nExecStart=-/sbin/agetty --autologin root --noclear %%I 115200 linux\n' \
	>/etc/systemd/system/serial-getty@ttyS0.service.d/autologin.conf

echo flr-guest >/etc/hostname
# flintlockd gives the MicroVM its root block device, and nothing else to
# mount.
: >/etc/fstab

#= docs/requirements/13-guest-image.md#guest-network
#/ The Guest Image SHALL contain an empty machine ID, so that each
#/ MicroVM makes its own at boot.
#
# With one ID for all, udev gives every guest the same MAC address (its
# MACAddressPolicy=persistent derives it from the ID), and DHCP the same
# address. An empty file is not a first boot, so systemd-firstboot does not
# run.
: >/etc/machine-id
mkdir -p /var/lib/dbus
ln -sf /etc/machine-id /var/lib/dbus/machine-id

#= docs/requirements/13-guest-image.md#guest-rootfs
#/ The Guest Image SHALL record the versions of the guest agent
#/ and of `gitlab-runner-helper` it carries in a versions file on the root
#/ filesystem's `/usr` tree.
mkdir -p /usr/share/flr-guest
cat >/usr/share/flr-guest/versions.env <<EOF
# The versions the flintlock-runner Guest Image carries. Written by its
# build (guest/rootfs/configure.sh). Do not edit.
UBUNTU_VERSION=$(. /etc/os-release && echo "$VERSION_ID")
GUEST_AGENT_VERSION=$GUEST_AGENT_VERSION
GITLAB_RUNNER_VERSION=$GITLAB_RUNNER_VERSION
EOF

apt-get clean
rm -rf /var/lib/apt/lists/*
