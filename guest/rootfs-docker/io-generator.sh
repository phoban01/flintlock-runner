#!/bin/sh
# flr-io, a systemd generator installed as
# /usr/lib/systemd/system-generators/flr-io in the Guest Image's Docker
# image (guest/rootfs-docker): the block I/O limits of containers.slice
# from /etc/default/flr-io, when that file sets any.
#
# containers.slice limits the containers' block I/O on the root
# filesystem's device to 1500 read and 1500 write operations per second
# and 60 MB/s each way (GI-072), so that a container cannot keep the
# MicroVM's block queue full and hold back the guest agent's heartbeats
# (#133). A Host whose thin pool completes more can raise or remove a
# limit with /etc/default/flr-io, which the image does not have; a derived
# image adds it:
#
#   FLR_IO_READ_IOPS=6000      # read operations per second
#   FLR_IO_WRITE_IOPS=6000     # write operations per second
#   FLR_IO_READ_BPS=250M       # bytes per second of reads; K, M, G and T
#   FLR_IO_WRITE_BPS=infinity  #   are powers of 1000; infinity is no limit
#
# systemd runs its generators at boot and on every daemon-reload, before
# it loads any unit, with three output directories. This one writes a
# drop-in for containers.slice into the first, of normal priority, with
# one IO directive per key the file sets. A key with a value that is not
# digits with an optional K, M, G or T, or infinity, is reported and
# skipped. The file is read, not run. FLR_IO_DEFAULTS names a file to read
# in place of /etc/default/flr-io; the check stage uses it.
set -u

normal=${1:?usage: flr-io NORMAL_DIR EARLY_DIR LATE_DIR}
defaults=${FLR_IO_DEFAULTS:-/etc/default/flr-io}
[ -r "$defaults" ] || exit 0

# value KEY prints the value of the last KEY=VALUE line of the defaults
# file, without quotes or spaces around it.
value() {
	sed -n "s/^[[:space:]]*$1=[[:space:]]*[\"']\{0,1\}\([^\"']*\)[\"']\{0,1\}[[:space:]]*$/\1/p" "$defaults" | tail -n 1
}

# valid VALUE succeeds when VALUE is what systemd's IO limits take: digits
# with an optional K, M, G or T, or infinity.
valid() {
	case $1 in
	infinity) return 0 ;;
	*[!0-9KMGT]*) return 1 ;;
	[0-9]*)
		case ${1%[KMGT]} in *[!0-9]*) return 1 ;; esac
		return 0
		;;
	esac
	return 1
}

#= docs/requirements/13-guest-image.md#guest-agent-protection
#/ The Guest Image's Docker image SHALL limit the block I/O of the
#/ containers' slice on the root filesystem's block device to 1500 read and
#/ 1500 write operations per second and to 60 MB/s of reads and 60 MB/s of
#/ writes, or to the limits that `/etc/default/flr-io` sets in their place.
out=''
for pair in FLR_IO_READ_IOPS:IOReadIOPSMax FLR_IO_WRITE_IOPS:IOWriteIOPSMax \
	FLR_IO_READ_BPS:IOReadBandwidthMax FLR_IO_WRITE_BPS:IOWriteBandwidthMax; do
	key=${pair%%:*}
	directive=${pair#*:}
	v=$(value "$key")
	[ -n "$v" ] || continue
	if ! valid "$v"; then
		echo "flr-io: $key=$v in $defaults is not digits with an optional K, M, G or T, or infinity; skipped" >&2
		continue
	fi
	out="$out$directive=/ $v
"
done
[ -n "$out" ] || exit 0

mkdir -p "$normal/containers.slice.d"
{
	printf '# Written at boot by /usr/lib/systemd/system-generators/flr-io from %s.\n' "$defaults"
	printf '[Slice]\n%s' "$out"
} >"$normal/containers.slice.d/50-flr-io.conf"
