package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// Domain prefixes every label, annotation and taint of this project.
const Domain = "gitlab-runner.flintlock.dev"

// Labels of a Pool's ReplicaSet and of its pods. The first two are the
// labels battery's MicroVM templates carry (PL-014), under the same keys.
const (
	// LabelRunner names the Runner that declared the Pool (KF-043).
	LabelRunner = poolmgr.LabelRunner
	// LabelProfile names the Profile the Pool was derived from (KF-043).
	LabelProfile = poolmgr.LabelProfile
	// LabelState is StateIdle on a warm pod and StateClaimed on a leased
	// one. It is part of the ReplicaSet's selector, so changing it is what
	// takes a pod out of its Pool (KF-043, KF-044).
	LabelState = Domain + "/state"
	// LabelTemplateHash is the hash of the pod template a pod was created
	// from. A pod whose hash is not its ReplicaSet's is a pod of a previous
	// template: it is never claimed (KF-044) and is rolled out (KF-050).
	LabelTemplateHash = Domain + "/template-hash"
	// LabelVirtualNode is set to "true" on every Virtual Node (KF-013).
	LabelVirtualNode = Domain + "/virtual-node"
	// LabelHostNode is the name of the Host's own Node on its Virtual Node
	// (KF-013). There is one value per Host, which makes it the topology
	// key Pool pods are spread over (KF-042).
	LabelHostNode = Domain + "/host-node"
	// LabelArch is the well-known architecture label, which the Pod Provider
	// copies to the Virtual Node (KF-013).
	LabelArch = "kubernetes.io/arch"
)

// Values of LabelState.
const (
	StateIdle    = "idle"
	StateClaimed = "claimed"
)

// TaintMicroVM is the key of the Virtual Node's taint, with the value "true"
// and the effect NoSchedule (KF-014).
const TaintMicroVM = Domain + "/microvm"

// Annotations of a Pool pod that the Pod Provider builds the MicroVM from
// (KF-020, KF-023). The root filesystem image, the vCPU count and the memory
// are the pod's container image and limits and need no annotation.
const (
	// AnnotationKernelImage is the kernel OCI image.
	AnnotationKernelImage = Domain + "/kernel-image"
	// AnnotationKernelFilename is the kernel file inside that image, where
	// the Profile names one.
	AnnotationKernelFilename = Domain + "/kernel-filename"
	// AnnotationKernelCmdline is the additional kernel command line: the
	// Profile's arguments as space-separated key=value words in key order.
	AnnotationKernelCmdline = Domain + "/kernel-cmdline"
	// AnnotationInitrdImage and AnnotationInitrdFilename are the initial
	// ramdisk, where the Profile has one.
	AnnotationInitrdImage    = Domain + "/initrd-image"
	AnnotationInitrdFilename = Domain + "/initrd-filename"
	// AnnotationHypervisor is the flintlock provider name. It is absent
	// where the Profile leaves the choice to the Host.
	AnnotationHypervisor = Domain + "/hypervisor"
	// AnnotationCloudInit names the ConfigMap that holds the MicroVM's
	// cloud-init user data (KF-023).
	AnnotationCloudInit = Domain + "/cloud-init-configmap"
)

// AnnotationLease is the lease annotation of a claimed pod: the time of the
// claim or of the last heartbeat, in RFC 3339 with nanoseconds, UTC (KF-044,
// KF-047). The Pod Provider deletes a claimed pod whose lease annotation is
// older than its lease duration (KF-032).
const AnnotationLease = Domain + "/lease-renewed-at"

// Annotations of a Pool's ReplicaSet. They carry what GetPool has to return
// and a ReplicaSet has no field for.
const (
	annotationPoolName        = Domain + "/pool-name"
	annotationPoolNamespace   = Domain + "/pool-namespace"
	annotationHeartbeat       = Domain + "/heartbeat-interval"
	annotationHeartbeatExpiry = Domain + "/heartbeat-expiry"
	// annotationRolledAt is when an idle pod of a previous template was last
	// deleted. It lives on the ReplicaSet so that several instances of one
	// Runner share a single rate (KF-050).
	annotationRolledAt = Domain + "/rolled-at"
)

// containerName is the name of a Pool pod's only container (KF-020).
const containerName = "microvm"

// maxLabelValue is the longest label value Kubernetes accepts, and
// maxReplicaSetName leaves room for the suffix the ReplicaSet controller
// appends to make a pod name of at most that length.
const (
	maxLabelValue     = 63
	maxReplicaSetName = 52
)

// labelValue turns a Runner or Profile name into a label value. A name that
// is one already is used as it is, so that `kubectl get pods -l` takes the
// name from the configuration; any other is reduced to the characters a
// label allows and made unique again with a hash of the original.
func labelValue(name string) string {
	return sanitise(name, maxLabelValue, true)
}

// replicaSetName is the name of a Pool's ReplicaSet: the Pool's namespace
// and name, which keeps the Pools of two Runners sharing one Kubernetes
// namespace apart as PL-017 keeps them apart at battery.
func replicaSetName(ref poolmgr.PoolRef) string {
	return sanitise(ref.Namespace+"-"+ref.Name, maxReplicaSetName, false)
}

// sanitise maps s onto lower-case alphanumerics and dashes (and, for a label
// value, the dots, underscores and capitals a label also allows), beginning
// and ending with an alphanumeric and no longer than limit. A string that had
// to be changed is suffixed with a hash of the original, so two names that
// differ only in what was removed stay distinct.
func sanitise(s string, limit int, label bool) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			if label {
				b.WriteRune(r)
			} else {
				b.WriteRune(r + 'a' - 'A')
			}
		case label && (r == '.' || r == '_'):
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-._")
	if out == s && len(out) <= limit && out != "" {
		return out
	}
	sum := sha256.Sum256([]byte(s))
	suffix := hex.EncodeToString(sum[:4])
	if room := limit - len(suffix) - 1; len(out) > room {
		out = strings.TrimRight(out[:room], "-._")
	}
	if out == "" {
		return suffix
	}
	return out + "-" + suffix
}
