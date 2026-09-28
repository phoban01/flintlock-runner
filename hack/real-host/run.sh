#!/usr/bin/env bash
# Runs one Job through the Runner on a real Host (README.md): the Runner on
# the claim backend, battery-operator's MicroVMClaim, and the Guest Image in
# a MicroVM under Firecracker.
#
#   run.sh               images deploy job
#   run.sh <step>...     only those: images deploy job clean
#
# KUBECONFIG names the cluster. battery-operator is deployed there already.
# The Host is reached like battery-operator's real hosts trial reaches it:
#
#   HOST_DRIVER=lima  (default) the Lima VM NODE_NAME (bo-host-1), through
#                     limactl, or through `mac limactl` from a Linux machine
#                     beside the Mac (OrbStack)
#   HOST_DRIVER=ssh   ssh ${HOST_SSH}, whose user has sudo
#
# Nothing is pushed to a public registry. Every step can run again.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
STATE_DIR="${HERE}/.state"

HOST_DRIVER="${HOST_DRIVER:-lima}"
NODE_NAME="${NODE_NAME:-bo-host-1}"
HOST_SSH="${HOST_SSH:-}"
SSH_OPTS="${SSH_OPTS:-}"

# The registry on the Host that flintlockd pulls the Guest Image from, over
# plain HTTP (battery-operator's trial runs one on 127.0.0.1:5000).
REGISTRY="${REGISTRY:-127.0.0.1:5000}"
# The containerd namespace the Guest Image passes through on its way into
# the registry.
CTR_NS="flr-real-host"
NS=flintlock-system
# The fake GitLab's runner token. It opens nothing but the fake.
RUNNER_TOKEN=glrt-real-host
# How long the job step waits for the Job, in seconds.
JOB_WAIT="${JOB_WAIT:-900}"

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
die() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
pass() { printf '\033[32m==> PASS: %s\033[0m\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# The Host.

# mac_run runs a command on the Mac. OrbStack's `mac` rewrites arguments
# that look like absolute paths, so the command goes through sh as one
# quoted string (as in battery-operator's hack/real-hosts/lib.sh).
mac_run() {
	if [[ "$(uname -s)" == Darwin ]]; then
		"$@"
	elif command -v mac >/dev/null 2>&1; then
		mac sh -c "$(printf '%q ' "$@")"
	else
		die "not on macOS and no 'mac' command to reach it; set HOST_DRIVER=ssh for another Host"
	fi
}

# host_run runs a command on the Host; stdin is passed on.
host_run() {
	case "${HOST_DRIVER}" in
	lima) mac_run limactl shell --workdir / "${NODE_NAME}" "$@" ;;
	ssh)
		[[ -n "${HOST_SSH}" ]] || die "HOST_DRIVER=ssh needs HOST_SSH"
		# SSH_OPTS is a list of options.
		# shellcheck disable=SC2086
		ssh -o BatchMode=yes ${SSH_OPTS} "${HOST_SSH}" -- "$(printf '%q ' "$@")"
		;;
	*) die "HOST_DRIVER is lima or ssh, not ${HOST_DRIVER}" ;;
	esac
}

host_root() { host_run sudo "$@"; }

host_arch() {
	case "$(host_run uname -m)" in
	aarch64 | arm64) echo arm64 ;;
	x86_64) echo amd64 ;;
	*) die "the Host is $(host_run uname -m); arm64 and amd64 are supported" ;;
	esac
}

# keep_awake stops the Mac sleeping for two hours: a sleeping Mac freezes
# the Lima VM.
keep_awake() {
	[[ "${HOST_DRIVER}" == lima ]] || return 0
	mac_run caffeinate -i -t 7200 >/dev/null 2>&1 &
	disown
}

# ---------------------------------------------------------------------------
# images: the flr image and the fake GitLab's image into the kubelet's
# containerd on the Host, and the Guest Image into the Host's registry.

# build_binary_image NAME PKG CONTAINERFILE ARCH prints the local tag of an
# image of one binary, tagged by its image id so that a new build rolls the
# pods and an unchanged one does not.
build_binary_image() {
	local name=$1 pkg=$2 file=$3 arch=$4
	local ctx="${STATE_DIR}/build/${name}" version
	version="real-host-$(git -C "${ROOT}" rev-parse --short HEAD)"
	rm -rf "${ctx}"
	mkdir -p "${ctx}/linux/${arch}"
	(cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux GOARCH="${arch}" \
		go build -trimpath -ldflags "-s -w -X main.version=${version}" -o "${ctx}/linux/${arch}/${name}" "${pkg}")
	docker buildx build --quiet --platform "linux/${arch}" --provenance=false -f "${file}" \
		-t "localhost/flintlock-runner/${name}:build" --load "${ctx}" >/dev/null
	local id tag
	id="$(docker image inspect -f '{{.Id}}' "localhost/flintlock-runner/${name}:build")"
	id="${id#sha256:}"
	tag="localhost/flintlock-runner/${name}:real-host-${id:0:12}"
	docker tag "localhost/flintlock-runner/${name}:build" "${tag}"
	echo "${tag}"
}

# import_to_kubelet TAG puts a local image into k3s's containerd.
import_to_kubelet() {
	docker save "$1" | host_root k3s ctr -n k8s.io images import - >/dev/null
}

# push_to_registry LOCAL NAME ARCH puts a local image into the Host's
# registry as ${REGISTRY}/flintlock-runner/NAME:real-host and prints that
# reference by digest.
push_to_registry() {
	local src=$1 name=$2 arch=$3
	local ref="${REGISTRY}/flintlock-runner/${name}:real-host" digest
	docker save "${src}" | host_root ctr -n "${CTR_NS}" images import --platform "linux/${arch}" - >/dev/null
	# docker save keeps the name as it is here: localhost/... has a domain,
	# so containerd adds no docker.io/ to it.
	host_root ctr -n "${CTR_NS}" images tag --force "${src}" "${ref}" >/dev/null
	host_root ctr -n "${CTR_NS}" images push --plain-http "${ref}" >/dev/null
	digest="$(host_root ctr -n "${CTR_NS}" images ls | awk -v r="${ref}" '$1 == r { print $3 }')"
	[[ "${digest}" == sha256:* ]] || die "no digest for ${ref}"
	echo "${REGISTRY}/flintlock-runner/${name}@${digest}"
}

step_images() {
	local arch flr gitlab kernel rootfs
	arch="$(host_arch)"
	command -v docker >/dev/null || die "docker is needed to build the images"
	mkdir -p "${STATE_DIR}"

	log "Building the flr image for linux/${arch}"
	flr="$(build_binary_image flr ./cmd/flr "${ROOT}/hack/release/flr.Containerfile" "${arch}")"
	log "Building the fake GitLab's image for linux/${arch}"
	gitlab="$(build_binary_image fake-gitlab ./hack/real-host/fake-gitlab "${HERE}/fake-gitlab/Containerfile" "${arch}")"
	log "Importing ${flr} and ${gitlab} into the kubelet's containerd on the Host"
	import_to_kubelet "${flr}"
	import_to_kubelet "${gitlab}"

	log "Building the Guest Image for linux/${arch}"
	(cd "${ROOT}" && guest/build.sh --platforms "linux/${arch}" --load)
	log "Pushing the Guest Image to the Host's registry on ${REGISTRY}"
	kernel="$(push_to_registry localhost/flr-guest-kernel:dev guest-kernel "${arch}")"
	rootfs="$(push_to_registry localhost/flr-guest-rootfs:dev guest-rootfs "${arch}")"
	# The import was only the way into the registry; flintlockd pulls into
	# its own namespace.
	host_root sh -c "ctr -n ${CTR_NS} images ls -q | xargs -r ctr -n ${CTR_NS} images rm >/dev/null; ctr -n ${CTR_NS} content prune references >/dev/null 2>&1 || true"

	cat >"${STATE_DIR}/images.env" <<-EOF
		FLR_IMAGE=${flr}
		FAKE_GITLAB_IMAGE=${gitlab}
		KERNEL_IMAGE=${kernel}
		ROOTFS_IMAGE=${rootfs}
	EOF
	cat "${STATE_DIR}/images.env" >&2
}

load_images() {
	[[ -f "${STATE_DIR}/images.env" ]] || die "no ${STATE_DIR}/images.env: run the images step first"
	# shellcheck source=/dev/null
	. "${STATE_DIR}/images.env"
}

# ---------------------------------------------------------------------------
# deploy: the Runner from deploy/runner, pointed at the fake GitLab, and the
# fake GitLab. deploy/README.md's order: battery-operator is there already.

# overlay writes the kustomize overlay of deploy/runner that run.sh applies,
# under .state, and prints its directory.
overlay() {
	local dir="${STATE_DIR}/overlay"
	mkdir -p "${dir}"
	sed -e "s#KERNEL_IMAGE#${KERNEL_IMAGE}#" -e "s#ROOTFS_IMAGE#${ROOTFS_IMAGE}#" \
		"${HERE}/runner-config.yaml" >"${dir}/config.yaml"
	cat >"${dir}/kustomization.yaml" <<-EOF
		# Written by hack/real-host/run.sh; do not edit.
		apiVersion: kustomize.config.k8s.io/v1beta1
		kind: Kustomization
		resources:
		  - ../../../../deploy/runner
		configMapGenerator:
		  - name: flintlock-runner-config
		    namespace: ${NS}
		    behavior: replace
		    files:
		      - config.yaml
		images:
		  - name: ghcr.io/phoban01/flintlock-runner/flr
		    newName: ${FLR_IMAGE%:*}
		    newTag: ${FLR_IMAGE##*:}
		# flr serves no /healthz or /readyz yet (OB-030, OB-031, #94): its
		# listen address has /metrics only, and deploy/runner's probes get a
		# 404, so the kubelet restarts the Runner every two minutes. Until
		# flr serves them, probe the port instead.
		patches:
		  - target:
		      kind: Deployment
		      name: flintlock-runner
		    patch: |-
		      - op: replace
		        path: /spec/template/spec/containers/0/readinessProbe
		        value: {tcpSocket: {port: metrics}, periodSeconds: 10}
		      - op: replace
		        path: /spec/template/spec/containers/0/livenessProbe
		        value: {tcpSocket: {port: metrics}, periodSeconds: 20, failureThreshold: 6}
	EOF
	echo "${dir}"
}

step_deploy() {
	load_images
	log "Checking battery-operator"
	kubectl get crd pools.battery.liquidmetal-x.dev microvmclaims.battery.liquidmetal-x.dev >/dev/null ||
		die "battery-operator's resources are not served: apply its Manifests first (deploy/README.md)"
	kubectl -n battery-operator-system get configmap flintlockd-ca >/dev/null ||
		die "battery-operator-system/flintlockd-ca is missing: is battery-operator running?"

	log "Applying the namespace and the GitLab runner token"
	kubectl apply -f "${ROOT}/deploy/namespace.yaml" >/dev/null
	kubectl -n "${NS}" create secret generic flintlock-runner-gitlab-token \
		--from-literal=token="${RUNNER_TOKEN}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

	log "Deploying the fake GitLab (${FAKE_GITLAB_IMAGE})"
	sed -e "s#FAKE_GITLAB_IMAGE#${FAKE_GITLAB_IMAGE}#" "${HERE}/fake-gitlab.yaml" | kubectl apply -f - >/dev/null
	kubectl -n "${NS}" rollout status deployment/fake-gitlab --timeout=120s

	log "Deploying the Runner (${FLR_IMAGE})"
	local dir
	dir="$(overlay)"
	kubectl kustomize "${dir}" | kubectl apply -f - >/dev/null
	kubectl -n "${NS}" rollout status deployment/flintlock-runner --timeout=180s

	log "Waiting for the Runner's Pool lima-arm64 to be Ready"
	local ready=""
	for _ in $(seq 1 90); do
		ready="$(kubectl -n "${NS}" get pool lima-arm64 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
		[[ "${ready}" == True ]] && break
		sleep 5
	done
	kubectl -n "${NS}" get pools
	[[ "${ready}" == True ]] || {
		kubectl -n "${NS}" get pool lima-arm64 -o yaml >&2 || true
		die "the Pool lima-arm64 is not Ready"
	}
	pass "the Runner is up, and its Pool lima-arm64 is Ready"
}

# ---------------------------------------------------------------------------
# job: queue job.json on the fake GitLab, watch its claim while it runs, and
# check the Job's result and that its claim is gone.

fake() {
	kubectl get --raw "/api/v1/namespaces/${NS}/services/fake-gitlab:http/proxy/fake/$1"
}

step_job() {
	mkdir -p "${STATE_DIR}"
	local out id status="" claims seen="${STATE_DIR}/claims-while-running.txt"
	out="${STATE_DIR}/run-$(date -u +%Y%m%dT%H%M%SZ).log"
	: >"${seen}"

	log "Queueing hack/real-host/job.json on the fake GitLab"
	id="$(kubectl create --raw "/api/v1/namespaces/${NS}/services/fake-gitlab:http/proxy/fake/jobs" \
		-f "${HERE}/job.json" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')"
	[[ -n "${id}" ]] || die "the fake GitLab queued no Job"
	log "Job ${id} is queued; watching its claim until it ends (at most ${JOB_WAIT}s)"

	for _ in $(seq 1 $((JOB_WAIT / 2))); do
		status="$(fake "jobs/${id}/status" 2>/dev/null || true)"
		claims="$(kubectl get microvmclaims -A 2>/dev/null || true)"
		if [[ "${claims}" == *Bound* ]] && ! grep -q Bound "${seen}"; then
			printf '%s\n' "${claims}" >"${seen}"
			log "The Job's claim, while the Job runs:"
			printf '%s\n' "${claims}" >&2
		fi
		case "${status}" in
		success | failed | canceled) break ;;
		esac
		sleep 2
	done

	log "Waiting for the Job's claim to go"
	local after=""
	for _ in $(seq 1 30); do
		after="$(kubectl -n "${NS}" get microvmclaims -o name 2>/dev/null || true)"
		[[ -z "${after}" ]] && break
		sleep 2
	done

	{
		echo "job ${id}: ${status:-unknown}"
		echo "--- trace ---"
		fake "jobs/${id}/trace" || true
		echo "--- kubectl get microvmclaims -A, while the Job ran ---"
		cat "${seen}"
		echo "--- kubectl get microvmclaims -A, after the Job ---"
		kubectl get microvmclaims -A 2>&1
	} | tee "${out}"
	log "The run's log is ${out}"

	local trace
	trace="$(fake "jobs/${id}/trace" || true)"
	[[ "${status}" == success ]] || die "job ${id} is ${status:-still running after ${JOB_WAIT}s}"
	grep -q Bound "${seen}" || die "no Bound claim was seen while job ${id} ran"
	[[ -z "${after}" ]] || die "the Job's claim is still there: ${after}"
	[[ "${trace}" == *Linux* ]] || die "the trace has no uname line"
	[[ "${trace}" == *"cloned liquidmetal-dev/flintlock at "* ]] || die "the trace shows no clone"
	pass "job ${id} succeeded in a MicroVM: uname, os-release and a clone from GitHub; its claim was Bound while it ran, and is gone"
}

# ---------------------------------------------------------------------------
# clean: the Runner, its Pool and the fake GitLab go; battery-operator
# stays. The Runner never deletes its Pools, so this does.

step_clean() {
	log "Deleting the Runner"
	if [[ -f "${STATE_DIR}/images.env" ]]; then
		load_images
		kubectl kustomize "$(overlay)" | kubectl delete --ignore-not-found --wait=true -f - >/dev/null
	else
		kubectl -n "${NS}" delete deployment flintlock-runner --ignore-not-found --wait=true >/dev/null
	fi
	log "Deleting the fake GitLab"
	kubectl -n "${NS}" delete deployment,service fake-gitlab --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if kubectl get namespace "${NS}" >/dev/null 2>&1; then
		log "Deleting the claims and Pools in ${NS}"
		kubectl -n "${NS}" delete microvmclaims --all --wait=true --timeout=180s >/dev/null
		kubectl -n "${NS}" delete pools --all --wait=true --timeout=300s >/dev/null
		log "Deleting the namespace ${NS}"
		kubectl delete namespace "${NS}" --wait=true --timeout=180s >/dev/null
	fi
	kubectl get pools,microvmclaims -A
	pass "no Runner, no Pools and no claims are left; battery-operator stays"
}

main() {
	[[ -n "${KUBECONFIG:-}" ]] || die "set KUBECONFIG to the cluster's kubeconfig"
	local steps=("$@") s
	[[ ${#steps[@]} -gt 0 ]] || steps=(images deploy job)
	keep_awake
	for s in "${steps[@]}"; do
		case "${s}" in
		images | deploy | job | clean) "step_${s}" ;;
		*) die "unknown step ${s}: images deploy job clean" ;;
		esac
	done
}

main "$@"
