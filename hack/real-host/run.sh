#!/usr/bin/env bash
# Runs Jobs through the Runner on a real Host (README.md): the Runner on the
# claim backend, battery-operator's MicroVMClaim, the Host Agent with its
# Host Services, and the Guest Image in a MicroVM under Firecracker. The
# Host boots battery-operator's Host Image and joins a kubeadm cluster, as
# battery-operator's hack/kubeadm-host makes it.
#
#   run.sh               images deploy job services
#   run.sh <step>...     only those: images deploy job services
#                        cache-demo build-demo kind
#                        fault-flintlockd fault-flintlockd-hang
#                        fault-exec-agent-hang fault-not-ready clean
#
# KUBEADM_HOST is battery-operator's hack/kubeadm-host directory, after its
# up.sh. The kubeconfig and the ssh configuration come from its .state.
# Without it, set KUBECONFIG, HOST_SSH and SSH_OPTS yourself.
#
# Nothing is pushed to a public registry. Every step can run again.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"
STATE_DIR="${HERE}/.state"

KUBEADM_HOST="${KUBEADM_HOST:-}"
# The Host's Node. The Host is reached over ssh as HOST_SSH, whose user has
# sudo; by default HOST_USER at the Node's InternalIP.
NODE_NAME="${NODE_NAME:-bo-kubeadm-host-1}"
HOST_USER="${HOST_USER:-battery}"
HOST_SSH="${HOST_SSH:-}"
SSH_OPTS="${SSH_OPTS:-}"
# The Node that runs the Runner and the fake GitLab, off the Host: the
# kubeadm control plane, a Lima VM on the Mac (CP_DRIVER=lima, CP_NAME), or
# a machine over ssh (CP_DRIVER=ssh, CP_SSH).
CP_DRIVER="${CP_DRIVER:-lima}"
CP_NAME="${CP_NAME:-bo-kubeadm-cp}"
CP_SSH="${CP_SSH:-}"
if [[ -n "${KUBEADM_HOST}" ]]; then
	[[ -f "${KUBEADM_HOST}/.state/kubeconfig" ]] || {
		printf 'FAIL: no %s/.state/kubeconfig: run its up.sh first\n' "${KUBEADM_HOST}" >&2
		exit 1
	}
	export KUBECONFIG="${KUBECONFIG:-${KUBEADM_HOST}/.state/kubeconfig}"
	SSH_OPTS="${SSH_OPTS:--F ${KUBEADM_HOST}/.state/ssh_config}"
fi

# Where the Guest Image comes from. GUEST=release takes the guest-kernel,
# guest-rootfs, guest-rootfs-docker and guest-rootfs-kind digests from
# RELEASE's images.txt; flintlockd pulls them from ghcr.io. A release with
# no Docker or kind image leaves out their Profiles. GUEST=build builds all
# four from guest/ and pushes them to the Host's registry on REGISTRY, which
# flintlockd pulls from over plain HTTP.
GUEST="${GUEST:-release}"
RELEASE="${RELEASE:-v1.0.0-rc.9}"
RELEASE_REPO="${RELEASE_REPO:-phoban01/flintlock-runner}"
REGISTRY="${REGISTRY:-127.0.0.1:5000}"
# The containerd namespace a built Guest Image passes through on its way
# into the registry.
CTR_NS="flr-real-host"
NS=flintlock-system
# The Pools of the Runner's Profiles (runner-config.yaml): the default
# Profile's, and the Docker and kind Profiles'.
POOL=real-host-arm64
DOCKER_POOL=real-host-docker
KIND_POOL=real-host-kind
# The Profiles of runner-config.yaml that deploy keeps: arm64 (the default
# Profile, for job and services), docker (for cache-demo and build-demo)
# and kind. On a Mac, keep one at a time (runner-config.yaml says why);
# deploy deletes the Pools of the others.
PROFILES="${PROFILES:-arm64}"
# The fake GitLab's runner token. It opens nothing but the fake.
RUNNER_TOKEN=glrt-real-host
# How long a job step waits for its Job, in seconds.
JOB_WAIT="${JOB_WAIT:-900}"
# How long a fault step waits for the Job to fail after the fault, in
# seconds. With the Runner's defaults the bounds are 60 s (KF-202) and
# 20 s after the claim's HostReady turns false (KF-205).
FAULT_WAIT="${FAULT_WAIT:-180}"
# How long a job step waits for a MicroVM to claim, in seconds. A Pool whose
# MicroVM missed battery's wait for its guest agent refills only at its next
# reseed, minutes later.
POOL_WAIT="${POOL_WAIT:-180}"
# The not ready reason file the fault-not-ready step writes on the Host.
NOT_READY_FILE=/run/battery/not-ready.d/flr-real-host

log() { printf '\033[1m==> %s\033[0m\n' "$*" >&2; }
die() { printf '\033[31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
pass() { printf '\033[32m==> PASS: %s\033[0m\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# The Host and the control plane.

# mac_run runs a command on the Mac. OrbStack's `mac` rewrites arguments
# that look like absolute paths, so the command goes through sh as one
# quoted string (as in battery-operator's hack/real-hosts/lib.sh).
mac_run() {
	if [[ "$(uname -s)" == Darwin ]]; then
		"$@"
	elif command -v mac >/dev/null 2>&1; then
		mac sh -c "$(printf '%q ' "$@")"
	else
		die "not on macOS and no 'mac' command to reach it"
	fi
}

host_ssh() {
	if [[ -z "${HOST_SSH}" ]]; then
		local ip
		ip="$(kubectl get node "${NODE_NAME}" -o jsonpath='{.status.addresses[?(@.type=="InternalIP")].address}')"
		[[ -n "${ip}" ]] || die "the Node ${NODE_NAME} has no InternalIP; set HOST_SSH"
		HOST_SSH="${HOST_USER}@${ip}"
	fi
	echo "${HOST_SSH}"
}

# host_run runs a command on the Host; stdin is passed on.
host_run() {
	local dest
	dest="$(host_ssh)"
	# SSH_OPTS is a list of options.
	# shellcheck disable=SC2086
	ssh -o BatchMode=yes ${SSH_OPTS} "${dest}" -- "$(printf '%q ' "$@")"
}

host_root() { host_run sudo "$@"; }

# cp_root runs a command as root on the Node that runs the Runner; stdin is
# passed on.
cp_root() {
	case "${CP_DRIVER}" in
	lima) mac_run limactl shell --workdir / "${CP_NAME}" sudo "$@" ;;
	ssh)
		[[ -n "${CP_SSH}" ]] || die "CP_DRIVER=ssh needs CP_SSH"
		# shellcheck disable=SC2086
		ssh -o BatchMode=yes ${SSH_OPTS} "${CP_SSH}" -- "$(printf '%q ' sudo "$@")"
		;;
	*) die "CP_DRIVER is lima or ssh, not ${CP_DRIVER}" ;;
	esac
}

host_arch() {
	case "$(host_run uname -m)" in
	aarch64 | arm64) echo arm64 ;;
	x86_64) echo amd64 ;;
	*) die "the Host is $(host_run uname -m); arm64 and amd64 are supported" ;;
	esac
}

# keep_awake stops the Mac sleeping for two hours: a sleeping Mac freezes
# its VMs. caffeinate, or `mac` on Linux, is the background process itself.
keep_awake() {
	[[ "${CP_DRIVER}" == lima ]] || return 0
	local cmd=(caffeinate -i -t 7200)
	[[ "$(uname -s)" == Darwin ]] || cmd=(mac sh -c "$(printf '%q ' "${cmd[@]}")")
	"${cmd[@]}" >/dev/null 2>&1 &
	disown
}

# ---------------------------------------------------------------------------
# images: the flr image and the fake GitLab's image into the kubelet's
# containerd on the Host and on the control plane, and the Guest Image
# references.

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

# import_to_kubelets TAG puts a local image into the kubelet's containerd on
# the Host, for the Host Agent, and on the control plane, for the Runner
# and the fake GitLab. Both have containerd's ctr.
import_to_kubelets() {
	local tar="${STATE_DIR}/build/image.tar"
	docker save "$1" -o "${tar}"
	host_root ctr -n k8s.io images import - <"${tar}" >/dev/null
	cp_root ctr -n k8s.io images import - <"${tar}" >/dev/null
	rm -f "${tar}"
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

# release_guest prints KERNEL_IMAGE= and ROOTFS_IMAGE= lines with the
# digests of RELEASE's images.txt, which it checks against the release's
# checksums.txt first.
release_guest() {
	local dir="${STATE_DIR}/release/${RELEASE}" url kernel rootfs
	url="https://github.com/${RELEASE_REPO}/releases/download/${RELEASE}"
	mkdir -p "${dir}"
	curl -fsSL -o "${dir}/images.txt" "${url}/images.txt"
	curl -fsSL -o "${dir}/checksums.txt" "${url}/checksums.txt"
	(cd "${dir}" && grep ' images.txt$' checksums.txt | sha256sum -c --quiet -) ||
		die "${RELEASE}'s images.txt does not match its checksums.txt"
	kernel="$(awk '$1 ~ /\/guest-kernel:/ { print $2 }' "${dir}/images.txt")"
	rootfs="$(awk '$1 ~ /\/guest-rootfs:/ { print $2 }' "${dir}/images.txt")"
	[[ "${kernel}" == *@sha256:* && "${rootfs}" == *@sha256:* ]] ||
		die "${RELEASE}'s images.txt names no guest-kernel or guest-rootfs digest"
	printf 'KERNEL_IMAGE=%s\nROOTFS_IMAGE=%s\n' "${kernel}" "${rootfs}"
	# Releases before the Docker and kind images name neither.
	printf 'DOCKER_ROOTFS_IMAGE=%s\n' "$(awk '$1 ~ /\/guest-rootfs-docker:/ { print $2 }' "${dir}/images.txt")"
	printf 'KIND_ROOTFS_IMAGE=%s\n' "$(awk '$1 ~ /\/guest-rootfs-kind:/ { print $2 }' "${dir}/images.txt")"
}

step_images() {
	local arch flr gitlab guest
	arch="$(host_arch)"
	command -v docker >/dev/null || die "docker is needed to build the images"
	mkdir -p "${STATE_DIR}/build"

	log "Building the flr image for linux/${arch}"
	flr="$(build_binary_image flr ./cmd/flr "${ROOT}/hack/release/flr.Containerfile" "${arch}")"
	log "Building the fake GitLab's image for linux/${arch}"
	gitlab="$(build_binary_image fake-gitlab ./hack/real-host/fake-gitlab "${HERE}/fake-gitlab/Containerfile" "${arch}")"
	log "Importing ${flr} and ${gitlab} into the kubelet's containerd on ${NODE_NAME} and the control plane"
	import_to_kubelets "${flr}"
	import_to_kubelets "${gitlab}"

	case "${GUEST}" in
	release)
		log "Taking the Guest Image from ${RELEASE}'s images.txt"
		guest="$(release_guest)"
		;;
	build)
		log "Building the Guest Image for linux/${arch}"
		(cd "${ROOT}" && guest/build.sh --platforms "linux/${arch}" --load)
		log "Pushing the Guest Image to the Host's registry on ${REGISTRY}"
		guest="KERNEL_IMAGE=$(push_to_registry localhost/flr-guest-kernel:dev guest-kernel "${arch}")"
		guest+=$'\n'"ROOTFS_IMAGE=$(push_to_registry localhost/flr-guest-rootfs:dev guest-rootfs "${arch}")"
		guest+=$'\n'"DOCKER_ROOTFS_IMAGE=$(push_to_registry localhost/flr-guest-rootfs-docker:dev guest-rootfs-docker "${arch}")"
		guest+=$'\n'"KIND_ROOTFS_IMAGE=$(push_to_registry localhost/flr-guest-rootfs-kind:dev guest-rootfs-kind "${arch}")"
		# The import was only the way into the registry; flintlockd pulls
		# into its own namespace.
		host_root sh -c "ctr -n ${CTR_NS} images ls -q | xargs -r ctr -n ${CTR_NS} images rm >/dev/null; ctr -n ${CTR_NS} content prune references >/dev/null 2>&1 || true"
		;;
	*) die "GUEST is release or build, not ${GUEST}" ;;
	esac

	{
		echo "FLR_IMAGE=${flr}"
		echo "FAKE_GITLAB_IMAGE=${gitlab}"
		echo "${guest}"
	} >"${STATE_DIR}/images.env"
	cat "${STATE_DIR}/images.env" >&2
}

load_images() {
	[[ -f "${STATE_DIR}/images.env" ]] || die "no ${STATE_DIR}/images.env: run the images step first"
	# shellcheck source=/dev/null
	. "${STATE_DIR}/images.env"
}

# ---------------------------------------------------------------------------
# deploy: the Fleet Manifests, deploy/, with the Runner pointed at the fake
# GitLab, and the fake GitLab. deploy/README.md's order: battery-operator is
# there already.

# profile_kept NAME succeeds when deploy keeps the Profile NAME of
# runner-config.yaml: PROFILES names it, and images.env names its root
# filesystem image.
profile_kept() {
	[[ " ${PROFILES} " == *" $1 "* ]] || return 1
	case $1 in
	docker) [[ -n "${DOCKER_ROOTFS_IMAGE:-}" ]] ;;
	kind) [[ -n "${KIND_ROOTFS_IMAGE:-}" ]] ;;
	*) return 0 ;;
	esac
}

# overlay writes the kustomize overlay of deploy/ that run.sh applies,
# under .state, and prints its directory.
overlay() {
	local dir="${STATE_DIR}/overlay"
	mkdir -p "${dir}"
	local drop=() p
	for p in arm64 docker kind; do
		profile_kept "${p}" || drop+=(-e "/# BEGIN profile ${p}/,/# END profile ${p}/d")
	done
	sed "${drop[@]}" -e "s#: KERNEL_IMAGE\$#: ${KERNEL_IMAGE}#" -e "s#: ROOTFS_IMAGE\$#: ${ROOTFS_IMAGE}#" \
		-e "s#: DOCKER_ROOTFS_IMAGE\$#: ${DOCKER_ROOTFS_IMAGE:-}#" -e "s#: KIND_ROOTFS_IMAGE\$#: ${KIND_ROOTFS_IMAGE:-}#" \
		"${HERE}/runner-config.yaml" >"${dir}/config.yaml"
	cat >"${dir}/kustomization.yaml" <<-EOF
		# Written by hack/real-host/run.sh; do not edit.
		apiVersion: kustomize.config.k8s.io/v1beta1
		kind: Kustomization
		resources:
		  - ../../../../deploy
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
	EOF
	echo "${dir}"
}

# check_host_pool fails unless the Host's host.env has the gateway service
# ports and user ids of deploy/host-agent/config/host-pool.conf. The Host
# Agent's render container makes the same check and keeps the Host
# Services from starting (KF-199); this says so before anything is
# deployed.
check_host_pool() {
	local env want_ports want_uids
	env="$(host_run cat /run/battery/host.env)"
	want_ports="$(sed -n 's/^GATEWAY_SERVICE_PORTS=//p' "${ROOT}/deploy/host-agent/config/host-pool.conf")"
	want_uids="$(sed -n 's/^GATEWAY_SERVICE_UIDS=//p' "${ROOT}/deploy/host-agent/config/host-pool.conf")"
	if ! grep -qx "BATTERY_GATEWAY_SERVICE_PORTS=${want_ports}" <<<"${env}" ||
		! grep -qx "BATTERY_GATEWAY_SERVICE_UIDS=${want_uids}" <<<"${env}"; then
		grep '^BATTERY_GATEWAY' <<<"${env}" >&2 || true
		die "the Host's host.conf lacks GATEWAY_SERVICE_PORTS=${want_ports} or GATEWAY_SERVICE_UIDS=${want_uids}: see README.md, \"The Host pool\""
	fi
}

host_ready() {
	kubectl get node "${NODE_NAME}" -o jsonpath='{.metadata.annotations.battery\.liquidmetal-x\.dev/exec-agent-ready}'
}

# pool_ready [POOL] prints the Ready status of POOL, by default the default
# Profile's.
pool_ready() {
	kubectl -n "${NS}" get pool "${1:-${POOL}}" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true
}

# wait_until SECONDS CMD... runs CMD every 5 s until it succeeds, for at
# most SECONDS.
wait_until() {
	local secs=$1
	shift
	for _ in $(seq 1 $((secs / 5))); do
		"$@" && return 0
		sleep 5
	done
	"$@"
}

is_pool_ready() { [[ "$(pool_ready "$@")" == True ]]; }
is_host_ready() { [[ "$(host_ready)" == true ]]; }

# has_available [POOL] succeeds while POOL, by default the default
# Profile's, has a MicroVM ready to claim and none provisioning.
has_available() {
	local s
	s="$(kubectl -n "${NS}" get pool "${1:-${POOL}}" -o jsonpath='{.status.available}/{.status.provisioning}' 2>/dev/null || true)"
	[[ "${s%/*}" =~ ^[1-9] && "${s#*/}" =~ ^0?$ ]]
}

step_deploy() {
	load_images
	log "Checking battery-operator and the Host ${NODE_NAME}"
	kubectl get crd pools.battery.liquidmetal-x.dev microvmclaims.battery.liquidmetal-x.dev >/dev/null ||
		die "battery-operator's resources are not served: apply its Manifests first (deploy/README.md)"
	kubectl -n battery-operator-system get configmap flintlockd-ca >/dev/null ||
		die "battery-operator-system/flintlockd-ca is missing: is battery-operator running?"
	is_host_ready || die "battery-operator's Exec Agent does not report ${NODE_NAME} ready"
	check_host_pool

	log "Applying the namespace and the GitLab runner token"
	kubectl apply -f "${ROOT}/deploy/namespace.yaml" >/dev/null
	kubectl -n "${NS}" create secret generic flintlock-runner-gitlab-token \
		--from-literal=token="${RUNNER_TOKEN}" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

	log "Deploying the fake GitLab (${FAKE_GITLAB_IMAGE})"
	sed -e "s#FAKE_GITLAB_IMAGE#${FAKE_GITLAB_IMAGE}#" "${HERE}/fake-gitlab.yaml" | kubectl apply -f - >/dev/null
	kubectl -n "${NS}" rollout status deployment/fake-gitlab --timeout=120s

	log "Deploying the Fleet Manifests: the Host Agent and the Runner (${FLR_IMAGE})"
	local dir
	dir="$(overlay)"
	kubectl kustomize "${dir}" | kubectl apply -f - >/dev/null
	# The Host Agent pulls its Host Services' images on its first start.
	kubectl -n "${NS}" rollout status daemonset/flintlock-host-agent --timeout=600s || {
		kubectl -n "${NS}" get pods -o wide >&2
		kubectl -n "${NS}" logs daemonset/flintlock-host-agent -c render >&2 || true
		die "the Host Agent did not start"
	}
	kubectl -n "${NS}" rollout status deployment/flintlock-runner --timeout=300s

	# The Runner never deletes a Pool, so the Pools of the Profiles that
	# PROFILES leaves out go here. Their MicroVMs would take the Host's
	# memory and slow every boot.
	local p kept=()
	for p in arm64 docker kind; do
		if profile_kept "${p}"; then
			kept+=("real-host-${p}")
		else
			kubectl -n "${NS}" delete pool "real-host-${p}" --ignore-not-found --wait=true --timeout=300s >/dev/null
		fi
	done
	[[ ${#kept[@]} -gt 0 ]] || die "PROFILES=${PROFILES} keeps no Profile of runner-config.yaml"
	# The Pool's Ready condition can say AtSize while nothing is available
	# (battery-operator#223), so this waits for a MicroVM itself.
	for p in "${kept[@]}"; do
		log "Waiting for the Pool ${p} to have a MicroVM available"
		wait_until 900 has_available "${p}" || {
			kubectl -n "${NS}" get pool "${p}" -o yaml >&2 || true
			die "the Pool ${p} has no MicroVM available"
		}
	done
	kubectl -n "${NS}" get pools,pods -o wide
	log "The Host Services on ${NODE_NAME}'s Node"
	kubectl get node "${NODE_NAME}" -o json |
		grep -o '"host-service.gitlab-runner.flintlock.dev/[a-z_]*": "[^"]*"' >&2 ||
		die "the Host Agent published no Host Service on ${NODE_NAME}"
	pass "the Host Agent and the Runner are up, and the Pools ${kept[*]} have a MicroVM available"
}

# ---------------------------------------------------------------------------
# Jobs on the fake GitLab.

fake() {
	kubectl get --raw "/api/v1/namespaces/${NS}/services/fake-gitlab:http/proxy/fake/$1"
}

# queue FILE queues a Job and prints its id.
queue() {
	local id
	id="$(kubectl create --raw "/api/v1/namespaces/${NS}/services/fake-gitlab:http/proxy/fake/jobs" \
		-f "$1" | sed -n 's/.*"id":\([0-9]*\).*/\1/p')"
	[[ -n "${id}" ]] || die "the fake GitLab queued no Job"
	echo "${id}"
}

job_status() { fake "jobs/$1/status" 2>/dev/null || true; }

finished() {
	case "$1" in success | failed | canceled) return 0 ;; esac
	return 1
}

# wait_claims_gone waits up to a minute for the Runner to delete its
# claims, and prints those it has not deleted. A deleted claim can stay a
# while: battery-operator keeps its finalizer until battery has released
# the claim's lease, which needs the Host's flintlockd.
wait_claims_gone() {
	local after=""
	for _ in $(seq 1 30); do
		after="$(kubectl -n "${NS}" get microvmclaims -o go-template \
			--template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}' 2>/dev/null || true)"
		[[ -z "${after}" ]] && break
		sleep 2
	done
	echo "${after}"
}

# run_job FILE NAME [POOL] queues FILE once POOL, by default the default
# Profile's, has a MicroVM available, records the claims while the Job runs,
# waits for the Job to end and its claim to go, and writes the run's log.
# It sets JOB_ID, JOB_STATUS, JOB_SECONDS, JOB_TRACE, CLAIMS_SEEN and
# CLAIMS_AFTER.
run_job() {
	local file=$1 name=$2 pool=${3:-${POOL}} claims seen="${STATE_DIR}/claims-while-running.txt" start out
	mkdir -p "${STATE_DIR}"
	: >"${seen}"
	wait_until "${POOL_WAIT}" has_available "${pool}" ||
		die "the Pool ${pool} has no available MicroVM: $(kubectl -n "${NS}" get pool "${pool}" -o jsonpath='{.status}')"
	log "Queueing $(basename "${file}") on the fake GitLab"
	JOB_ID="$(queue "${file}")"
	log "Job ${JOB_ID} is queued; watching its claim until it ends (at most ${JOB_WAIT}s)"
	start=${SECONDS}
	JOB_STATUS=""
	for _ in $(seq 1 $((JOB_WAIT / 2))); do
		JOB_STATUS="$(job_status "${JOB_ID}")"
		claims="$(kubectl get microvmclaims -A 2>/dev/null || true)"
		if [[ "${claims}" == *Bound* ]] && ! grep -q Bound "${seen}"; then
			printf '%s\n' "${claims}" >"${seen}"
			log "The Job's claim, while the Job runs:"
			printf '%s\n' "${claims}" >&2
		fi
		finished "${JOB_STATUS}" && break
		sleep 2
	done
	JOB_SECONDS=$((SECONDS - start))
	log "Job ${JOB_ID} is ${JOB_STATUS:-unknown} after ${JOB_SECONDS}s; waiting for its claim to go"
	CLAIMS_AFTER="$(wait_claims_gone)"
	CLAIMS_SEEN="$(cat "${seen}")"
	JOB_TRACE="$(fake "jobs/${JOB_ID}/trace" || true)"
	out="${STATE_DIR}/run-${name}-$(date -u +%Y%m%dT%H%M%SZ).log"
	{
		echo "job ${JOB_ID}: ${JOB_STATUS:-unknown}, ${JOB_SECONDS}s from queued to the end"
		echo "--- trace ---"
		printf '%s\n' "${JOB_TRACE}"
		echo "--- kubectl get microvmclaims -A, while the Job ran ---"
		printf '%s\n' "${CLAIMS_SEEN}"
		echo "--- kubectl get microvmclaims -A, after the Job ---"
		kubectl get microvmclaims -A 2>&1
	} | tee "${out}"
	log "The run's log is ${out}"
}

# check_job_passed fails unless the Job of run_job succeeded, had a Bound
# claim while it ran, and left no claim.
check_job_passed() {
	[[ "${JOB_STATUS}" == success ]] || die "job ${JOB_ID} is ${JOB_STATUS:-still running after ${JOB_WAIT}s}"
	grep -q Bound <<<"${CLAIMS_SEEN}" || die "no Bound claim was seen while job ${JOB_ID} ran"
	[[ -z "${CLAIMS_AFTER}" ]] || die "the Job's claim is still there: ${CLAIMS_AFTER}"
}

step_job() {
	run_job "${HERE}/job.json" job
	check_job_passed
	[[ "${JOB_TRACE}" == *Linux* ]] || die "the trace has no uname line"
	[[ "${JOB_TRACE}" == *"cloned liquidmetal-dev/flintlock at "* ]] || die "the trace shows no clone"
	pass "job ${JOB_ID} succeeded in a MicroVM: uname, os-release and a clone from GitHub; its claim was Bound while it ran, and is gone"
}

# ---------------------------------------------------------------------------
# services: a Job that uses the Host Services, then the Host Agent on the
# Host: its SELinux domains, the AVC denials since boot, and its cache in
# the emptyDir.

host_agent_pod() {
	kubectl -n "${NS}" get pods -l app.kubernetes.io/name=flintlock-host-agent \
		--field-selector "spec.nodeName=${NODE_NAME}" -o jsonpath='{.items[0].metadata.uid}'
}

# avc_denials prints the Host's AVC denials since boot, one line each:
# count, command, permission, source and target contexts, class. The Host
# Image has no ausearch, so they come from the journal.
avc_denials() {
	host_root journalctl -b --no-pager -o cat _TRANSPORT=audit |
		grep 'avc:  denied' |
		sed -E 's/.*denied  \{ ([^}]*[^ ]) \}.*comm="([^"]*)"(.*path="([^"]*)")?.*scontext=([^ ]*) tcontext=([^ ]*) tclass=([^ ]*).*/\2 {\1} \4 \5 -> \6 \7/' |
		sort | uniq -c || true
}

step_services() {
	run_job "${HERE}/job-host-services.json" services
	check_job_passed
	[[ "${JOB_TRACE}" == *'"Version":"v1.6.0"'* ]] || die "the trace shows no module from the Go module proxy"
	[[ "${JOB_TRACE}" == *"registry mirror: 200"* ]] || die "the trace shows no manifest from the registry mirror"

	local uid cache report level avc agent
	report="${STATE_DIR}/host-agent-$(date -u +%Y%m%dT%H%M%SZ).log"
	uid="$(host_agent_pod)"
	[[ -n "${uid}" ]] || die "no Host Agent pod on ${NODE_NAME}"
	cache="/var/lib/kubelet/pods/${uid}/volumes/kubernetes.io~empty-dir/cache"
	# The Host Agent pod's fixed SELinux level, which every one of its
	# containers runs at.
	level="$(sed -n 's/^ *level: "\(s0:c[0-9]*,c[0-9]*\)"$/\1/p' "${ROOT}/deploy/host-agent/daemonset.yaml" | head -n 1)"
	avc="$(avc_denials)"
	log "The Host Agent on ${NODE_NAME}"
	{
		echo "--- getenforce ---"
		host_run getenforce
		echo "--- kubectl get pods ---"
		kubectl -n "${NS}" get pods -l app.kubernetes.io/name=flintlock-host-agent -o wide
		echo "--- the Host Agent's processes on the Host ---"
		host_root sh -c 'ps -eo label,user,args | grep -E "zot|athens|nginx|buildkitd|flr host-services" | grep -v grep'
		echo "--- the cache emptyDir, ${cache} ---"
		host_root ls -laZ "${cache}"
		echo "--- what is in it ---"
		host_root sh -c "cd '${cache}' && du -sh ./*"
		echo "--- AVC denials since boot, from the journal ---"
		printf '%s\n' "${avc:-<none>}"
	} | tee "${report}"
	log "The Host Agent's report is ${report}"

	[[ "$(host_run getenforce)" == Enforcing ]] || die "SELinux is not enforcing on ${NODE_NAME}"
	host_root sh -c "find '${cache}' -mindepth 2 -type f | grep -q ." || die "no Host Service wrote into its cache in ${cache}"
	# The Host Agent's denials, but buildkitd's probe of the Host's
	# qemu-user-static emulators, which the Host Image registers and no
	# confined container may map (battery-operator#222). buildkitd runs
	# without them.
	agent="$(grep -F ":${level} " <<<"${avc}" | grep -v ' {map} /usr/bin/qemu-[a-z0-9_]*-static ' || true)"
	[[ -z "${agent}" ]] || die "SELinux denied the Host Agent: ${agent}"
	pass "job ${JOB_ID} used the Go module proxy and the registry mirror; the Host Services run confined, with no denial but battery-operator#222's, and keep their cache in the emptyDir"
}

# ---------------------------------------------------------------------------
# The Docker and kind images: docs/docker-and-kind.md's example Jobs, on
# the Profiles flr/docker and flr/kind. Each Job prints its timings as
# TIMING <what> <seconds> lines, and when its services started in the guest
# as BOOT <unit> <ms> lines, counted from the start of the kernel. Each step
# adds them to .state/timings.txt and reports the AVC denials on the Host.

# need_profile NAME fails unless PROFILES names the Profile NAME and the
# images step found its image. Pass the PROFILES that deploy had.
need_profile() {
	load_images
	profile_kept "$1" ||
		die "the ${1} Profile is not deployed: run deploy with PROFILES=${1}, after images with GUEST=build or a RELEASE that has its image"
}

# trace_lines prints the Job's log as the Job wrote it: without the time,
# the stream tag and the colours that GitLab's trace format adds.
trace_lines() {
	sed -E 's/^[0-9-]+T[0-9:.]+Z [0-9]+[OE]\+? ?//; s/\x1b\[[0-9;]*m//g; s/\r$//' <<<"${JOB_TRACE}"
}

# record NAME writes the Job's duration and its TIMING and BOOT lines to
# .state/timings.txt and prints them, then the Host's AVC denials since boot.
record() {
	local name=$1 lines
	lines="$(trace_lines | grep -E '^(TIMING|BOOT) [a-z-]+ [0-9.]+( ms)?$' || true)"
	{
		echo "${name} job ${JOB_ID}: ${JOB_STATUS}, ${JOB_SECONDS}s from queued to the end"
		printf '%s\n' "${lines}" | sed 's/^/  /'
	} | tee -a "${STATE_DIR}/timings.txt" >&2
	log "AVC denials on ${NODE_NAME} since boot"
	avc_denials >&2
	log "SELinux on ${NODE_NAME} is $(host_run getenforce)"
}

# expect_trace PATTERN WHAT fails unless the Job's trace has PATTERN.
expect_trace() {
	trace_lines | grep -qE -- "$1" || die "job ${JOB_ID}'s trace shows no $2"
}

# The checks every Job on the Docker image makes of the guest.
check_docker_guest() {
	expect_trace 'storage driver overlay2, cgroup v2 \(systemd\), registry mirrors \[http://[0-9.]+:5000/?\]' \
		"Docker Engine on overlay2 and cgroup v2 with the Host's registry mirror"
	expect_trace '^iptables v[0-9.]+ \(legacy\)$' "iptables on its legacy backend"
	expect_trace '^-A POSTROUTING -s 172\.17\.0\.0/16 ! -o docker0 -j MASQUERADE$' "Docker Engine's NAT rule"
	expect_trace '^BOOT docker [1-9][0-9]* ms$' "the time Docker Engine started"
}

step_cache-demo() {
	need_profile docker
	run_job "${HERE}/job-cache-demo.json" cache-demo "${DOCKER_POOL}"
	check_job_passed
	check_docker_guest
	expect_trace '^TIMING docker-pull [0-9.]+$' "timed docker pull"
	expect_trace '^TIMING go-mod-download [0-9.]+$' "timed module download"
	record cache-demo
	pass "job ${JOB_ID} pulled an image through the registry mirror and Go modules through GOPROXY on the Docker image"
}

step_build-demo() {
	need_profile docker
	run_job "${HERE}/job-build-demo.json" build-demo "${DOCKER_POOL}"
	check_job_passed
	check_docker_guest
	expect_trace '^TIMING buildx-build [0-9.]+$' "timed buildx build"
	expect_trace '^hello from a flintlock-runner MicroVM$' "output of the image it built"
	record build-demo
	pass "job ${JOB_ID} built with Go, built an image on the Host's buildkitd and ran it, on the Docker image"
}

step_kind() {
	need_profile kind
	run_job "${HERE}/job-kind.json" kind "${KIND_POOL}"
	check_job_passed
	check_docker_guest
	expect_trace 'Kubernetes control plane.* is running at https://127\.0\.0\.1:[0-9]+' "kubectl cluster-info"
	expect_trace '^pod/nginx condition met$' "nginx Ready"
	expect_trace '^flr-control-plane +Ready ' "kind node Ready"
	record kind
	pass "job ${JOB_ID} made a kind cluster, ran nginx in it and saw it Ready, on the kind image"
}

# ---------------------------------------------------------------------------
# Faults: a Job that runs for ten minutes, a fault on the Host while it
# runs, and the Job failing as runner_system_failure soon after (KF-200 to
# KF-205).

# fault NAME INJECT RESTORE runs job-long.json, runs INJECT on the Host once
# the Job's script runs, and waits for the Job to fail. Then it runs RESTORE
# and waits for the Host and the Pool to be ready again.
fault() {
	local name=$1 inject=$2 restore=$3 id status="" t0 t1 took reason claims after claims_after out
	mkdir -p "${STATE_DIR}"
	# The Pool's Ready condition can say AtSize while nothing is available
	# (battery-operator#223), so this looks for a MicroVM itself.
	wait_until 120 has_available ||
		die "the Pool ${pool} has no available MicroVM: $(kubectl -n "${NS}" get pool "${pool}" -o jsonpath='{.status}')"
	log "Queueing job-long.json on the fake GitLab"
	id="$(queue "${HERE}/job-long.json")"
	log "Job ${id} is queued; waiting for its script to run"
	for _ in $(seq 1 120); do
		[[ "$(fake "jobs/${id}/trace" 2>/dev/null || true)" == *"tick 5"* ]] && break
		sleep 2
	done
	[[ "$(job_status "${id}")" == running ]] || die "job ${id} is $(job_status "${id}"), not running"
	# battery replaces the claimed MicroVM. A replacement still booting when
	# flintlockd goes leaks on the Host, and the Pool stops refilling
	# (battery-operator#223), so the fault waits for it.
	log "Waiting for battery to replace the Job's MicroVM in the Pool"
	wait_until 180 has_available || die "battery did not replace the Job's MicroVM in the Pool ${POOL}"
	claims="$(kubectl get microvmclaims -A 2>&1)"
	printf '%s\n' "${claims}" >&2

	log "Fault on ${NODE_NAME}: ${inject}"
	t0=$(date +%s.%N)
	host_root sh -c "${inject}"
	for _ in $(seq 1 "${FAULT_WAIT}"); do
		status="$(job_status "${id}")"
		finished "${status}" && break
		sleep 1
	done
	t1=$(date +%s.%N)
	took="$(awk -v a="${t0}" -v b="${t1}" 'BEGIN { printf "%.1f", b - a }')"
	reason="$(fake "jobs/${id}/failure_reason" 2>/dev/null || true)"
	after="$(wait_claims_gone)"
	claims_after="$(kubectl get microvmclaims -A -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,DELETING:.metadata.deletionTimestamp' 2>&1)"

	log "Restoring ${NODE_NAME}: ${restore}"
	host_root sh -c "${restore}"
	log "Waiting for ${NODE_NAME} and the Pool ${POOL} to be ready again"
	wait_until 300 is_host_ready || die "${NODE_NAME} is not ready again after the fault"
	wait_until 300 is_pool_ready || die "the Pool ${POOL} is not Ready again after the fault"
	wait_until 180 no_claims || die "the Job's claim is still there after the fault"

	out="${STATE_DIR}/run-${name}-$(date -u +%Y%m%dT%H%M%SZ).log"
	{
		echo "job ${id}: ${status:-unknown} (${reason:-no failure reason}), ${took}s after the fault"
		echo "--- kubectl get microvmclaims -A, before the fault ---"
		printf '%s\n' "${claims}"
		echo "--- the end of the trace ---"
		fake "jobs/${id}/trace" | tail -n 4 || true
		echo "--- the claims once the Job failed ---"
		printf '%s\n' "${claims_after}"
		echo "--- kubectl get microvmclaims -A, once the Host is ready again ---"
		kubectl get microvmclaims -A 2>&1
	} | tee "${out}"
	log "The run's log is ${out}"

	[[ "${status}" == failed ]] || die "job ${id} is ${status:-still running} ${took}s after the fault"
	[[ "${reason}" == runner_system_failure ]] || die "job ${id} failed with ${reason:-no reason}, not runner_system_failure"
	[[ -z "${after}" ]] || die "the Runner did not delete the Job's claim: ${after}"
	pass "job ${id} failed as runner_system_failure ${took}s after the fault, and its claim is gone"
}

no_claims() { [[ -z "$(kubectl -n "${NS}" get microvmclaims -o name 2>/dev/null)" ]]; }

# fault-flintlockd stops flintlockd on the Host. flintlockd.path starts it
# again while its certificate exists, so the path unit stops too.
step_fault-flintlockd() {
	fault flintlockd \
		"systemctl stop flintlockd.path flintlockd.service" \
		"systemctl start flintlockd.path flintlockd.service"
}

# fault-flintlockd-hang freezes flintlockd with SIGSTOP. Its connections
# stay open and nothing answers on them, so the Stage's exec stream does
# not end. battery-operator's Exec Agent reports the Host not ready
# (FlintlockdNotReady), and the Runner's probe fails too.
step_fault-flintlockd-hang() {
	fault flintlockd-hang \
		"kill -STOP \$(systemctl show -p MainPID --value flintlockd.service)" \
		"kill -CONT \$(systemctl show -p MainPID --value flintlockd.service)"
}

# fault-exec-agent-hang freezes battery-operator's Exec Agent with SIGSTOP.
# It no longer answers, and no longer reports, so its Node keeps saying
# the Host is ready: only the Runner's probe of the Exec Agent sees the
# Host go (KF-200 to KF-202). The Stage's stream, through the same Exec
# Agent, stalls without ending.
step_fault-exec-agent-hang() {
	fault exec-agent-hang "pkill -STOP -x exec-agent" "pkill -CONT -x exec-agent"
}

# fault-not-ready gives the Host a not ready reason, as the Host Image's
# units do when the KVM device or the thin pool goes. flintlockd keeps
# answering.
step_fault-not-ready() {
	fault not-ready \
		"echo 'flintlock-runner hack/real-host: the Host is not ready' >${NOT_READY_FILE}" \
		"rm -f ${NOT_READY_FILE}"
}

# ---------------------------------------------------------------------------
# clean: the Fleet Manifests, their Pool and the fake GitLab go;
# battery-operator stays. The Runner never deletes its Pools, so this does.

step_clean() {
	log "Deleting the Host Agent and the Runner"
	kubectl -n "${NS}" delete deployment flintlock-runner --ignore-not-found --wait=true >/dev/null 2>&1 || true
	kubectl -n "${NS}" delete daemonset flintlock-host-agent --ignore-not-found --wait=true >/dev/null 2>&1 || true
	log "Deleting the fake GitLab"
	kubectl -n "${NS}" delete deployment,service fake-gitlab --ignore-not-found --wait=true >/dev/null 2>&1 || true
	if kubectl get namespace "${NS}" >/dev/null 2>&1; then
		log "Deleting the claims and Pools in ${NS}"
		kubectl -n "${NS}" delete microvmclaims --all --wait=true --timeout=180s >/dev/null
		kubectl -n "${NS}" delete pools --all --wait=true --timeout=300s >/dev/null
	fi
	# What is left of the Fleet Manifests, the namespace and the objects
	# outside it among them.
	if [[ -f "${STATE_DIR}/images.env" ]]; then
		load_images
		log "Deleting the rest of the Fleet Manifests and the namespace ${NS}"
		kubectl kustomize "$(overlay)" | kubectl delete --ignore-not-found --wait=true --timeout=180s -f - >/dev/null
	fi
	kubectl delete namespace "${NS}" --ignore-not-found --wait=true --timeout=180s >/dev/null
	kubectl get pools,microvmclaims -A
	pass "no Runner, no Host Agent, no Pools and no claims are left; battery-operator stays"
}

main() {
	[[ -n "${KUBECONFIG:-}" ]] || die "set KUBEADM_HOST to battery-operator's hack/kubeadm-host, or KUBECONFIG"
	local steps=("$@") s
	[[ ${#steps[@]} -gt 0 ]] || steps=(images deploy job services)
	keep_awake
	for s in "${steps[@]}"; do
		case "${s}" in
		images | deploy | job | services | cache-demo | build-demo | kind | fault-flintlockd | fault-flintlockd-hang | fault-exec-agent-hang | fault-not-ready | clean) "step_${s}" ;;
		*) die "unknown step ${s}: images deploy job services cache-demo build-demo kind fault-flintlockd fault-flintlockd-hang fault-exec-agent-hang fault-not-ready clean" ;;
		esac
	done
}

main "$@"
