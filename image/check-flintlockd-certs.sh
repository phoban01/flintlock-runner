#!/usr/bin/env bash
# Checks how flintlockd waits for and follows the certificates
# battery-operator's Exec Agent writes (HI-071, HI-072): the units that
# start it once they exist and restart it when they change, and
# /usr/libexec/flr/flintlockd-certs, run against a temporary directory with
# stand-ins for systemctl and restorecon. Installed as
# /usr/libexec/flr/check-flintlockd-certs-cases and called by the check
# stage; `image/check-flintlockd-certs.sh DIR LIBEXEC UNITS DEFAULTS` runs it
# outside the image against the sources, which `make image-lint` does.
#
# What this cannot show is systemd itself watching the files and starting
# units, nor the kernel enforcing the labels: that is for a booted Host.
#
#= docs/requirements/11-host-image.md#image-flintlockd
#= type=test
#/ The Host Image SHALL start `flintlockd` only once the serving
#/ certificate, its key and the client CA bundle exist in
#/ `/etc/battery/flintlockd`, and SHALL restart `flintlockd` when the serving
#/ certificate or the client CA bundle changes.
#
#= docs/requirements/11-host-image.md#kernel-and-kvm
#= type=test
#/ The Host Image SHALL create `/etc/battery/flintlockd` owned by
#/ the Exec Agent's user id, and SHALL label that directory and every file in
#/ it so that the Exec Agent's containers can write them and `flintlockd` can
#/ read them
set -uo pipefail
work=${1:-$(mktemp -d)}
libexec=${2:-/usr/libexec/flr}
units=${3:-/usr/lib/systemd/system}
defaults=${4:-/usr/share/flr/host.conf.defaults}
C=$work/flintlockd-certs-cases
failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  failures=$((failures + 1))
}
# has FILE LINE: the unit has exactly this line.
has() { grep -qxF -- "$2" "$1"; }
rm -rf "$C"
mkdir -p "$C/run/not-ready.d" "$C/etc"
dir=$C/etc/flintlockd

# Stand-ins that record what they are asked to do.
cat >"$C/systemctl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$SYSTEMCTL_LOG"
EOF
cat >"$C/relabel" <<'EOF'
#!/usr/bin/env bash
for p in "$@"; do printf '%s\n' "$p" >>"$RELABEL_LOG"; done
exit "${RELABEL_EXIT:-0}"
EOF
chmod +x "$C/systemctl" "$C/relabel"
certs() {
  env FLR_PATH="$C:$PATH" CHOWN_LOG="$C/chown.log" FLR_LIB="$libexec/lib.sh" FLR_RUN="$C/run" FLR_NOT_READY_DIR="$C/run/not-ready.d" \
    FLR_HOST_ENV="$C/run/host.env" FLR_HOST_DEFAULTS="$defaults" FLR_HOST_CONF="$C/host.conf" \
    FLR_FLINTLOCKD_CERT_DIR="$dir" FLR_SYSTEMCTL="$C/systemctl" SYSTEMCTL_LOG="$C/systemctl.log" \
    FLR_RELABEL="$C/relabel" RELABEL_LOG="$C/relabel.log" "$@"
}
restarts() { grep -c '^try-restart flintlockd.service$' "$C/systemctl.log" 2>/dev/null || true; }

# ---- the units --------------------------------------------------------------
fl=$units/flintlockd.service
path=$units/flintlockd.path
rpath=$units/flr-flintlockd-restart.path
rsvc=$units/flr-flintlockd-restart.service
psvc=$units/flr-flintlockd-certs.service

# flintlockd is started by its path unit alone, once tls.crt, which the Exec
# Agent writes last, exists, and not before all three files do.
if has "$path" 'PathExists=/etc/battery/flintlockd/tls.crt' && has "$path" 'Unit=flintlockd.service'; then
  ok "flintlockd.path starts flintlockd once /etc/battery/flintlockd/tls.crt exists"
else
  fail "flintlockd.path does not start flintlockd on /etc/battery/flintlockd/tls.crt"
fi
if grep -q '^\[Install\]' "$fl"; then
  fail "flintlockd.service has an [Install] section, so it would start at boot without its certificates"
else
  ok "flintlockd.service is not started at boot by itself"
fi
for f in tls.crt tls.key client-ca.crt; do
  if has "$fl" "ConditionPathExists=/etc/battery/flintlockd/$f"; then ok "flintlockd.service needs $f"; else fail "flintlockd.service starts without $f"; fi
done
if has "$fl" 'ExecStartPre=/usr/libexec/flr/flintlockd-certs started'; then
  ok "flintlockd.service records the certificates it is about to load"
else
  fail "flintlockd.service does not record the certificates it loads"
fi
for dep in flr-kvm flr-thin-pool flr-network flr-flintlockd-certs; do
  if grep -Eq "^Requires=.*$dep\\.service" "$path" && grep -Eq "^After=.*$dep\\.service" "$path"; then
    ok "flintlockd.path waits for $dep, so a failed gate does not retrigger flintlockd"
  else
    fail "flintlockd.path does not require $dep"
  fi
done
# A change to the serving certificate or the client CA bundle, and nothing
# else, restarts flintlockd.
if has "$rpath" 'PathChanged=/etc/battery/flintlockd/tls.crt' && has "$rpath" 'PathChanged=/etc/battery/flintlockd/client-ca.crt' &&
  has "$rpath" 'Unit=flr-flintlockd-restart.service' && [ "$(grep -c '^Path' "$rpath")" -eq 2 ]; then
  ok "flr-flintlockd-restart.path watches tls.crt and client-ca.crt, and nothing else"
else
  fail "flr-flintlockd-restart.path does not watch exactly tls.crt and client-ca.crt"
fi
if has "$rsvc" 'ExecStart=/usr/libexec/flr/flintlockd-certs changed' && has "$rsvc" 'Type=oneshot'; then
  ok "flr-flintlockd-restart.service runs flintlockd-certs changed"
else
  fail "flr-flintlockd-restart.service does not run flintlockd-certs changed"
fi
for u in "$path" "$rpath"; do
  if has "$u" 'DefaultDependencies=no'; then ok "$(basename "$u") drops the default dependencies it would form a cycle with"; else fail "$(basename "$u") keeps its default dependencies"; fi
done
if has "$psvc" 'ExecStart=/usr/libexec/flr/flintlockd-certs prepare' && grep -Eq '^Before=.*kubelet\.service' "$psvc"; then
  ok "flr-flintlockd-certs.service makes the directory before the kubelet starts the Exec Agent"
else
  fail "flr-flintlockd-certs.service does not prepare the directory before the kubelet"
fi

# ---- prepare ------------------------------------------------------------------
# chown is a stand-in too, which records the owner it is given: the checks
# do not run as root.
cat >"$C/chown" <<'EOF2'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$CHOWN_LOG"
EOF2
chmod +x "$C/chown"
printf 'EXEC_AGENT_UID=4242\n' >"$C/host.conf"
certs bash "$libexec/host-config" >/dev/null 2>&1 || fail "host-config fails: EXEC_AGENT_UID=4242"
: >"$C/relabel.log"
: >"$C/chown.log"
if certs bash "$libexec/flintlockd-certs" prepare >"$C/out" 2>&1; then
  if [ -d "$dir" ] && [ "$(stat -c %a "$dir")" = 700 ] && grep -qx "4242:4242 $dir" "$C/chown.log"; then
    ok "prepare makes the directory, mode 0700, owned by the configured Exec Agent's user id 4242"
  else
    fail "prepare made: $(stat -c %a "$dir" 2>&1), chown $(cat "$C/chown.log")"
  fi
  if grep -qx "$dir" "$C/relabel.log"; then ok "prepare labels the directory"; else fail "prepare does not label the directory: $(cat "$C/relabel.log")"; fi
else
  fail "prepare fails: $(cat "$C/out")"
fi
printf '' >"$C/host.conf"
certs bash "$libexec/host-config" >/dev/null 2>&1
: >"$C/chown.log"
certs bash "$libexec/flintlockd-certs" prepare >/dev/null 2>&1
if grep -qx "65532:65532 $dir" "$C/chown.log"; then ok "by default prepare gives the directory to user id 65532"; else fail "by default prepare ran chown: $(cat "$C/chown.log")"; fi
if RELABEL_EXIT=1 certs bash "$libexec/flintlockd-certs" prepare >/dev/null 2>&1; then
  fail "prepare succeeds when the directory cannot be labelled"
elif grep -qs "^cannot label $dir for the Exec Agent" "$C/run/not-ready.d/flr-flintlockd-certs"; then
  ok "prepare fails, and says why, when the directory cannot be labelled"
else
  fail "prepare fails without a reason when the directory cannot be labelled"
fi
certs bash "$libexec/flintlockd-certs" prepare >/dev/null 2>&1
if [ -e "$C/run/not-ready.d/flr-flintlockd-certs" ]; then fail "a successful prepare leaves its reason behind"; else ok "a successful prepare clears its reason"; fi

# ---- started and changed --------------------------------------------------------
# write NAME CONTENT replaces a file the way the Exec Agent does, through a
# temporary file and a rename.
write() {
  printf '%s\n' "$2" >"$dir/.$1.tmp"
  mv -f "$dir/.$1.tmp" "$dir/$1"
}
write client-ca.crt ca-1
write tls.key key-1
write tls.crt cert-1
: >"$C/systemctl.log"
certs bash "$libexec/flintlockd-certs" started >/dev/null 2>&1
certs bash "$libexec/flintlockd-certs" changed >/dev/null 2>&1
if [ "$(restarts)" = 0 ]; then ok "files that are the ones flintlockd loaded do not restart it"; else fail "flintlockd was restarted for the files it loaded"; fi
write tls.key key-2
write tls.crt cert-2
: >"$C/relabel.log"
certs bash "$libexec/flintlockd-certs" changed >/dev/null 2>&1
if [ "$(restarts)" = 1 ]; then ok "a renewed serving certificate restarts flintlockd"; else fail "a renewed serving certificate restarted flintlockd $(restarts) times"; fi
if grep -qx "$dir" "$C/relabel.log" && grep -qx "$dir/tls.crt" "$C/relabel.log" && grep -qx "$dir/tls.key" "$C/relabel.log" &&
  grep -qx "$dir/client-ca.crt" "$C/relabel.log"; then
  ok "the change labels the directory and every file in it again, the key among them"
else
  fail "the change labelled: $(xargs <"$C/relabel.log")"
fi
# Labelling the files is a change the path unit sees: the run it starts
# finds nothing new and leaves flintlockd alone.
certs bash "$libexec/flintlockd-certs" changed >/dev/null 2>&1
if [ "$(restarts)" = 1 ]; then ok "the change a relabel makes does not restart flintlockd again"; else fail "the relabel's own change restarted flintlockd"; fi
write client-ca.crt ca-2
certs bash "$libexec/flintlockd-certs" changed >/dev/null 2>&1
if [ "$(restarts)" = 2 ]; then ok "a new client CA bundle restarts flintlockd"; else fail "a new client CA bundle restarted flintlockd $(( $(restarts) - 1 )) times"; fi
touch "$dir/tls.crt"
certs bash "$libexec/flintlockd-certs" changed >/dev/null 2>&1
if [ "$(restarts)" = 2 ]; then ok "a file touched but unchanged does not restart flintlockd"; else fail "a touched file restarted flintlockd"; fi
if grep -Eq '^(restart|start|stop) ' "$C/systemctl.log"; then
  fail "flintlockd-certs did more than try-restart: $(cat "$C/systemctl.log")"
else
  ok "flintlockd-certs only ever try-restarts flintlockd, so a flintlockd that is not running is left to flintlockd.path"
fi

rm -rf "$C"
[ "$failures" -eq 0 ]
