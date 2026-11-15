#!/usr/bin/env bash
# Check the Guest Image's baker image, in the check stage of
# guest/baker/Containerfile. It runs inside the built image, with no
# network, and reports every failure before it exits non-zero.
set -uo pipefail

failures=0
ok() { printf 'ok    %s\n' "$*"; }
fail() {
	printf 'FAIL  %s\n' "$*"
	failures=$((failures + 1))
}

# shellcheck disable=SC1091
. /usr/share/flr-guest/versions.env

#= docs/requirements/13-guest-image.md#guest-kind-bake
#= type=test
#/ The Guest Image SHALL include a baker image, for `linux/amd64`
#/ and `linux/arm64`, that contains `flr-kind`, `flr-bake`,
#/ `docker-store.sh`, skopeo and curl, and kustomize and helm at the
#/ versions pinned in its sources.
for cmd in flr-kind flr-bake skopeo curl kustomize helm; do
	if command -v "$cmd" >/dev/null; then ok "$cmd is on PATH"; else fail "$cmd is not on PATH"; fi
done
if [ -f /usr/local/libexec/flr/docker-store.sh ]; then
	ok "docker-store.sh is at /usr/local/libexec/flr/docker-store.sh"
else
	fail "docker-store.sh is missing"
fi
got=$(helm version --template '{{.Version}}' 2>&1)
if [ "$got" = "v$HELM_VERSION" ]; then ok "helm is $got"; else fail "helm is \"$got\", want v$HELM_VERSION"; fi
got=$(kustomize version 2>&1)
if [ "$got" = "v$KUSTOMIZE_VERSION" ]; then ok "kustomize is $got"; else fail "kustomize is \"$got\", want v$KUSTOMIZE_VERSION"; fi
if skopeo --version >/dev/null 2>&1; then ok "skopeo runs"; else fail "skopeo does not run"; fi

# A bake with no network: a manifest by path and a kustomize directory
# whose output starts no container, through flr-bake as a project runs it.
work=$(mktemp -d)
mkdir -p "$work/src/.flr" "$work/src/deploy/deps"
cat >"$work/src/.flr/kind-cluster.yaml" <<'SPEC'
apiVersion: flintlock-runner.dev/v1alpha1
kind: KindCluster
name: check
manifests:
  - path: ../deploy/ns.yaml
    wait:
      - namespace/check
  - kustomize: ../deploy/deps
SPEC
printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: check\n' >"$work/src/deploy/ns.yaml"
printf 'resources: [cm.yaml]\nnamespace: check\n' >"$work/src/deploy/deps/kustomization.yaml"
printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\ndata:\n  a: "1"\n' >"$work/src/deploy/deps/cm.yaml"
if out=$(flr-bake "$work/src/.flr/kind-cluster.yaml" "$work/out" 2>&1); then
	ok "flr-bake baked a spec with no network: ${out##*$'\n'}"
else
	fail "flr-bake failed: $out"
fi
digest=$(flr-kind digest "$work/src/.flr/kind-cluster.yaml")
if [ "$(cat "$work/out/etc/flr/kind-cluster.digest" 2>/dev/null)" = "$digest" ]; then
	ok "the baked digest is the spec's"
else
	fail "the baked digest is not $digest"
fi
if grep -q 'namespace: check' "$work/out/var/lib/flr/manifests/1.yaml" 2>/dev/null; then
	ok "kustomize rendered the directory to manifests/1.yaml"
else
	fail "manifests/1.yaml is not the rendered directory"
fi
if flr-kind check -root "$work/out" "$work/out/etc/flr/kind-cluster.yaml"; then
	ok "the baked spec loads with OUT as its root"
else
	fail "the baked spec does not load"
fi
rm -rf "$work"

if [ "$failures" -gt 0 ]; then
	echo "$failures check(s) failed" >&2
	exit 1
fi
echo "every check passed"
