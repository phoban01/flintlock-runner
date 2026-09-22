#!/usr/bin/env bash
# deploy/check.sh: `make manifests-check`. Renders the Fleet Manifests,
# validates every object against the Kubernetes 1.35 schemas and the Cluster
# API and CAPA CRD schemas with kubeconform, runs the Host Agent's render,
# cache and pre-warm scripts against a sample Host, and then runs the
# targeted checks of deploy/tests/checks.yaml, which carry the requirement
# citations (duvet reads deploy/**/*.yaml, not this file).
#
# Tools come from the environment, as the Makefile pins and installs them:
#   KUSTOMIZE, KUBECONFORM, YQ  the binaries
#   FLR                         an flr binary, for loading the rendered
#                               Runner and Exec Agent configurations
#   K8S_SCHEMA_VERSION          the Kubernetes schema version, v1.35.x
#   K8S_SCHEMA_LOCATION         kubeconform's location for the built-in kinds
#   CRD_SCHEMA_LOCATION         kubeconform's location for the CRDs
#   CHECK_WORK                  a scratch directory (default: mktemp)
#   CHECK_ONLY                  run only the checks whose name matches this
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
deploy=$root/deploy

: "${KUSTOMIZE:=kustomize}"
: "${KUBECONFORM:=kubeconform}"
: "${YQ:=yq}"
: "${FLR:=}"
: "${K8S_SCHEMA_VERSION:=1.35.8}"
: "${K8S_SCHEMA_LOCATION:=default}"
: "${CRD_SCHEMA_LOCATION:=https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json}"

if [ -n "${CHECK_WORK:-}" ]; then
  work=$CHECK_WORK
  rm -rf "$work"
  mkdir -p "$work"
else
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
fi

log() { printf '%s\n' "$*" >&2; }
die() {
  log "manifests-check: $*"
  exit 1
}

for tool in "$KUSTOMIZE" "$KUBECONFORM" "$YQ"; do
  command -v "$tool" >/dev/null || die "$tool is not installed; run it through make manifests-check"
done

# yq over a multi-document file prints a separator for every document; the
# checks want the values alone.
yq_real=$(command -v "$YQ")
printf '#!/bin/sh\nexec "%s" --no-doc "$@"\n' "$yq_real" >"$work/yq"
chmod +x "$work/yq"
YQ=$work/yq

# ---- the scripts ----
# The Host Agent's scripts run under busybox sh on the Host, so they are
# checked as POSIX sh. CI requires shellcheck; a laptop without it skips.
scripts=("$deploy/check.sh" "$deploy"/host-agent/config/*.sh)
if command -v shellcheck >/dev/null; then
  shellcheck "${scripts[@]}" || die "shellcheck failed"
elif [ -n "${MANIFESTS_REQUIRE_SHELLCHECK:-}" ]; then
  die "shellcheck is not installed"
else
  log "shellcheck is not installed; skipping it"
fi

# ---- render ----
# The Runner is not in deploy/ until the claim backend exists
# (deploy/runner/kustomization.yaml), so it is rendered and checked on its
# own.
fleet=$work/fleet.yaml
capi=$work/capi.yaml
runner=$work/runner.yaml
"$KUSTOMIZE" build "$deploy" >"$fleet"
"$KUSTOMIZE" build "$deploy/capi" >"$capi"
"$KUSTOMIZE" build "$deploy/runner" >"$runner"
log "rendered deploy/ ($(grep -c '^kind:' "$fleet") objects), deploy/capi ($(grep -c '^kind:' "$capi") objects) and deploy/runner ($(grep -c '^kind:' "$runner") objects)"

# The test overlays under deploy/tests, each rendered to <name>.yaml.
for k in "$deploy"/tests/*/kustomization.yaml; do
  [ -e "$k" ] || continue
  d=$(dirname "$k")
  "$KUSTOMIZE" build "$d" >"$work/$(basename "$d").yaml"
done

# ---- schemas ----
kubeconform_run() {
  "$KUBECONFORM" -strict -summary -output text \
    -kubernetes-version "$K8S_SCHEMA_VERSION" \
    -schema-location "$K8S_SCHEMA_LOCATION" \
    -schema-location "$CRD_SCHEMA_LOCATION" \
    "$@"
}
kubeconform_run "$fleet" "$capi" "$runner" >"$work/kubeconform.txt" 2>&1 || {
  cat "$work/kubeconform.txt" >&2
  die "kubeconform failed"
}
cat "$work/kubeconform.txt" >&2
# A kind without a schema is skipped, not failed, unless -ignore-missing-schemas
# is off, which it is; still, refuse any skip outright.
if grep -q 'Skipped: [1-9]' "$work/kubeconform.txt"; then
  die "kubeconform skipped objects it had no schema for"
fi

# ---- the Host Agent's scripts against a sample Host ----
# extract_config FILE DIR writes every entry of the Host Agent's ConfigMap in
# FILE into DIR, as the pod's volume would.
extract_config() {
  mkdir -p "$2"
  local keys k
  keys=$("$YQ" 'select(.kind == "ConfigMap" and (.metadata.name | test("^flintlock-host-agent-config-"))) | .data | keys | .[]' "$1")
  for k in $keys; do
    K=$k "$YQ" 'select(.kind == "ConfigMap" and (.metadata.name | test("^flintlock-host-agent-config-"))) | .data[strenv(K)]' "$1" >"$2/$k"
  done
}
templates=$work/templates
extract_config "$fleet" "$templates"

# A Host as flr-host-config describes it in /run/flr/host.env.
sample_host_env() {
  cat <<EOF
# Written by flr-host-config.service at every boot. Do not edit.
FLR_GUEST_SUBNET=10.200.0.0/16
FLR_PREFIX=16
FLR_NETMASK=255.255.0.0
FLR_GATEWAY=10.200.0.1
FLR_DHCP_START=10.200.0.10
FLR_DHCP_END=10.200.255.254
FLR_THIN_POOL_DEVICE=
FLR_PROTECTED_CIDRS=10.0.0.0/16
FLR_HOST_RESERVE_VCPU=3
FLR_HOST_RESERVE_MEMORY_MB=6144
FLR_HOST_SERVICE_PORTS=1234,3000,5000,3128
FLR_HOST_CONTROL_PORTS=9090,8090,10248,10250,10255,10256,10260,10270,9252,1338
FLR_CACHE_VOLUME_PERCENT=15
FLR_POD_PROVIDER_UID=10250
FLR_HOST_SERVICE_UIDS=101,1000,10001,10002,100000-165535
EOF
}
sample_host_env >"$work/host.env"
gateway=10.200.0.1

# render_to TEMPLATES OUT runs render.sh as the render init container does.
render_to() {
  FLR_TEMPLATES=$1 FLR_HOST_ENV=$work/host.env FLR_OUT=$2 sh "$1/render.sh" >"$2.log" 2>&1 || {
    cat "$2.log" >&2
    return 1
  }
}
rendered=$work/rendered
render_to "$templates" "$rendered" || die "render.sh failed on the default configuration"

# The same with private Go modules and HTTP cache upstreams configured, so
# that the parts of render.sh that are off by default are exercised too.
templates_full=$work/templates-full
cp -r "$templates" "$templates_full"
cat >"$templates_full/go-proxy.env" <<'EOF'
UPSTREAM=https://proxy.golang.org
PRIVATE_PATTERNS=gitlab.example.com/Platform/*,gitlab.example.com/tools
PRIVATE_VCS_HOST=gitlab.example.com
PRIVATE_REVALIDATE_SECONDS=3600
EOF
cat >"$templates_full/http-cache.upstreams" <<'EOF'
ubuntu https://archive.ubuntu.com/ubuntu 20g 86400
alpine_cdn https://dl-cdn.alpinelinux.org/alpine/ 5g 3600
EOF
cat >"$templates_full/registry-mirror.upstreams" <<'EOF'
https://registry-1.docker.io
https://ghcr.io
EOF
rendered_full=$work/rendered-full
render_to "$templates_full" "$rendered_full" || die "render.sh failed on the full configuration"

# ---- the targeted checks ----
checks=$deploy/tests/checks.yaml
n=$("$YQ" '.checks | length' "$checks")
[ "$n" -gt 0 ] || die "$checks has no checks"

# Helpers the check bodies use. q FILE EXPR evaluates a yq expression.
cat >"$work/helpers.sh" <<'EOF'
q() { "$YQ" "$2" "$1"; }
fail() {
  printf '    %s\n' "$*" >&2
  exit 1
}
# obj FILE KIND NAME: one object of FILE, as YAML.
obj() { K=$2 N=$3 "$YQ" 'select(.kind == strenv(K) and .metadata.name == strenv(N))' "$1"; }
# seconds DURATION: a Go duration of whole h, m and s units in seconds.
seconds() {
  local d=$1 total=0 num
  while [ -n "$d" ]; do
    num=${d%%[hms]*}
    case $num in '' | *[!0-9]*) echo "bad duration $1" >&2; return 1 ;; esac
    d=${d#"$num"}
    case $d in
    h*) total=$((total + num * 3600)) ;;
    m*) total=$((total + num * 60)) ;;
    s*) total=$((total + num)) ;;
    esac
    d=${d#?}
  done
  echo "$total"
}
EOF

export YQ KUSTOMIZE KUBECONFORM FLR root deploy work fleet capi runner templates templates_full rendered rendered_full gateway

failed=0
passed=0
for ((i = 0; i < n; i++)); do
  name=$(I=$i "$YQ" '.checks[env(I)].name' "$checks")
  if [ -n "${CHECK_ONLY:-}" ] && [[ $name != *"$CHECK_ONLY"* ]]; then
    continue
  fi
  # Each check is a bash process of its own with errexit on. It is not run
  # in an `if` or `||` context, where bash would ignore errexit inside it and
  # only its last command would count.
  {
    echo 'set -euo pipefail'
    cat "$work/helpers.sh"
    I=$i "$YQ" '.checks[env(I)].run' "$checks"
  } >"$work/check.sh"
  set +e
  (cd "$work" && bash "$work/check.sh") >"$work/check.out" 2>&1
  status=$?
  set -e
  if [ "$status" -eq 0 ]; then
    passed=$((passed + 1))
    log "ok    $name"
  else
    failed=$((failed + 1))
    log "FAIL  $name"
    sed 's/^/      /' "$work/check.out" >&2
  fi
done

log "manifests-check: $passed passed, $failed failed"
[ "$failed" -eq 0 ]
