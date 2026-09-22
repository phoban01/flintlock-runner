package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// poolLabels are the two labels that identify a Pool: every object of the
// Pool carries them, and with LabelState they select its pods.
func (b *Backend) poolLabels(profile string) map[string]string {
	return map[string]string{
		LabelRunner:  labelValue(b.opts.RunnerName),
		LabelProfile: labelValue(profile),
	}
}

// idleSelector selects a Pool's idle pods, which is the selector of its
// ReplicaSet (KF-043).
func (b *Backend) idleSelector(profile string) map[string]string {
	sel := b.poolLabels(profile)
	sel[LabelState] = StateIdle
	return sel
}

// runnerSelector selects every object this Runner owns, of every Pool. It is
// what the informers watch.
func (b *Backend) runnerSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{LabelRunner: labelValue(b.opts.RunnerName)})
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# Where the Kubernetes pool backend is configured, the Scheduler
//# SHALL create or update one ReplicaSet per Profile in the Runner's
//# namespace, with the Pool size as its replica count and a pod template
//# derived from the Profile as KF-020 expects.

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Scheduler SHALL label every Pool pod with the Runner name,
//# the Profile name and `gitlab-runner.flintlock.dev/state` set to `idle`,
//# and SHALL select the ReplicaSet's pods by all three.

// replicaSet renders a Pool as its ReplicaSet. The replica count is the Pool
// size and the selector is the three labels of KF-043, so that a pod whose
// state label changes to claimed stops counting and is replaced at once. What
// GetPool has to give back and a ReplicaSet has no field for travels in
// annotations.
func (b *Backend) replicaSet(spec poolmgr.PoolSpec, profile config.Profile) (*appsv1.ReplicaSet, error) {
	template, err := b.podTemplate(spec, profile)
	if err != nil {
		return nil, err
	}
	replicas := spec.Size
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replicaSetName(spec.Ref),
			Namespace: b.opts.Namespace,
			Labels:    b.poolLabels(profile.Name),
			Annotations: map[string]string{
				annotationPoolName:        spec.Ref.Name,
				annotationPoolNamespace:   spec.Ref.Namespace,
				annotationHeartbeat:       spec.HeartbeatInterval.String(),
				annotationHeartbeatExpiry: spec.HeartbeatExpiryThreshold.String(),
			},
		},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: b.idleSelector(profile.Name)},
			Template: template,
		},
	}, nil
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Scheduler SHALL give every Pool pod a toleration for the
//# taint of KF-014 and a node selector built from the Profile's architecture
//# and Host selector and the label `gitlab-runner.flintlock.dev/virtual-node`.

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Scheduler SHALL give every Pool pod a topology spread
//# constraint over Virtual Nodes, so that a Pool's warm MicroVMs are spread
//# across Hosts.

// podTemplate renders the pod the Pod Provider turns into one MicroVM
// (KF-020): a single container whose image is the root filesystem image and
// whose limits are the vCPU count and the memory, the kernel and the
// hypervisor in annotations, and nothing a MicroVM has no place for, which
// is why the service account token is not mounted (KF-022). The pod only
// fits a Virtual Node of the Profile's architecture that carries the
// Profile's Host selector labels, tolerates that Node's taint and is spread
// over Hosts. The template hash is computed last, over everything else.
func (b *Backend) podTemplate(spec poolmgr.PoolSpec, profile config.Profile) (corev1.PodTemplateSpec, error) {
	vm := spec.Template
	if vm == nil {
		return corev1.PodTemplateSpec{}, fmt.Errorf("%w: pool %s has no microvm template", poolmgr.ErrInvalid, spec.Ref)
	}
	rootfs := vm.GetRootVolume().GetSource().GetContainerSource()
	if rootfs == "" || vm.GetKernel().GetImage() == "" {
		return corev1.PodTemplateSpec{}, fmt.Errorf("%w: pool %s needs a root filesystem image and a kernel image", poolmgr.ErrInvalid, spec.Ref)
	}
	if vm.GetVcpu() <= 0 || vm.GetMemoryInMb() <= 0 {
		return corev1.PodTemplateSpec{}, fmt.Errorf("%w: pool %s needs a vcpu count and a memory size", poolmgr.ErrInvalid, spec.Ref)
	}

	annotations := map[string]string{AnnotationKernelImage: vm.GetKernel().GetImage()}
	setIf := func(key, value string) {
		if value != "" {
			annotations[key] = value
		}
	}
	setIf(AnnotationKernelFilename, vm.GetKernel().GetFilename())
	setIf(AnnotationKernelCmdline, cmdline(vm.GetKernel().GetCmdline()))
	setIf(AnnotationInitrdImage, vm.GetInitrd().GetImage())
	setIf(AnnotationInitrdFilename, vm.GetInitrd().GetFilename())
	setIf(AnnotationHypervisor, vm.GetProvider())
	setIf(AnnotationCloudInit, b.opts.CloudInitConfigMaps[profile.Name])

	limits := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewQuantity(int64(vm.GetVcpu()), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(int64(vm.GetMemoryInMb())<<20, resource.BinarySI),
	}
	no := false
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{
			Labels:      b.idleSelector(profile.Name),
			Annotations: annotations,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  containerName,
				Image: rootfs,
				// Requests default to the limits, so the scheduler counts a
				// MicroVM against its Virtual Node at its full size (KF-012).
				Resources: corev1.ResourceRequirements{Limits: limits},
			}},
			RestartPolicy:                corev1.RestartPolicyAlways,
			AutomountServiceAccountToken: &no,
			EnableServiceLinks:           &no,
			NodeSelector:                 nodeSelector(profile),
			Tolerations: []corev1.Toleration{{
				Key:      TaintMicroVM,
				Operator: corev1.TolerationOpEqual,
				Value:    "true",
				Effect:   corev1.TaintEffectNoSchedule,
			}},
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{
				MaxSkew:     1,
				TopologyKey: LabelHostNode,
				// Spreading is a preference: a Pool larger than the fleet
				// can spread still has to reach its size.
				WhenUnsatisfiable: corev1.ScheduleAnyway,
				LabelSelector:     &metav1.LabelSelector{MatchLabels: b.idleSelector(profile.Name)},
			}},
		},
	}
	hash, err := templateHash(template)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	template.Labels[LabelTemplateHash] = hash
	return template, nil
}

// nodeSelector confines a Pool's pods to the Virtual Nodes of the Profile's
// architecture that carry every label of its Host selector (KF-041). A Host
// selector key without a domain is looked for under this project's, because
// those are the Host labels the Pod Provider copies to the Virtual Node
// (KF-013); a key that names its own domain is used as it is.
func nodeSelector(profile config.Profile) map[string]string {
	sel := map[string]string{
		LabelVirtualNode: "true",
		LabelArch:        string(profile.Arch),
	}
	for key, value := range profile.HostSelector {
		if !strings.Contains(key, "/") {
			key = Domain + "/" + key
		}
		sel[key] = value
	}
	return sel
}

// cmdline renders the Profile's additional kernel arguments as one string,
// in key order so that the same Profile always renders the same template.
func cmdline(args map[string]string) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	words := make([]string, 0, len(keys))
	for _, k := range keys {
		if args[k] == "" {
			words = append(words, k)
			continue
		}
		words = append(words, k+"="+args[k])
	}
	return strings.Join(words, " ")
}

// templateHash identifies a pod template. It is a hash of the template's
// JSON, which encoding/json renders with map keys in order, so it changes
// exactly when something the Pod Provider or the scheduler reads changes.
func templateHash(template corev1.PodTemplateSpec) (string, error) {
	data, err := json.Marshal(template)
	if err != nil {
		return "", fmt.Errorf("kube: hashing the pod template: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]), nil
}

// poolFromReplicaSet is the PoolSpec a ReplicaSet stands for. The MicroVM
// template is not reconstructed: nothing above the interface reads it back,
// and the Pod Provider's reading of the pod template is the one that counts.
func poolSpecFromReplicaSet(rs *appsv1.ReplicaSet) poolmgr.PoolSpec {
	spec := poolmgr.PoolSpec{
		Ref: poolmgr.PoolRef{
			Name:      rs.Annotations[annotationPoolName],
			Namespace: rs.Annotations[annotationPoolNamespace],
		},
	}
	if rs.Spec.Replicas != nil {
		spec.Size = *rs.Spec.Replicas
	}
	spec.HeartbeatInterval, _ = time.ParseDuration(rs.Annotations[annotationHeartbeat])
	spec.HeartbeatExpiryThreshold, _ = time.ParseDuration(rs.Annotations[annotationHeartbeatExpiry])
	return spec
}

// currentHash is the hash of a ReplicaSet's current pod template.
func currentHash(rs *appsv1.ReplicaSet) string {
	return rs.Spec.Template.Labels[LabelTemplateHash]
}

// deadlineSeconds renders a Job timeout as a pod's active deadline, which is
// in whole seconds and at least one.
func deadlineSeconds(d time.Duration) int64 {
	secs := int64((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// formatLease renders a time as the value of AnnotationLease.
func formatLease(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// LeaseTime reads a pod's lease annotation: when it was claimed or last
// heartbeated. It reports false for a pod that carries none. The Pod Provider
// compares it with its lease duration (KF-032).
func LeaseTime(pod *corev1.Pod) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, pod.Annotations[AnnotationLease])
	return t, err == nil
}
