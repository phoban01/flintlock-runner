#!/usr/bin/env bash
# Usage: image/lint.sh
#
# Lints the Host Image sources without building anything: `bash -n` and
# the shellcheck tool over every script, with the settings internal/fleet/scripts
# uses for the provisioning scripts; the digest pin and the versions file;
# the thin-pool cases against stand-in tools; and `systemd-analyze verify`
# where it is installed. FLINTLOCK_RUNNER_REQUIRE_SHELLCHECK=1, which CI
# sets, makes a missing shellcheck a failure instead of a skip.
set -uo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
cd "$here/.." || exit 1
failed=0
fail() {
  echo "FAIL  $*"
  failed=1
}

scripts=(image/check.sh image/check-thin-pool.sh image/check-labels.sh image/lint.sh image/publish-ami.sh
  image/build/*.sh image/rootfs/usr/libexec/flr/*)
for s in "${scripts[@]}"; do
  bash -n "$s" || fail "bash -n $s"
done
echo "ok    bash -n: ${#scripts[@]} scripts"

if command -v shellcheck >/dev/null 2>&1; then
  if shellcheck --shell=bash --severity=style --external-sources "${scripts[@]}"; then
    echo "ok    shellcheck"
  else
    fail shellcheck
  fi
elif [ -n "${FLINTLOCK_RUNNER_REQUIRE_SHELLCHECK:-}" ]; then
  fail "FLINTLOCK_RUNNER_REQUIRE_SHELLCHECK is set but shellcheck is not installed"
else
  echo "skip  shellcheck is not installed"
fi

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image SHALL be defined by one Containerfile whose base
#/ is a bootc base image pinned by digest.
n=$(find image -name 'Containerfile*' -o -name 'Dockerfile*' | wc -l)
if [ "$n" -eq 1 ]; then echo "ok    one Containerfile"; else fail "$n Containerfiles under image/"; fi
# Every FROM names either an earlier stage or an image by sha256 digest.
stages=" "
while read -r _ a b c d; do
  ref=$a
  case "$a" in --platform=*) ref=$b; b=$c; c=$d ;; esac
  case "$ref" in
  *@sha256:????????????????????????????????????????????????????????????????) echo "ok    FROM $ref is pinned by digest" ;;
  *) case "$stages" in *" $ref "*) ;; *) fail "FROM $ref is neither a stage nor pinned by digest" ;; esac ;;
  esac
  [ "${b,,}" = as ] && stages="$stages$c "
done < <(grep -E '^FROM ' image/Containerfile)
grep -Eq '^FROM --platform=linux/amd64 .*bootc@sha256:' image/Containerfile || fail "the base is not a bootc image for linux/amd64"

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ The Host Image build SHALL verify the checksum of every binary
#/ it downloads against a checksum recorded in the versions file and SHALL
#/ fail when one does not match.
# Every download of the build goes through fetch(), which checks before use,
# and the versions file is nothing but KEY=value lines.
if grep -nE '\b(curl|wget)\b' image/build/*.sh image/Containerfile | grep -v '^image/build/fetch.sh:[0-9]*:  curl --proto' | grep -v '^[^:]*:[0-9]*:#' | grep -q .; then
  fail "a download outside fetch() in image/build/fetch.sh"
else
  echo "ok    every download goes through fetch()"
fi
if grep -vE '^(#.*|[A-Z][A-Z0-9_]*=[A-Za-z0-9._-]*|)$' image/versions.env | grep -q .; then
  fail "image/versions.env has a line that is not KEY=value"
else
  echo "ok    image/versions.env is KEY=value lines"
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if image/check-thin-pool.sh "$tmp" image/rootfs/usr/libexec/flr/thin-pool image/rootfs/usr/libexec/flr/lib.sh; then
  echo "ok    thin-pool cases"
else
  fail "thin-pool cases"
fi

#= docs/requirements/11-host-image.md#image-build
#= type=test
#/ Where publishing an AMI is requested, the Host Image build SHALL
#/ convert the container image to an AMI with `bootc-image-builder` and SHALL
#/ tag the AMI with the container image digest and the pinned Kubernetes
#/ version.
# A dry run: what the script would run, without podman, AWS or an image.
# It shows the commands are formed as intended, not that AWS accepts them;
# nothing has ever run this script for real.
digest=sha256:$(printf 'a%.0s' $(seq 1 64))
k8s=$(sed -n 's/^KUBERNETES_VERSION=//p' image/versions.env)
ami_out=$(IMAGE_DIGEST=$digest image/publish-ami.sh registry.example/host:1 --bucket b --region eu-west-1 --dry-run 2>&1)
for want in "bootc-image-builder@sha256:" "--type ami" "--aws-bucket b" "--aws-region eu-west-1" "registry.example/host:1" \
  "create-tags" "Key=gitlab-runner.flintlock.dev/image-digest,Value=$digest" \
  "Key=gitlab-runner.flintlock.dev/kubernetes-version,Value=$k8s"; do
  if printf '%s\n' "$ami_out" | tr -d "\\\\" | grep -qF -- "$want"; then
    echo "ok    publish-ami dry run has: $want"
  else
    fail "publish-ami dry run lacks: $want"
  fi
done
# Only when requested: no build target and no workflow runs it.
if grep -n 'publish-ami\|image-ami' Makefile .github/workflows/*.yml | grep -v '^Makefile:[0-9]*:\(#\|image-ami:\|\.PHONY\|	CONTAINER_ENGINE=.*image/publish-ami.sh\)' | grep -q .; then
  fail "something other than the image-ami target runs publish-ami.sh"
else
  echo "ok    only make image-ami runs publish-ami.sh"
fi

if command -v systemd-analyze >/dev/null 2>&1; then
  # Outside the image the binaries and most dependencies are absent, so
  # only syntax problems are taken from the output.
  out=$(systemd-analyze verify --man=no image/rootfs/usr/lib/systemd/system/*.service 2>&1 |
    grep -E 'Unknown (key|section)|Failed to parse|Invalid|Missing|ignoring line' || true)
  if [ -z "$out" ]; then echo "ok    systemd-analyze verify finds no syntax problem"; else fail "systemd-analyze verify: $out"; fi
else
  echo "skip  systemd-analyze is not installed"
fi

exit "$failed"
