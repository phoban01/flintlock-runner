#!/usr/bin/env bash
# The tests of render-manifests.sh and check-manifests.sh, against the
# stand-in kustomizations in testdata/. It needs kustomize and nothing else,
# and the packaging dry run runs it on every pull request that changes the
# packaging. The digests are synthetic test data, not of any image.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

flr=ghcr.io/phoban01/flintlock-runner/flr@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
other=ghcr.io/phoban01/flintlock-runner/flr@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd

failures=0
pass() { echo "ok   $*"; }
fail() { echo "FAIL $*"; failures=$((failures + 1)); }
expect_ok() { # expect_ok <name> <command...>
	local name=$1; shift
	if "$@" >"$work/log" 2>&1; then pass "$name"; else fail "$name"; cat "$work/log"; fi
}
expect_fail() { # expect_fail <name> <pattern the output must contain> <command...>
	local name=$1 pattern=$2; shift 2
	if "$@" >"$work/log" 2>&1; then
		fail "$name: succeeded"; cat "$work/log"
	elif ! grep -q -- "$pattern" "$work/log"; then
		fail "$name: failed without saying \"$pattern\""; cat "$work/log"
	else
		pass "$name"
	fi
}

#= docs/requirements/12-cluster-fleet.md#cluster-release
#= type=test
#/ The Release SHALL publish the Fleet Manifests as one release asset
#/ for the workload cluster, in which every container image of this project
#/ is referenced by digest.

# The fleet renders into one file in which every flr reference, the image
# fields of init containers and of a DaemonSet included, is this release's
# digest, and a third-party reference by digest is left as it was.
expect_ok "render the stand-in deploy/" \
	"$here/render-manifests.sh" --flr "$flr" --images "$here/testdata/images.txt" --deploy "$here/testdata/deploy" --out "$work/out"
fleet=$work/out/flintlock-runner-fleet.yaml
if [ -s "$fleet" ]; then pass "the fleet asset written"; else fail "the fleet asset written"; fi
if [ -e "$work/out/flintlock-runner-capi.yaml" ]; then fail "no Cluster API asset"; else pass "no Cluster API asset"; fi
n=$(grep -c "image: $flr\$" "$fleet" || true)
if [ "$n" = 3 ]; then pass "all three flr image fields by this release's digest"; else fail "flr image fields by digest: $n of 3"; fi
if grep -q ':dev' "$fleet"; then fail "no :dev tag left in the fleet"; else pass "no :dev tag left in the fleet"; fi
if grep -q 'registry.example/other@sha256:c' "$fleet"; then pass "third-party digest untouched"; else fail "third-party digest untouched"; fi

# A tag where kustomize cannot rewrite it is refused, and nothing is written.
cp -r "$here/testdata/tagged" "$work/tagged"
expect_fail "a tag outside an image field is refused" "flr:dev is not a reference by digest" \
	"$here/render-manifests.sh" --flr "$flr" --deploy "$work/tagged" --out "$work/tagged-out"
if [ -e "$work/tagged-out/flintlock-runner-fleet.yaml" ]; then fail "a refused asset is not written"; else pass "a refused asset is not written"; fi

# The arguments must be references by digest into the project's repositories.
expect_fail "--flr by tag is refused" "not a reference by digest" \
	"$here/render-manifests.sh" --flr ghcr.io/phoban01/flintlock-runner/flr:1.0.0 --deploy "$here/testdata/deploy" --out "$work/x"
expect_fail "--flr in another repository is refused" "must be in ghcr.io/phoban01/flintlock-runner/flr" \
	"$here/render-manifests.sh" --flr "ghcr.io/someone/flr@${flr#*@}" --deploy "$here/testdata/deploy" --out "$work/x"
expect_fail "a missing kustomization is refused" "has no kustomization.yaml" \
	"$here/render-manifests.sh" --flr "$flr" --deploy "$work" --out "$work/x"

# The Guest Image in the Runner's configuration takes the digests of the
# release's images.txt, and the ConfigMap generated from it carries them.
kernel=ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:1111111111111111111111111111111111111111111111111111111111111111
rootfs=ghcr.io/phoban01/flintlock-runner/guest-rootfs@sha256:2222222222222222222222222222222222222222222222222222222222222222
if grep -q "image: $kernel\$" "$fleet" && grep -q "rootfs: $rootfs\$" "$fleet"; then pass "the Guest Image by the digests of images.txt"; else fail "the Guest Image by the digests of images.txt"; fi
if grep -q 'guest-[a-z]*:REPLACE' "$fleet"; then fail "no :REPLACE left in the fleet"; else pass "no :REPLACE left in the fleet"; fi
if grep -q 'guest-kernel:REPLACE' "$here/testdata/deploy/config.yaml"; then pass "the source is not changed"; else fail "the source is not changed"; fi

# Without images.txt the Guest Image keeps its tag, and the render refuses
# it; images.txt with two digests for one Guest Image is refused as well.
expect_fail "the Guest Image by tag is refused" "guest-kernel:REPLACE is not a reference by digest" \
	"$here/render-manifests.sh" --flr "$flr" --deploy "$here/testdata/deploy" --out "$work/no-images"
if [ -e "$work/no-images/flintlock-runner-fleet.yaml" ]; then fail "a fleet with the Guest Image by tag is not written"; else pass "a fleet with the Guest Image by tag is not written"; fi
{ cat "$here/testdata/images.txt"; echo "ghcr.io/phoban01/flintlock-runner/guest-kernel:other ghcr.io/phoban01/flintlock-runner/guest-kernel@sha256:3333333333333333333333333333333333333333333333333333333333333333"; } >"$work/two-kernels.txt"
expect_fail "images.txt with two guest-kernel digests is refused" "not one" \
	"$here/render-manifests.sh" --flr "$flr" --images "$work/two-kernels.txt" --deploy "$here/testdata/deploy" --out "$work/x"
grep -v guest-rootfs "$here/testdata/images.txt" >"$work/no-rootfs.txt"
expect_fail "images.txt without guest-rootfs is refused" "guest-rootfs, not one" \
	"$here/render-manifests.sh" --flr "$flr" --images "$work/no-rootfs.txt" --deploy "$here/testdata/deploy" --out "$work/x"

# check-manifests on its own.
printf 'image: %s\n' "$flr" >"$work/good.yaml"
expect_ok "a reference by this release's digest passes" \
	"$here/check-manifests.sh" --flr "$flr" --require-flr "$work/good.yaml"
printf 'image: %s\n' "$other" >"$work/stale.yaml"
expect_fail "another release's digest fails" "is not this release's" \
	"$here/check-manifests.sh" --flr "$flr" "$work/stale.yaml"
printf 'image: "ghcr.io/phoban01/flintlock-runner/flr:1.0.0"\n' >"$work/tag.yaml"
expect_fail "a quoted version tag fails" "flr:1.0.0 is not a reference by digest" \
	"$here/check-manifests.sh" "$work/tag.yaml"
printf 'args: [--image=ghcr.io/phoban01/flintlock-runner/flr]\n' >"$work/bare.yaml"
expect_fail "a bare repository name fails" "flintlock-runner/flr is not a reference by digest" \
	"$here/check-manifests.sh" "$work/bare.yaml"
printf 'image: ghcr.io/phoban01/flintlock-runner/flr:1.0.0@%s\n' "${flr#*@}" >"$work/tagdigest.yaml"
expect_fail "a tag and a digest together fail" "is not a reference by digest" \
	"$here/check-manifests.sh" "$work/tagdigest.yaml"
printf 'kind: ConfigMap\n' >"$work/none.yaml"
expect_fail "a fleet without the flr image fails" "does not reference" \
	"$here/check-manifests.sh" --flr "$flr" --require-flr "$work/none.yaml"
expect_fail "an empty asset fails" "missing or empty" \
	"$here/check-manifests.sh" "$work/nothing.yaml"
printf 'image: %s\n' "${kernel%@*}@sha256:3333333333333333333333333333333333333333333333333333333333333333" >"$work/stale-kernel.yaml"
expect_fail "another release's Guest Image digest fails" "is not this release's" \
	"$here/check-manifests.sh" --flr "$flr" --image "$kernel" "$work/stale-kernel.yaml"

if [ "$failures" -ne 0 ]; then
	echo "test-render: $failures failure(s)"
	exit 1
fi
echo "test-render: all passed"
