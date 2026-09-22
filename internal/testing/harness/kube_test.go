package harness

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/phoban01/flintlock-runner/internal/config"
	"github.com/phoban01/flintlock-runner/internal/kubelabels"
)

// These tests exercise the Kubernetes stack without the Runner binary. They
// need the envtest binaries and skip without them, unless
// FLINTLOCK_RUNNER_REQUIRE_ENVTEST is set.

func startTestCluster(t *testing.T) *Cluster {
	t.Helper()
	c, err := StartCluster()
	if errors.Is(err, ErrNoCluster) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop() })
	return c
}

//= docs/requirements/10-test-doubles.md#end-to-end-harness
//= type=test
//# The harness SHALL fail if any scenario leaves a Lease held or
//# a sandbox directory behind after the Runner has shut down.

// TestKubeStack starts a Kubernetes stack, checks the configuration it
// writes for the Runner, then leaves a claimed pod behind and checks that
// Shutdown reports it as a held Lease and still removes its MicroVM.
func TestKubeStack(t *testing.T) {
	c := startTestCluster(t)
	s := start(t, Options{Backend: BackendKubernetes, Cluster: c, Hosts: 2})
	ctx := context.Background()

	cfg := s.Config
	if !cfg.PoolManager.IsKubernetes() || len(cfg.Inventory.Hosts) != 0 || cfg.PoolManager.Kubernetes == nil ||
		cfg.PoolManager.Kubernetes.Namespace != s.KubeNamespace() {
		t.Errorf("pool_manager = %+v, inventory %d hosts; want the kubernetes backend in %s and no inventory",
			cfg.PoolManager, len(cfg.Inventory.Hosts), s.KubeNamespace())
	}
	if kind := cfg.Profiles[0].Transport.Kind; kind != config.TransportKubeExec {
		t.Errorf("transport %s, want kube-exec", kind)
	}
	kubeconfig, err := clientcmd.LoadFromFile(cfg.PoolManager.Kubernetes.Kubeconfig)
	if err != nil {
		t.Fatalf("the runner's kubeconfig: %v", err)
	}
	if info, err := os.Stat(cfg.PoolManager.Kubernetes.Kubeconfig); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode %v (%v), want 600: it holds a private key", info.Mode().Perm(), err)
	}
	if kubeconfig.CurrentContext == "" {
		t.Error("the runner's kubeconfig has no current context")
	}

	hosts := s.KubeHosts()
	if len(hosts) != 2 || len(s.Hosts) != 2 {
		t.Fatalf("%d kube hosts, %d fake hosts; want 2 of each", len(hosts), len(s.Hosts))
	}
	admin := c.Admin()
	for _, h := range hosts {
		n, err := admin.CoreV1().Nodes().Get(ctx, h.VirtualNode, metav1.GetOptions{})
		if err != nil || !nodeReady(n) {
			t.Errorf("virtual node %s: %v, want it registered and ready", h.VirtualNode, err)
		}
	}

	// A claimed pod nobody releases: the scheduler stand-in binds it and
	// the Pod Provider makes it a MicroVM.
	noToken := false
	_, err = admin.CoreV1().Pods(s.KubeNamespace()).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "left-behind",
			Labels: map[string]string{kubelabels.LabelState: kubelabels.StateClaimed},
			Annotations: map[string]string{
				kubelabels.AnnotationKernelImage: fakeKernelImage,
				kubelabels.AnnotationLease:       kubelabels.FormatLease(time.Now()),
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: &noToken,
			NodeSelector:                 map[string]string{kubelabels.LabelVirtualNode: "true"},
			Tolerations:                  []corev1.Toleration{{Key: kubelabels.TaintMicroVM, Operator: corev1.TolerationOpExists}},
			Containers: []corev1.Container{{
				Name: "microvm", Image: fakeRootFSImage,
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi"),
				}},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = poll(ctx, time.Minute, func() bool {
		left, err := s.LeaseHosts(ctx)
		if err != nil || len(left) != 1 {
			return false
		}
		p, err := admin.CoreV1().Pods(s.KubeNamespace()).Get(ctx, "left-behind", metav1.GetOptions{})
		if err != nil || p.Status.Phase != corev1.PodRunning {
			return false
		}
		sandboxes, err := s.Host(left[0]).Sandboxes()
		return err == nil && len(sandboxes) == 1
	})
	if err != nil {
		t.Fatalf("the claimed pod never became a microvm: %v\n%s", err, s.DescribeCluster(ctx))
	}

	err = s.Shutdown(ctx)
	if err == nil || !strings.Contains(err.Error(), "left-behind") {
		t.Errorf("Shutdown with a claimed pod left: %v, want it reported as a held lease", err)
	}
	if err != nil && strings.Contains(err.Error(), "sandbox") {
		t.Errorf("Shutdown left the claimed pod's microvm behind: %v", err)
	}
}
