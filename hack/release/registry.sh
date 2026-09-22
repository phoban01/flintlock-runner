# shellcheck shell=bash
# Registry helpers for the release scripts, sourced, not run. They use
# skopeo (SKOPEO names it) so that they work the same against ghcr.io and a
# local registry. REGISTRY_TLS_VERIFY=false is for a plain-HTTP registry on
# localhost in the packaging dry run, never for ghcr.io.

skopeo_cmd() {
	# shellcheck disable=SC2086 # SKOPEO may be a command with arguments
	${SKOPEO:-skopeo} "$@"
}

tls_flag() { # tls_flag <option name>
	printf -- '--%s=%s' "$1" "${REGISTRY_TLS_VERIFY:-true}"
}

# manifest_digest REF: the sha256 of the manifest REF resolves to, computed
# from the raw bytes the registry serves, which is what a digest reference
# pins.
manifest_digest() {
	local sum
	# Straight into sha256sum: a command substitution would drop a trailing
	# newline of the manifest and so change its digest.
	sum=$(set -o pipefail; skopeo_cmd inspect "$(tls_flag tls-verify)" --raw "docker://$1" | sha256sum) || return 1
	printf 'sha256:%s\n' "${sum%% *}"
}

# tag_exists REPO:TAG: 0 when the tag exists, 1 when the registry says it
# does not, and 2 (with the registry's error) for anything else, so that a
# network or server error never passes for absence.
tag_exists() {
	local err
	if err=$(skopeo_cmd inspect "$(tls_flag tls-verify)" --raw "docker://$1" 2>&1 >/dev/null); then
		return 0
	fi
	# ghcr.io answers "denied" for a package that does not exist yet; a real
	# lack of permission makes the push fail instead.
	if grep -qiE 'manifest unknown|name unknown|not found|denied' <<<"$err"; then
		return 1
	fi
	echo "$err" >&2
	return 2
}

# list_tags REPO: every tag of REPO, one per line.
list_tags() {
	skopeo_cmd list-tags "$(tls_flag tls-verify)" "docker://$1" | jq -r '.Tags[]'
}

# kubernetes_version: KUBERNETES_VERSION from image/versions.env, the one
# place it is pinned.
kubernetes_version() {
	local versions=${VERSIONS_ENV:-image/versions.env} v
	v=$(sed -n 's/^KUBERNETES_VERSION=//p' "$versions")
	if ! [[ $v =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		echo "no KUBERNETES_VERSION=vX.Y.Z in $versions (got \"$v\")" >&2
		return 1
	fi
	printf '%s\n' "$v"
}

# The one form of a release version: 1.2.3 or 1.2.3-rc.4, without the v of
# the git tag, as goreleaser's .Version has it.
# shellcheck disable=SC2034 # used by the scripts that source this file
release_version_re='[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?'
