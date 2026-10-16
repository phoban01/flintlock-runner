#!/usr/bin/env bash
# Configure the Guest Image's kind image, in the kind stage of
# guest/rootfs-kind/Containerfile, on the Docker image for the image's
# platform.
#
# TARGETARCH, KIND_VERSION, KUBECTL_VERSION, KIND_NODE_IMAGE and the
# KIND_SHA256_* and KUBECTL_SHA256_* sums come from the Containerfile's
# build arguments.
set -euo pipefail
: "${TARGETARCH:?}" "${KIND_VERSION:?}" "${KUBECTL_VERSION:?}" "${KIND_NODE_IMAGE:?}" \
	"${KIND_SHA256_AMD64:?}" "${KIND_SHA256_ARM64:?}" \
	"${KUBECTL_SHA256_AMD64:?}" "${KUBECTL_SHA256_ARM64:?}"

case $TARGETARCH in
arm64) kind_sum=$KIND_SHA256_ARM64 kubectl_sum=$KUBECTL_SHA256_ARM64 ;;
amd64) kind_sum=$KIND_SHA256_AMD64 kubectl_sum=$KUBECTL_SHA256_AMD64 ;;
*) echo "configure: no kind image for $TARGETARCH" >&2; exit 1 ;;
esac

#= docs/requirements/13-guest-image.md#guest-build
#/ The Guest Image build SHALL verify the checksum of every file
#/ it downloads against a checksum recorded in the Guest Image's sources and
#/ SHALL fail when one does not match.
#
#= docs/requirements/13-guest-image.md#guest-kind
#/ The Guest Image's kind image SHALL contain kind and kubectl, each
#/ at the version pinned in its sources.
curl -fsSL -o /usr/local/bin/kind \
	"https://github.com/kubernetes-sigs/kind/releases/download/v${KIND_VERSION}/kind-linux-${TARGETARCH}"
curl -fsSL -o /usr/local/bin/kubectl \
	"https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl"
printf '%s  %s\n' "$kind_sum" /usr/local/bin/kind "$kubectl_sum" /usr/local/bin/kubectl | sha256sum -c -
chmod 0755 /usr/local/bin/kind /usr/local/bin/kubectl

#= docs/requirements/13-guest-image.md#guest-kind-fast
#/ The Guest Image's kind image SHALL contain the node image that
#/ `KIND_NODE_IMAGE` names, for the image's architecture, in Docker
#/ Engine's image store under that reference, so that a kind cluster
#/ starts without a pull or a load of the node image.
#
# The node-image stage wrote it into /var/lib/docker. The image ID goes in
# the versions file, for the check stage and for a Job that looks.
node_id=$(sed -n 's/.*"[^"]*@sha256:[0-9a-f]\{64\}":"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' /var/lib/docker/image/overlay2/repositories.json)
if ! [[ $node_id =~ ^sha256:[0-9a-f]{64}$ ]]; then
	echo "configure: Docker Engine's image store has no node image" >&2
	exit 1
fi
chmod 0755 /usr/local/bin/flr-kind-cluster

#= docs/requirements/13-guest-image.md#guest-kind
#/ The Guest Image's kind image SHALL contain
#/ `/etc/default/kind-bootstrap`, which exports `KIND_CLUSTER_NAME`,
#/ `KIND_NODE_IMAGE`, `KUBECONFIG_PATH` and `KUBECONFIG`, with
#/ `KIND_NODE_IMAGE` naming by digest a kindest/node image that the pinned
#/ kind release lists.
cat >/etc/default/kind-bootstrap <<EOF
# What a Job needs to make a kind cluster in the Guest Image's kind image.
# Written by its build (guest/rootfs-kind/configure.sh). Source it, then
# make the cluster with flr-kind-cluster, which takes the running cluster
# when the Pool's create hook made one already:
#
#   . /etc/default/kind-bootstrap
#   flr-kind-cluster
#
# KIND_NODE_IMAGE is the node image kind ${KIND_VERSION} lists for Kubernetes
# ${KIND_NODE_IMAGE#*:v}, by digest. Docker Engine has it already, so kind
# pulls nothing. KIND_CONFIG is written at boot, with the Host's registry
# mirror when the guest has one.
export KIND_CLUSTER_NAME=flr
export KIND_NODE_IMAGE=${KIND_NODE_IMAGE}
export KIND_CONFIG=/etc/kind/kind-config.yaml
export KUBECONFIG_PATH="\${HOME:-/root}/.kube/kind-\${KIND_CLUSTER_NAME}.yaml"
export KUBECONFIG="\${KUBECONFIG_PATH}"
EOF
chmod 0644 /etc/default/kind-bootstrap

# The kind configuration with no mirror, until the boot unit writes it.
chmod 0755 /usr/libexec/flr-guest/registry-mirror.d/20-kind
FLR_REGISTRY_MIRROR='' /usr/libexec/flr-guest/registry-mirror.d/20-kind

cat >>/usr/share/flr-guest/versions.env <<EOF
KIND_VERSION=$KIND_VERSION
KUBECTL_VERSION=$KUBECTL_VERSION
KIND_NODE_IMAGE=$KIND_NODE_IMAGE
KIND_NODE_IMAGE_ID=$node_id
EOF
