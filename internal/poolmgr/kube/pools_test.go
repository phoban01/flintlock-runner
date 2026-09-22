package kube_test

import (
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/poolmgr"
	"github.com/phoban01/flintlock-runner/internal/poolmgr/kube"
)

// replicaSet reads the fixture's only ReplicaSet.
func (f *fixture) replicaSet() *appsv1.ReplicaSet {
	f.t.Helper()
	list, err := f.admin.AppsV1().ReplicaSets(f.namespace).List(f.ctx, metav1.ListOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(list.Items) != 1 {
		f.t.Fatalf("%d replicasets, want 1", len(list.Items))
	}
	return &list.Items[0]
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# Where the Kubernetes pool backend is configured, the Scheduler
//# SHALL create or update one ReplicaSet per Profile in the Runner's
//# namespace, with the Pool size as its replica count and a pod template
//# derived from the Profile as KF-020 expects.

// TestDeclareCreatesAndUpdatesTheReplicaSet declares a Pool through the real
// Declarer, which creates it, and again with another size, which updates it.
func TestDeclareCreatesAndUpdatesTheReplicaSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	ref := f.declare(b)

	rs := f.replicaSet()
	if rs.Namespace != f.namespace || *rs.Spec.Replicas != 2 {
		t.Errorf("replicaset in %s with %d replicas, want %s with 2", rs.Namespace, *rs.Spec.Replicas, f.namespace)
	}
	template := rs.Spec.Template
	if len(template.Spec.Containers) != 1 || len(template.Spec.InitContainers) != 0 || len(template.Spec.Volumes) != 0 {
		t.Fatalf("template has %d containers, %d init containers and %d volumes, want one container only",
			len(template.Spec.Containers), len(template.Spec.InitContainers), len(template.Spec.Volumes))
	}
	c := template.Spec.Containers[0]
	if c.Image != f.profile.RootFS {
		t.Errorf("container image = %q, want the root filesystem image %q", c.Image, f.profile.RootFS)
	}
	if cpu := c.Resources.Limits.Cpu().Value(); cpu != 2 {
		t.Errorf("cpu limit = %d, want the 2 vcpus", cpu)
	}
	if mem := c.Resources.Limits.Memory().Value(); mem != 4096<<20 {
		t.Errorf("memory limit = %d, want 4096 MiB", mem)
	}
	wantAnnotations := map[string]string{
		kube.AnnotationKernelImage:   f.profile.Kernel.Image,
		kube.AnnotationKernelCmdline: "console=ttyS0 quiet",
		kube.AnnotationHypervisor:    "firecracker",
	}
	for key, want := range wantAnnotations {
		if got := template.Annotations[key]; got != want {
			t.Errorf("annotation %s = %q, want %q", key, got, want)
		}
	}
	if _, set := template.Annotations[kube.AnnotationCloudInit]; set {
		t.Error("a cloud-init ConfigMap is named though none is configured")
	}
	if mount := template.Spec.AutomountServiceAccountToken; mount == nil || *mount {
		t.Error("the service account token would be mounted, which the Pod Provider refuses (KF-022)")
	}

	pool, err := b.GetPool(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if pool.Spec.Ref != ref || pool.Spec.Size != 2 || pool.Spec.HeartbeatExpiryThreshold != f.profile.Pool.HeartbeatExpiry {
		t.Errorf("GetPool spec = %+v", pool.Spec)
	}

	// Declaring again with another size updates the same ReplicaSet.
	f.profile.Pool.Size = 3
	if _, err := poolmgr.NewDeclarer(b).Declare(f.ctx, f.spec()); err != nil {
		t.Fatal(err)
	}
	f.waitAvailable(b, ref, 3)
	if rs := f.replicaSet(); *rs.Spec.Replicas != 3 {
		t.Errorf("replicas = %d after the update, want 3", *rs.Spec.Replicas)
	}

	pools, err := b.ListPools(f.ctx, ref.Namespace)
	if err != nil || len(pools) != 1 || pools[0].Status.Available != 3 {
		t.Errorf("ListPools = %+v, %v, want the one pool with 3 available", pools, err)
	}
	if pools, err := b.ListPools(f.ctx, "another-namespace"); err != nil || len(pools) != 0 {
		t.Errorf("ListPools of another pool namespace = %+v, %v, want none", pools, err)
	}
}

// TestPoolAdminErrors checks that the PoolAdmin methods report the battery
// client's sentinel errors, which is what the Declarer branches on.
func TestPoolAdminErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	spec := f.spec()

	if _, err := b.UpdatePool(f.ctx, spec); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("UpdatePool of an unknown pool = %v, want ErrNotFound", err)
	}
	if _, err := b.GetPool(f.ctx, spec.Ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("GetPool of an unknown pool = %v, want ErrNotFound", err)
	}
	if _, err := b.ClaimVM(f.ctx, spec.Ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("ClaimVM from an unknown pool = %v, want ErrNotFound", err)
	}
	if err := b.DeletePool(f.ctx, spec.Ref); !errors.Is(err, poolmgr.ErrNotFound) {
		t.Errorf("DeletePool of an unknown pool = %v, want ErrNotFound", err)
	}
	if _, err := b.CreatePool(f.ctx, spec); err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreatePool(f.ctx, spec); !errors.Is(err, poolmgr.ErrAlreadyExists) {
		t.Errorf("second CreatePool = %v, want ErrAlreadyExists", err)
	}

	unknown := f.spec()
	unknown.Template.Labels[poolmgr.LabelProfile] = "no-such-profile"
	if _, err := b.CreatePool(f.ctx, unknown); !errors.Is(err, poolmgr.ErrInvalid) {
		t.Errorf("CreatePool for an unknown profile = %v, want ErrInvalid", err)
	}

	// Teardown deletes the ReplicaSet and with it the idle pods.
	f.waitAvailable(b, spec.Ref, spec.Size)
	if err := b.DeletePool(f.ctx, spec.Ref); err != nil {
		t.Fatal(err)
	}
	f.eventually("the pool's pods are gone", func() bool { return len(f.pods()) == 0 })
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Scheduler SHALL give every Pool pod a toleration for the
//# taint of KF-014 and a node selector built from the Profile's architecture
//# and Host selector and the label `gitlab-runner.flintlock.dev/virtual-node`.

// TestPoolPodsFitOnlyVirtualNodes checks the toleration and the node selector
// on the pods themselves, not only on the template.
func TestPoolPodsFitOnlyVirtualNodes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.declare(f.backend())

	wantSelector := map[string]string{
		"gitlab-runner.flintlock.dev/virtual-node": "true",
		"kubernetes.io/arch":                       "arm64",
		// A bare Host selector key is one of the project's Host labels, which
		// the Pod Provider copies to the Virtual Node (KF-013); a key with a
		// domain of its own is used as written.
		"gitlab-runner.flintlock.dev/disk": "nvme",
		"example.com/zone":                 "a",
	}
	wantToleration := corev1.Toleration{
		Key:      "gitlab-runner.flintlock.dev/microvm",
		Operator: corev1.TolerationOpEqual,
		Value:    "true",
		Effect:   corev1.TaintEffectNoSchedule,
	}
	for _, pod := range f.pods() {
		if len(pod.Spec.NodeSelector) != len(wantSelector) {
			t.Errorf("pod %s node selector = %v, want %v", pod.Name, pod.Spec.NodeSelector, wantSelector)
		}
		for key, want := range wantSelector {
			if got := pod.Spec.NodeSelector[key]; got != want {
				t.Errorf("pod %s node selector %s = %q, want %q", pod.Name, key, got, want)
			}
		}
		tolerated := false
		for _, tol := range pod.Spec.Tolerations {
			if tol.MatchToleration(&wantToleration) {
				tolerated = true
			}
		}
		if !tolerated {
			t.Errorf("pod %s tolerations = %+v, want one for the virtual node taint", pod.Name, pod.Spec.Tolerations)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Scheduler SHALL give every Pool pod a topology spread
//# constraint over Virtual Nodes, so that a Pool's warm MicroVMs are spread
//# across Hosts.

// TestPoolPodsSpreadOverVirtualNodes checks the constraint: one per Pool,
// keyed on the label that has one value per Host and counting the Pool's idle
// pods.
func TestPoolPodsSpreadOverVirtualNodes(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.declare(f.backend())

	for _, pod := range f.pods() {
		if len(pod.Spec.TopologySpreadConstraints) != 1 {
			t.Fatalf("pod %s has %d topology spread constraints, want 1", pod.Name, len(pod.Spec.TopologySpreadConstraints))
		}
		c := pod.Spec.TopologySpreadConstraints[0]
		if c.TopologyKey != "gitlab-runner.flintlock.dev/host-node" || c.MaxSkew != 1 {
			t.Errorf("pod %s spreads over %q with skew %d, want the host-node label with skew 1", pod.Name, c.TopologyKey, c.MaxSkew)
		}
		if c.LabelSelector == nil || c.LabelSelector.MatchLabels[kube.LabelState] != kube.StateIdle ||
			c.LabelSelector.MatchLabels[kube.LabelProfile] != f.profile.Name {
			t.Errorf("pod %s spread selector = %+v, want the pool's idle pods", pod.Name, c.LabelSelector)
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# The Scheduler SHALL label every Pool pod with the Runner name,
//# the Profile name and `gitlab-runner.flintlock.dev/state` set to `idle`,
//# and SHALL select the ReplicaSet's pods by all three.

// TestPoolPodsCarryTheThreeLabels checks the labels on the pods and that the
// selector is exactly those three.
func TestPoolPodsCarryTheThreeLabels(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.declare(f.backend())

	want := map[string]string{
		"gitlab-runner.flintlock.dev/runner":  runnerName,
		"gitlab-runner.flintlock.dev/profile": f.profile.Name,
		"gitlab-runner.flintlock.dev/state":   "idle",
	}
	selector := f.replicaSet().Spec.Selector
	if len(selector.MatchExpressions) != 0 || len(selector.MatchLabels) != len(want) {
		t.Errorf("selector = %+v, want exactly %v", selector, want)
	}
	pods := f.pods()
	if len(pods) != 2 {
		t.Fatalf("%d pods, want 2", len(pods))
	}
	for key, value := range want {
		if selector.MatchLabels[key] != value {
			t.Errorf("selector %s = %q, want %q", key, selector.MatchLabels[key], value)
		}
		for _, pod := range pods {
			if pod.Labels[key] != value {
				t.Errorf("pod %s label %s = %q, want %q", pod.Name, key, pod.Labels[key], value)
			}
		}
	}
}

//= docs/requirements/12-cluster-fleet.md#kube-pools
//= type=test
//# When a Profile is removed by a configuration reload, the
//# Scheduler SHALL NOT delete its ReplicaSet and SHALL log that the Pool is
//# no longer referenced.

// TestRemovedProfileKeepsItsReplicaSet reloads the backend with no Profile
// left and checks that the ReplicaSet and its pods stay exactly as they
// were, through rollout passes as well, and that the removal is logged.
func TestRemovedProfileKeepsItsReplicaSet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	b := f.backend()
	f.declare(b)
	before := f.replicaSet()

	b.SetProfiles([]config.Profile{})

	f.consistently("the replicaset and its pods are left alone", consistentFor, func() bool {
		b.Rollout(f.ctx)
		rs := f.replicaSet()
		return rs.UID == before.UID && rs.ResourceVersion == before.ResourceVersion &&
			rs.DeletionTimestamp == nil && len(f.podsInState(kube.StateIdle)) == 2
	})
	if logs := f.logs.String(); !strings.Contains(logs, "no longer referenced") || !strings.Contains(logs, "profile="+f.profile.Name) {
		t.Errorf("the removal was not logged:\n%s", logs)
	}
}
