package kube_test

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

//= docs/requirements/12-cluster-fleet.md#cluster-least-privilege
//= type=test
//# The Runner SHALL operate with a Role in its own namespace
//# limited to managing ReplicaSets, reading, updating and deleting pods and
//# creating pod exec sessions, and with read access to Nodes.

// TestRunnerRole checks the requirement from both sides. The manifest grants
// nothing beyond what KF-110 lists, rule by rule. And the backend lives
// within it: like every backend in this package's tests, the one here runs as
// a user bound to that manifest and to nothing else, and it declares, claims,
// heartbeats, releases, rolls and tears down a Pool, so a call outside the
// Role would be refused by the API server and fail here. What the Role leaves
// out is refused to that same user.
func TestRunnerRole(t *testing.T) {
	t.Parallel()
	role, clusterRole := loadRoles(t)
	allowed := map[string][]string{
		"apps/replicasets": {"get", "list", "watch", "create", "update", "delete"},
		"/pods":            {"get", "list", "watch", "update", "patch", "delete"},
		"/pods/exec":       {"create", "get"},
	}
	checkRules(t, "Role", role.Rules, allowed)
	checkRules(t, "ClusterRole", clusterRole.Rules, map[string][]string{"/nodes": {"get", "list", "watch"}})

	f := newFixture(t)

	b := f.backend()
	ref := f.declare(b)
	claim, err := b.ClaimVM(f.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Heartbeat(f.ctx, claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseVM(f.ctx, claim.LeaseID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ListPools(f.ctx, ""); err != nil {
		t.Fatal(err)
	}
	b.Rollout(f.ctx)
	if err := b.DeletePool(f.ctx, ref); err != nil {
		t.Fatal(err)
	}

	runner := kubernetes.NewForConfigOrDie(f.runnerConfig())
	if _, err := runner.CoreV1().Nodes().List(f.ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("reading nodes: %v", err)
	}
	refused := map[string]error{
		"creating a pod": func() error {
			_, err := runner.CoreV1().Pods(f.namespace).Create(f.ctx, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "intruder"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "busybox"}}},
			}, metav1.CreateOptions{})
			return err
		}(),
		"reading secrets": func() error {
			_, err := runner.CoreV1().Secrets(f.namespace).List(f.ctx, metav1.ListOptions{})
			return err
		}(),
		"reading config maps": func() error {
			_, err := runner.CoreV1().ConfigMaps(f.namespace).List(f.ctx, metav1.ListOptions{})
			return err
		}(),
		"reading another namespace's pods": func() error {
			_, err := runner.CoreV1().Pods("default").List(f.ctx, metav1.ListOptions{})
			return err
		}(),
		"updating a node": func() error {
			_, err := runner.CoreV1().Nodes().Update(f.ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}}, metav1.UpdateOptions{})
			return err
		}(),
	}
	for what, err := range refused {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s = %v, want it forbidden", what, err)
		}
	}
}

// checkRules fails for any rule that grants a resource or a verb that is not
// allowed, and for a wildcard or a non-resource rule of any kind.
func checkRules(t *testing.T, kind string, rules []rbacv1.PolicyRule, allowed map[string][]string) {
	t.Helper()
	seen := map[string]bool{}
	for _, rule := range rules {
		if len(rule.NonResourceURLs) > 0 || len(rule.ResourceNames) > 0 {
			t.Errorf("%s rule %+v: unexpected non-resource URLs or resource names", kind, rule)
		}
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				key := group + "/" + resource
				verbs, ok := allowed[key]
				if !ok {
					t.Errorf("%s grants %q, which KF-110 does not list", kind, key)
					continue
				}
				seen[key] = true
				for _, verb := range rule.Verbs {
					if !slices.Contains(verbs, verb) {
						t.Errorf("%s grants %q on %q, want only %v", kind, verb, key, verbs)
					}
				}
			}
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("%s grants nothing on %q, which the Runner needs", kind, key)
		}
	}
}
