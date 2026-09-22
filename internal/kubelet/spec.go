package kubelet

import (
	"encoding/base64"
	"fmt"
	"math"
	"strings"

	"github.com/liquidmetal-dev/flintlock/api/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// Labels the provider puts on every MicroVM (KF-021). They are how a MicroVM
// is matched to its pod again after a restart (KF-028).
const (
	vmLabelPodUID       = kubelabels.Prefix + "pod-uid"
	vmLabelPodNamespace = kubelabels.Prefix + "pod-namespace"
	vmLabelPodName      = kubelabels.Prefix + "pod-name"
)

// The initrd of a Profile, as the Kubernetes pool backend writes it on a
// pod. They are kept here rather than in kubelabels so that the two work
// packages do not both edit that file; they belong there once both land.
const (
	annotationInitrdImage    = kubelabels.Prefix + "initrd-image"
	annotationInitrdFilename = kubelabels.Prefix + "initrd-filename"
)

// Fixed parts of every MicroVM spec.
const (
	// rootVolumeID is the id of the root volume; flintlock tells the root
	// volume by position, so the value only has to be stable.
	rootVolumeID = "root"
	// guestDeviceID is the guest's network interface. It is not `eth0`,
	// which Firecracker reserves (PL-023).
	guestDeviceID = "eth1"
	// metadataUserData is the cloud-init metadata key for user data.
	metadataUserData = "user-data"
	// mebibyte converts a memory limit to flintlock's unit.
	mebibyte = 1024 * 1024
)

// unsupportedError says which field of a pod the provider cannot realise
// (KF-022).
type unsupportedError struct {
	field  string
	detail string
}

func (e *unsupportedError) Error() string {
	return fmt.Sprintf("%s %s: a MicroVM pod is one container image booted as a machine", e.field, e.detail)
}

func unsupported(field, detail string) error {
	return &unsupportedError{field: field, detail: detail}
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# If a pod has more than one container, an init container, a
//# volume, a host namespace or a mounted service account token, then the Pod
//# Provider SHALL mark the pod failed with a reason naming the unsupported
//# field and SHALL NOT create a MicroVM for it.

// validatePod refuses every pod shape a MicroVM has nowhere to put. The
// service account token comes first because, where the cluster mounts one,
// it also shows up as a volume, and the field the pod's author can change is
// automountServiceAccountToken. The error names the field.
func validatePod(pod *corev1.Pod) error {
	spec := &pod.Spec
	switch {
	case len(spec.Containers) != 1:
		return unsupported("spec.containers", fmt.Sprintf("has %d entries and has to have exactly one", len(spec.Containers)))
	case len(spec.InitContainers) > 0:
		return unsupported("spec.initContainers", "is not supported")
	case len(spec.EphemeralContainers) > 0:
		return unsupported("spec.ephemeralContainers", "is not supported")
	case spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken:
		return unsupported("spec.automountServiceAccountToken", "has to be false, a service account token cannot be mounted into a MicroVM")
	case len(spec.Volumes) > 0:
		return unsupported("spec.volumes", "is not supported")
	case spec.HostNetwork:
		return unsupported("spec.hostNetwork", "is not supported")
	case spec.HostPID:
		return unsupported("spec.hostPID", "is not supported")
	case spec.HostIPC:
		return unsupported("spec.hostIPC", "is not supported")
	case len(spec.Containers[0].VolumeMounts) > 0:
		return unsupported("spec.containers[0].volumeMounts", "is not supported")
	}
	return nil
}

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# When a pod is bound to the Virtual Node, the Pod Provider SHALL
//# create one MicroVM for it through `flintlockd`, using the image of the
//# pod's only container as the root filesystem image, the container's CPU
//# and memory limits as the MicroVM's vCPU count and memory, and the pod's
//# `gitlab-runner.flintlock.dev` annotations for the kernel image, the
//# kernel command line and the hypervisor.

//= docs/requirements/12-cluster-fleet.md#microvm-pods
//# The Pod Provider SHALL label the MicroVM with the pod's UID,
//# namespace and name and SHALL set `allow_guest_agent` on it.

// buildMicroVMSpec translates a validated pod into the MicroVM it stands
// for. userData is the cloud-init user data of KF-023, empty when the pod
// names none. The MicroVM's id is the pod's UID, which is unique where a pod
// name is not across namespaces, and fits any rule flintlock has for ids;
// the uid is left for flintlockd to assign.
func buildMicroVMSpec(pod *corev1.Pod, namespace, userData string) (*types.MicroVMSpec, error) {
	container := &pod.Spec.Containers[0]
	if container.Image == "" {
		return nil, unsupported("spec.containers[0].image", "is empty and is the root filesystem image")
	}
	vcpu, err := vcpuLimit(container.Resources.Limits)
	if err != nil {
		return nil, err
	}
	memory, err := memoryLimit(container.Resources.Limits)
	if err != nil {
		return nil, err
	}
	kernel := pod.Annotations[kubelabels.AnnotationKernelImage]
	if kernel == "" {
		return nil, unsupported("metadata.annotations["+kubelabels.AnnotationKernelImage+"]", "is required")
	}

	image := container.Image
	spec := &types.MicroVMSpec{
		Id:         string(pod.UID),
		Namespace:  namespace,
		Labels:     microVMLabels(pod),
		Vcpu:       vcpu,
		MemoryInMb: memory,
		Kernel: &types.Kernel{
			Image:            kernel,
			Cmdline:          parseCmdline(pod.Annotations[kubelabels.AnnotationKernelCmdline]),
			AddNetworkConfig: true,
		},
		RootVolume: &types.Volume{
			Id:     rootVolumeID,
			Source: &types.VolumeSource{ContainerSource: &image},
		},
		Interfaces: []*types.NetworkInterface{{
			DeviceId: guestDeviceID,
			Type:     types.NetworkInterface_TAP,
		}},
		// AllowGuestAgent attaches the vsock device MicroVMExec needs, and
		// with it the readiness probe of KF-024 and every Job.
		AllowGuestAgent: true,
	}
	if filename := pod.Annotations[kubelabels.AnnotationKernelFilename]; filename != "" {
		spec.Kernel.Filename = &filename
	}
	if initrd := pod.Annotations[annotationInitrdImage]; initrd != "" {
		spec.Initrd = &types.Initrd{Image: initrd}
		if filename := pod.Annotations[annotationInitrdFilename]; filename != "" {
			spec.Initrd.Filename = &filename
		}
	}
	if provider := pod.Annotations[kubelabels.AnnotationHypervisor]; provider != "" {
		spec.Provider = &provider
	}
	if userData != "" {
		spec.Metadata = map[string]string{
			metadataUserData: base64.StdEncoding.EncodeToString([]byte(userData)),
		}
	}
	return spec, nil
}

// microVMLabels identify the pod a MicroVM belongs to (KF-021).
func microVMLabels(pod *corev1.Pod) map[string]string {
	return map[string]string{
		vmLabelPodUID:       string(pod.UID),
		vmLabelPodNamespace: pod.Namespace,
		vmLabelPodName:      pod.Name,
	}
}

// vcpuLimit is the CPU limit in whole vCPUs, rounded up: a MicroVM has no
// fractional cores.
func vcpuLimit(limits corev1.ResourceList) (int32, error) {
	cpu, ok := limits[corev1.ResourceCPU]
	if !ok || cpu.IsZero() {
		return 0, unsupported("spec.containers[0].resources.limits.cpu", "is required and is the MicroVM's vCPU count")
	}
	cores := cpu.ScaledValue(resource.Milli)
	vcpu := (cores + 999) / 1000
	if vcpu > math.MaxInt32 {
		return 0, unsupported("spec.containers[0].resources.limits.cpu", "is too large")
	}
	return int32(vcpu), nil
}

// memoryLimit is the memory limit in MiB, rounded up.
func memoryLimit(limits corev1.ResourceList) (int32, error) {
	memory, ok := limits[corev1.ResourceMemory]
	if !ok || memory.IsZero() {
		return 0, unsupported("spec.containers[0].resources.limits.memory", "is required and is the MicroVM's memory")
	}
	mib := (memory.Value() + mebibyte - 1) / mebibyte
	if mib > math.MaxInt32 {
		return 0, unsupported("spec.containers[0].resources.limits.memory", "is too large")
	}
	return int32(mib), nil
}

// parseCmdline reads a kernel command line into the map flintlock takes: a
// `key=value` word becomes an entry and a bare word an entry with an empty
// value.
func parseCmdline(line string) map[string]string {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	out := make(map[string]string, len(fields))
	for _, field := range fields {
		key, value, _ := strings.Cut(field, "=")
		out[key] = value
	}
	return out
}
