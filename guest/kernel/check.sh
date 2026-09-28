#!/bin/sh
# The check stage of the Guest Image's kernel (guest/kernel/Containerfile).
#
# usage: check.sh ROOT ARCH
#
# ROOT is the image's file tree and ARCH its architecture, amd64 or arm64.
# It needs nothing but a POSIX shell and od, so it runs in alpine on the
# build machine. Every failure is reported before it exits non-zero.
#
#= docs/requirements/13-guest-image.md#guest-build
#= type=test
#/ The Guest Image build SHALL check the contents of each image it
#/ builds in a check stage that fails unless the image carries everything
#/ this document requires of it.
set -u

root=${1:?usage: check.sh ROOT ARCH}
arch=${2:?usage: check.sh ROOT ARCH}
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}
# bytes FILE OFFSET COUNT: COUNT bytes of FILE from OFFSET, as hex, no spaces.
bytes() { od -An -tx1 -j "$2" -N "$3" "$1" | tr -d ' \n'; }

#= docs/requirements/13-guest-image.md#guest-kernel
#= type=test
#/ The Guest Image's kernel image SHALL contain Firecracker's CI
#/ kernel of the 6.1 series for its architecture at `boot/vmlinux`, as an
#/ ELF `vmlinux` for `linux/amd64` and as an uncompressed arm64 `Image` for
#/ `linux/arm64`.
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
	# Firecracker's CI kernels of the 6.1 series say so in their banner.
	version=$(grep -aoE 'Linux version 6\.1\.[0-9]+' "$kernel" | head -1)
	if [ -n "$version" ]; then
		ok "boot/vmlinux is ${version#Linux version }"
	else
		fail "boot/vmlinux is not a 6.1 kernel"
	fi
fi

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
