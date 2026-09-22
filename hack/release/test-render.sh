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
host=ghcr.io/phoban01/flintlock-runner/host@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
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
#/ The Release SHALL publish the Fleet Manifests as one release
#/ asset in which every container image of this project is referenced by
#/ digest.

# The fleet renders into one file in which every flr reference, the image
# fields of init containers and of a DaemonSet included, is this release's
# digest, and a third-party reference by digest is left as it was.
expect_ok "render the stand-in deploy/" \
	"$here/render-manifests.sh" --flr "$flr" --host "$host" --deploy "$here/testdata/deploy" --out "$work/out"
fleet=$work/out/flintlock-runner-fleet.yaml
capi=$work/out/flintlock-runner-capi.yaml
if [ -s "$fleet" ] && [ -s "$capi" ]; then pass "both assets written"; else fail "both assets written"; fi
n=$(grep -c "image: $flr\$" "$fleet" || true)
if [ "$n" = 3 ]; then pass "all three flr image fields by this release's digest"; else fail "flr image fields by digest: $n of 3"; fi
if grep -q ':dev' "$fleet"; then fail "no :dev tag left in the fleet"; else pass "no :dev tag left in the fleet"; fi
if grep -q 'registry.example/other@sha256:c' "$fleet"; then pass "third-party digest untouched"; else fail "third-party digest untouched"; fi
if grep -q 'kind: MachineDeployment' "$capi"; then pass "Cluster API objects in their own asset"; else fail "Cluster API objects in their own asset"; fi
if grep -q 'kind: MachineDeployment' "$fleet"; then fail "Cluster API objects kept out of the fleet asset"; else pass "Cluster API objects kept out of the fleet asset"; fi

# A tag where kustomize cannot rewrite it is refused, and nothing is written.
cp -r "$here/testdata/tagged" "$work/tagged"
cp -r "$here/testdata/deploy/capi" "$work/tagged/capi"
expect_fail "a tag outside an image field is refused" "host:dev is not a reference by digest" \
	"$here/render-manifests.sh" --flr "$flr" --host "$host" --deploy "$work/tagged" --out "$work/tagged-out"
if [ -e "$work/tagged-out/flintlock-runner-fleet.yaml" ]; then fail "a refused asset is not written"; else pass "a refused asset is not written"; fi

# The arguments must be references by digest into the project's repositories.
expect_fail "--flr by tag is refused" "not a reference by digest" \
	"$here/render-manifests.sh" --flr ghcr.io/phoban01/flintlock-runner/flr:1.0.0 --host "$host" --deploy "$here/testdata/deploy" --out "$work/x"
expect_fail "--flr in another repository is refused" "must be in ghcr.io/phoban01/flintlock-runner/flr" \
	"$here/render-manifests.sh" --flr "ghcr.io/someone/flr@${flr#*@}" --host "$host" --deploy "$here/testdata/deploy" --out "$work/x"
expect_fail "a missing kustomization is refused" "has no kustomization.yaml" \
	"$here/render-manifests.sh" --flr "$flr" --host "$host" --deploy "$work" --out "$work/x"

# check-manifests on its own.
printf 'image: %s\n' "$flr" >"$work/good.yaml"
expect_ok "a reference by this release's digest passes" \
	"$here/check-manifests.sh" --flr "$flr" --host "$host" --require-flr "$work/good.yaml"
printf 'image: %s\n' "$other" >"$work/stale.yaml"
expect_fail "another release's digest fails" "is not this release's" \
	"$here/check-manifests.sh" --flr "$flr" --host "$host" "$work/stale.yaml"
printf 'image: "ghcr.io/phoban01/flintlock-runner/flr:1.0.0"\n' >"$work/tag.yaml"
expect_fail "a quoted version tag fails" "flr:1.0.0 is not a reference by digest" \
	"$here/check-manifests.sh" "$work/tag.yaml"
printf 'args: [--image=ghcr.io/phoban01/flintlock-runner/host]\n' >"$work/bare.yaml"
expect_fail "a bare repository name fails" "flintlock-runner/host is not a reference by digest" \
	"$here/check-manifests.sh" "$work/bare.yaml"
printf 'image: ghcr.io/phoban01/flintlock-runner/flr:1.0.0@%s\n' "${flr#*@}" >"$work/tagdigest.yaml"
expect_fail "a tag and a digest together fail" "is not a reference by digest" \
	"$here/check-manifests.sh" "$work/tagdigest.yaml"
printf 'kind: ConfigMap\n' >"$work/none.yaml"
expect_fail "a fleet without the flr image fails" "does not reference" \
	"$here/check-manifests.sh" --flr "$flr" --require-flr "$work/none.yaml"
expect_fail "an empty asset fails" "missing or empty" \
	"$here/check-manifests.sh" "$work/nothing.yaml"

if [ "$failures" -ne 0 ]; then
	echo "test-render: $failures failure(s)"
	exit 1
fi
echo "test-render: all passed"
