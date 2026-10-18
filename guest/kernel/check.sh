#!/bin/sh
# The check stage of the Guest Image's kernel (guest/kernel/Containerfile).
#
# usage: check.sh ROOT ARCH CONFIG_DIR VERSION
#
# ROOT is the image's file tree, ARCH its architecture, amd64 or arm64,
# CONFIG_DIR holds the built kernel's .config as `config` and the
# fragments it was built with, and VERSION is the kernel version the build
# pins. It needs nothing but a POSIX shell, grep and od, so it runs in
# alpine on the build machine. Every failure is reported before it exits
# non-zero.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image build SHALL check the contents of each image it
#/ builds in a check stage that fails unless the image carries everything
#/ this document requires of it.
set -u

root=${1:?usage: check.sh ROOT ARCH CONFIG_DIR VERSION}
arch=${2:?usage: check.sh ROOT ARCH CONFIG_DIR VERSION}
confdir=${3:?usage: check.sh ROOT ARCH CONFIG_DIR VERSION}
want_version=${4:?usage: check.sh ROOT ARCH CONFIG_DIR VERSION}
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
# bytes FILE OFFSET COUNT: COUNT bytes of FILE from OFFSET, as hex, no spaces.
bytes() { od -An -tx1 -j "$2" -N "$3" "$1" | tr -d ' \n'; }
config=$confdir/config
# built OPTION: the built .config has OPTION=y.
built() { grep -qx "$1=y" "$config"; }

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Image's kernel image SHALL contain, at `boot/vmlinux`, a
#/ Linux kernel of the 6.1 series built from a kernel.org source tarball with
#/ the Firecracker CI kernel configuration of the Firecracker release that
#/ the Host Image runs, as an ELF `vmlinux` for `linux/amd64` and as an
#/ uncompressed arm64 `Image` for `linux/arm64`.
kernel=$root/boot/vmlinux
if [ ! -s "$kernel" ]; then
	fail "no kernel at boot/vmlinux"
else
	ok "the kernel is at boot/vmlinux"
	case $arch in
	amd64)
		# ELF magic, 64-bit, little-endian, and e_machine 0x3e, x86-64.
		if [ "$(bytes "$kernel" 0 6)" = 7f454c460201 ] && [ "$(bytes "$kernel" 18 2)" = 3e00 ]; then
			ok "boot/vmlinux is an x86-64 ELF vmlinux"
		else
			fail "boot/vmlinux is not an x86-64 ELF vmlinux"
		fi
		;;
	arm64)
		# The arm64 Image header's magic, "ARM\x64", at offset 56.
		if [ "$(bytes "$kernel" 56 4)" = 41524d64 ]; then
			ok "boot/vmlinux is an uncompressed arm64 Image"
		else
			fail "boot/vmlinux is not an uncompressed arm64 Image"
		fi
		;;
	*) fail "no kernel check for architecture $arch" ;;
	esac
	# The banner names the version the build pins, of the 6.1 series.
	version=$(grep -aoE 'Linux version 6\.1\.[0-9]+' "$kernel" | head -1)
	version=${version#Linux version }
	case $want_version in
	6.1.*) ok "the build pins $want_version, of the 6.1 series" ;;
	*) fail "the build pins $want_version, not a 6.1 kernel" ;;
	esac
	if [ "$version" = "$want_version" ]; then
		ok "boot/vmlinux is $version"
	else
		fail "boot/vmlinux is '${version:-not a 6.1 kernel}', want $want_version"
	fi
fi
# Firecracker's CI configuration: its options that the guest relies on are
# still there after the fragments, and it has no loadable modules.
for opt in CONFIG_VIRTIO_MMIO CONFIG_VIRTIO_BLK CONFIG_VIRTIO_NET CONFIG_VIRTIO_VSOCKETS \
	CONFIG_EXT4_FS CONFIG_DEVTMPFS CONFIG_DEVTMPFS_MOUNT CONFIG_OVERLAY_FS CONFIG_BRIDGE \
	CONFIG_VETH CONFIG_CGROUPS CONFIG_MEMCG CONFIG_IP_NF_NAT CONFIG_NETFILTER_XT_MATCH_CONNTRACK \
	CONFIG_NETFILTER_XT_MATCH_ADDRTYPE; do
	if built "$opt"; then ok "$opt=y, from Firecracker's configuration"; else fail "$opt is not built in"; fi
done
if grep -qx '# CONFIG_MODULES is not set' "$config"; then
	ok "the kernel has no loadable modules: everything it needs is built in"
else
	fail "the kernel was built with CONFIG_MODULES"
fi

#= docs/requirements/13-guest-image.md#guest-agent-protection
#= type=test
#/ The Guest Image's Docker image SHALL limit the block I/O of the
#/ containers' slice on the root filesystem's block device to 1500 read and
#/ 1500 write operations per second and to 60 MB/s of reads and 60 MB/s of
#/ writes, or to the limits that `/etc/default/flr-io` sets in their place.
#
# The limit is io.max of cgroup v2's io controller: the block cgroup with
# its throttling, and cgroup writeback, which charges a container's
# buffered writes to its cgroup. Firecracker's configuration has all three.
for opt in CONFIG_BLK_CGROUP CONFIG_BLK_DEV_THROTTLING CONFIG_CGROUP_WRITEBACK; do
	if built "$opt"; then ok "$opt=y, for io.max on containers.slice"; else fail "$opt is not built in: io.max on containers.slice needs it"; fi
done

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Image's kernel SHALL have the iptables `raw` table, the
#/ iptables `comment`, `mark` and `statistic` matches and the `MARK` target
#/ built in.
for opt in CONFIG_IP_NF_RAW CONFIG_NETFILTER_XT_MATCH_COMMENT CONFIG_NETFILTER_XT_MATCH_MARK \
	CONFIG_NETFILTER_XT_TARGET_MARK CONFIG_NETFILTER_XT_MATCH_STATISTIC; do
	if built "$opt"; then ok "$opt=y"; else fail "$opt is not built in"; fi
done

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Image's kernel SHALL have virtio-pci built in beside
#/ virtio-mmio, with the virtio block, network and vsock drivers.
for opt in CONFIG_PCI CONFIG_PCI_MSI CONFIG_VIRTIO_PCI CONFIG_VIRTIO_MMIO CONFIG_VIRTIO_BLK \
	CONFIG_VIRTIO_NET CONFIG_VIRTIO_VSOCKETS; do
	if built "$opt"; then ok "$opt=y"; else fail "$opt is not built in"; fi
done
case $arch in
arm64) for opt in CONFIG_PCI_HOST_GENERIC CONFIG_ARM_GIC_V3_ITS_PCI CONFIG_SERIAL_AMBA_PL011_CONSOLE; do
	if built "$opt"; then ok "$opt=y"; else fail "$opt is not built in"; fi
done ;;
amd64) if built CONFIG_PCI_MMCONFIG; then ok "CONFIG_PCI_MMCONFIG=y"; else fail "CONFIG_PCI_MMCONFIG is not built in"; fi ;;
esac

# Every option the fragments ask for made it through olddefconfig: one that
# a dependency drops would otherwise vanish without a word.
for frag in "$confdir/flr.config" "$confdir/flr-$arch.config"; do
	if [ ! -s "$frag" ]; then
		fail "no fragment $(basename "$frag")"
		continue
	fi
	missing=$(sed -n "s/^\(CONFIG_[A-Z0-9_]*\)=y$/\1/p" "$frag" | while read -r opt; do
		built "$opt" || echo "$opt"
	done)
	if [ -z "$missing" ]; then
		ok "every option of $(basename "$frag") is built in"
	else
		fail "$(basename "$frag") asks for $(echo "$missing" | tr '\n' ' ')and the built .config does not have them"
	fi
done

# Nothing else in the image: a Profile names one file, and anything beside
# it only makes the pull longer.
extra=$(find "$root" -type f ! -path "$kernel" | head -5)
if [ -n "$extra" ]; then
	fail "the image holds more than the kernel: $extra"
else
	ok "the image holds the kernel and nothing else"
fi

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
