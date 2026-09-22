#!/usr/bin/env bash
# Runs /usr/libexec/flr/thin-pool against stand-ins for the LVM and block
# device tools, because neither a build container nor a CI runner has a disk
# to make a thin pool on. Installed as /usr/libexec/flr/check-thin-pool-cases
# and called by the check stage; `image/check-thin-pool.sh DIR SCRIPT` runs
# it outside the image as well.
#
# The stand-ins record every command that would change a disk in
# $S/calls, and the cases assert on that record: what matters is which
# device thin-pool decides to touch, and that it touches none when it must
# not.
set -uo pipefail
work=${1:-$(mktemp -d)}
script=${2:-/usr/libexec/flr/thin-pool}
lib=${3:-/usr/libexec/flr/lib.sh}
S=$work/thin-pool-cases
failures=0

stubs() {
  rm -rf "$S"
  mkdir -p "$S/bin" "$S/dev" "$S/run/not-ready.d"
  : >"$S/calls"
  cat >"$S/bin/stub" <<'EOF'
#!/usr/bin/env bash
# One stand-in for every tool, dispatched on the name it is called by.
S=$(dirname "$(dirname "$0")")
name=$(basename "$0")
last=${*: -1}
base=$(basename "$last")
case "$name" in
lvs)
  case "$*" in
  *lv_attr*) [ -e "$S/pool_exists" ] && { echo "  twi-aotz--"; exit 0; }; exit 5 ;;
  *) grep -qx "$last" "$S/lvs" 2>/dev/null ;;
  esac ;;
pvs)
  if grep -qx "$last" "$S/pool_pvs" 2>/dev/null; then
    case "$*" in *vg_name*) echo "  flintlock" ;; esac
    exit 0
  fi
  exit 5 ;;
vgs) [ -s "$S/pool_pvs" ] ;;
pvcreate | vgcreate | lvcreate | lvconvert | lvchange | wipefs | mkfs.ext4)
  echo "$name $*" >>"$S/calls"
  case "$name" in
  vgcreate) echo "$last" >>"$S/pool_pvs" ;;
  lvcreate) echo "flintlock/$4" >>"$S/lvs" ;; # lvcreate -q -y -n NAME ...
  esac ;;
lsblk)
  case "$1" in
  -dno) [ "$2" = TYPE ] && echo disk; [ "$2" = MODEL ] && cat "$S/model.$base" 2>/dev/null ;;
  -nro) cat "$S/mounts.$base" 2>/dev/null ;;
  -nrso) echo "$base part"; echo "nvme0n1 disk" ;;
  -no) echo nvme0n1 ;;
  esac
  exit 0 ;;
findmnt)
  case "$*" in
  "-no SOURCE /sysroot") echo "$S/dev/nvme0n1p3" ;;
  *) exit 1 ;;
  esac ;;
blkid) case "$*" in *" TYPE "*) cat "$S/blkid.$base" 2>/dev/null ;; esac; exit 0 ;;
esac
EOF
  chmod 0755 "$S/bin/stub"
  for t in lvs pvs vgs pvcreate vgcreate lvcreate lvconvert lvchange wipefs mkfs.ext4 lsblk findmnt blkid; do
    ln -s stub "$S/bin/$t"
  done
  # nvme0n1 is the root disk, nvme1n1 an instance-store disk, nvme2n1 an EBS
  # data volume.
  : >"$S/dev/nvme0n1" && : >"$S/dev/nvme0n1p3" && : >"$S/dev/nvme1n1" && : >"$S/dev/nvme2n1"
  echo "Amazon Elastic Block Store" >"$S/model.nvme0n1"
  echo "Amazon EC2 NVMe Instance Storage" >"$S/model.nvme1n1"
  echo "Amazon Elastic Block Store" >"$S/model.nvme2n1"
  printf '/sysroot\n/boot\n' >"$S/mounts.nvme0n1"
  echo ext4 >"$S/blkid.nvme0n1"
}

# run DEVICE: run thin-pool with DEVICE named in the Host configuration.
run() {
  cat >"$S/run/host.env" <<EOF
FLR_THIN_POOL_DEVICE=$1
FLR_CACHE_VOLUME_PERCENT=15
EOF
  FLR_LIB=$lib FLR_PATH=$S/bin:$PATH FLR_DEV=$S/dev FLR_BLOCK_TEST="test -e" FLR_DEVICE_WAIT=1 \
    FLR_RUN=$S/run FLR_NOT_READY_DIR=$S/run/not-ready.d FLR_HOST_ENV=$S/run/host.env \
    bash "$script" >"$S/out" 2>&1
}
pass() { printf 'ok      thin-pool: %s\n' "$1"; }
flunk() {
  printf 'FAIL    thin-pool: %s\n' "$1"
  sed 's/^/        /' "$S/out" "$S/calls" 2>/dev/null
  failures=$((failures + 1))
}
called() { grep -q -- "$1" "$S/calls"; }

# HI-021: an existing pool is left alone, whatever the configuration names.
stubs
touch "$S/pool_exists"
echo "$S/dev/nvme1n1" >"$S/pool_pvs"
# Activating the existing pool is the one thing it may do.
if run "$S/dev/nvme2n1" && ! called 'create\|lvconvert\|wipefs\|mkfs' && [ ! -e "$S/run/not-ready.d/flr-thin-pool" ]; then
  pass "an existing pool is not recreated and no device is touched"
else flunk "an existing pool is not recreated and no device is touched"; fi

# HI-020: the named device is used.
stubs
if run "$S/dev/nvme2n1" && called "pvcreate -q $S/dev/nvme2n1" && called "lvconvert .*--thinpool flintlock/thinpool" &&
  called "lvcreate .*-n thinpool flintlock -l 80%VG"; then
  pass "the pool is created on the named device"
else flunk "the pool is created on the named device"; fi

# HI-020: with none named, the instance-store disk is detected, not the EBS one.
stubs
if run "" && called "pvcreate -q $S/dev/nvme1n1" && ! called nvme2n1 && ! called nvme0n1; then
  pass "the unused instance-store disk is detected"
else flunk "the unused instance-store disk is detected"; fi

# HI-023: the root disk is never selected; the one other unused NVMe disk is.
stubs
rm -f "$S/dev/nvme2n1"
if run "$S/dev/nvme0n1" && called "pvcreate -q $S/dev/nvme1n1" && ! called nvme0n1; then
  pass "a named device that is the root disk is replaced by the one other unused disk"
else flunk "a named device that is the root disk is replaced by the one other unused disk"; fi

# HI-023: and refused when the other disk is not unambiguous.
stubs
if ! run "$S/dev/nvme0n1" && [ ! -s "$S/calls" ] && grep -q 'refusing to guess' "$S/run/not-ready.d/flr-thin-pool"; then
  pass "a named root disk with two other disks is refused"
else flunk "a named root disk with two other disks is refused"; fi

# HI-023: a device holding a mounted filesystem is never selected.
stubs
echo /data >"$S/mounts.nvme2n1"
if ! run "$S/dev/nvme2n1" && [ ! -s "$S/calls" ] && grep -q 'holds a mounted filesystem' "$S/run/not-ready.d/flr-thin-pool"; then
  pass "a named device with a mounted filesystem is refused"
else flunk "a named device with a mounted filesystem is refused"; fi
stubs
echo /scratch >"$S/mounts.nvme1n1"
if ! run "" && [ ! -s "$S/calls" ] && grep -q '^no thin pool device is available: none is named' "$S/run/not-ready.d/flr-thin-pool"; then
  pass "a mounted instance-store disk is not detected"
else flunk "a mounted instance-store disk is not detected"; fi

# A device that is not blank is refused, never wiped.
stubs
echo xfs >"$S/blkid.nvme2n1"
if ! run "$S/dev/nvme2n1" && [ ! -s "$S/calls" ] && grep -q 'is not blank' "$S/run/not-ready.d/flr-thin-pool"; then
  pass "a named device with a filesystem signature is refused, not wiped"
else flunk "a named device with a filesystem signature is refused, not wiped"; fi

# HI-023, the case of pull request #43: the disk already backs the pool's
# volume group and its cache volume is mounted, but the pool volume itself
# is missing. It is the thin pool device, not a disk in use, and it is not
# made a physical volume again.
stubs
echo "$S/dev/nvme1n1" >"$S/pool_pvs"
echo /var/lib/flintlock-runner/cache >"$S/mounts.nvme1n1"
echo LVM2_member >"$S/blkid.nvme1n1"
if run "" && ! called pvcreate && ! called vgcreate && called "lvcreate .*-n thinpool flintlock"; then
  pass "a disk that already backs the pool is the thin pool device, not in use"
else flunk "a disk that already backs the pool is the thin pool device, not in use"; fi
stubs
echo "$S/dev/nvme1n1" >"$S/pool_pvs"
echo /var/lib/flintlock-runner/cache >"$S/mounts.nvme1n1"
if run "$S/dev/nvme1n1" && ! called pvcreate && ! called vgcreate; then
  pass "and the same holds when it is the named device"
else flunk "and the same holds when it is the named device"; fi

[ "$failures" -eq 0 ]
