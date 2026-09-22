package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// The keys the Pod Provider reads are shared with it through
// internal/kubelabels. They are repeated here under the same names, so that
// the backend and its tests read as one vocabulary.
const (
	// LabelRunner names the Runner that declared the Pool and LabelProfile
	// the Profile it was derived from (KF-043). They are the labels battery's
	// MicroVM templates carry (PL-014), under the same keys.
	LabelRunner  = poolmgr.LabelRunner
	LabelProfile = poolmgr.LabelProfile
	// LabelState is StateIdle on a warm pod and StateClaimed on a leased
	// one. It is part of the ReplicaSet's selector, so changing it is what
	// takes a pod out of its Pool (KF-043, KF-044).
	LabelState   = kubelabels.LabelState
	StateIdle    = kubelabels.StateIdle
	StateClaimed = kubelabels.StateClaimed
	// LabelTemplateHash is the hash of the pod template a pod was created
	// from. A pod whose hash is not its ReplicaSet's is a pod of a previous
	// template: it is never claimed (KF-044) and is rolled out (KF-050).
	LabelTemplateHash = kubelabels.Prefix + "template-hash"
	// LabelVirtualNode is set to "true" on every Virtual Node (KF-013).
	LabelVirtualNode = kubelabels.LabelVirtualNode
	// LabelHostNode is the name of the Host's own Node on its Virtual Node
	// (KF-013). There is one value per Host, which makes it the topology
	// key Pool pods are spread over (KF-042).
	LabelHostNode = kubelabels.LabelHostNode
	// LabelArch is the well-known architecture label, which the Pod Provider
	// copies to the Virtual Node (KF-013).
	LabelArch = "kubernetes.io/arch"
	// TaintMicroVM is the key of the Virtual Node's taint, with the value
	// "true" and the effect NoSchedule (KF-014).
	TaintMicroVM = kubelabels.TaintMicroVM
)

// Annotations of a Pool pod that the Pod Provider builds the MicroVM from
// (KF-020, KF-023). The root filesystem image, the vCPU count and the memory
// are the pod's container image and limits and need no annotation.
const (
	AnnotationKernelImage    = kubelabels.AnnotationKernelImage
	AnnotationKernelFilename = kubelabels.AnnotationKernelFilename
	// AnnotationKernelCmdline is the additional kernel command line: the
	// Profile's arguments as space-separated key=value words in key order.
	AnnotationKernelCmdline = kubelabels.AnnotationKernelCmdline
	// AnnotationHypervisor is the flintlock provider name. It is absent
	// where the Profile leaves the choice to the Host.
	AnnotationHypervisor = kubelabels.AnnotationHypervisor
	// AnnotationCloudInit names the ConfigMap that holds the MicroVM's
	// cloud-init user data (KF-023).
	AnnotationCloudInit = kubelabels.AnnotationCloudInitConfigMap
	// AnnotationInitrdImage and AnnotationInitrdFilename are the initial
	// ramdisk, where the Profile has one. internal/kubelabels has no key for
	// them yet, so nothing reads them.
	AnnotationInitrdImage    = kubelabels.Prefix + "initrd-image"
	AnnotationInitrdFilename = kubelabels.Prefix + "initrd-filename"
	// AnnotationLease is the lease annotation of a claimed pod: the time of
	// the claim or of the last heartbeat (KF-044, KF-047). The Pod Provider
	// deletes a claimed pod whose lease is older than its lease duration
	// (KF-032).
	AnnotationLease = kubelabels.AnnotationLease
)

// Annotations of a Pool's ReplicaSet. They carry what GetPool has to return
// and a ReplicaSet has no field for.
const (
	annotationPoolName        = kubelabels.Prefix + "pool-name"
	annotationPoolNamespace   = kubelabels.Prefix + "pool-namespace"
	annotationHeartbeat       = kubelabels.Prefix + "heartbeat-interval"
	annotationHeartbeatExpiry = kubelabels.Prefix + "heartbeat-expiry"
	// annotationRolledAt is when an idle pod of a previous template was last
	// deleted. It lives on the ReplicaSet so that several instances of one
	// Runner share a single rate (KF-050).
	annotationRolledAt = kubelabels.Prefix + "rolled-at"
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
