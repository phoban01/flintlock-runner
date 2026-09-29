package claimtest_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	flintlocktypes "github.com/liquidmetal-dev/flintlock/api/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr/claim/claimtest"
)

// recordedVMs is a Host's flintlockd that records the MicroVMs it holds.
type recordedVMs struct {
	mu  sync.Mutex
	vms []string
}

func (r *recordedVMs) CreateMicroVM(_ context.Context, spec *flintlocktypes.MicroVMSpec) (*flintlocktypes.MicroVM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vms = append(r.vms, spec.GetUid())
	return &flintlocktypes.MicroVM{Spec: spec}, nil
}

func (r *recordedVMs) DeleteMicroVM(_ context.Context, uid string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.vms = slices.DeleteFunc(r.vms, func(v string) bool { return v == uid })
	return nil
}

func (r *recordedVMs) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.vms)
}

// TestExpireKeepingMicroVMKeepsItUntilTheClaimGoes checks the fault the
// end-to-end harness injects for a Lease that expires during a Job: the
// claim is Expired at once, and its MicroVM stays until the claim is
// deleted.
func TestExpireKeepingMicroVMKeepsItUntilTheClaimGoes(t *testing.T) {
	env, note, err := claimtest.Start()
	if err != nil {
		t.Fatal(err)
	}
	if env == nil {
		t.Skip(note)
	}
	t.Cleanup(func() { _ = env.Stop() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c := env.Client
	const ns = "expiry"
	rootfs := "ghcr.io/example/rootfs:1"
	for _, obj := range []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&batteryv1alpha1.Pool{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "small"},
			Spec: batteryv1alpha1.PoolSpec{
				Size: 1,
				Template: batteryv1alpha1.MicroVMTemplate{
					VCPU: 1, MemoryInMb: 1024,
					Kernel: batteryv1alpha1.Kernel{Image: "ghcr.io/example/kernel:6.1"},
					RootVolume: batteryv1alpha1.Volume{
						ID: "root", ContainerSource: &rootfs,
					},
					Interfaces: []batteryv1alpha1.NetworkInterface{{DeviceID: "eth0"}},
				},
			},
		},
		&batteryv1alpha1.MicroVMClaim{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "job-1"},
			Spec: batteryv1alpha1.MicroVMClaimSpec{
				PoolRef: batteryv1alpha1.PoolReference{Name: "small"}, ServiceAccountName: "holder",
			},
		},
	} {
		if err := c.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	vms := &recordedVMs{}
	battery := claimtest.NewBattery(c, ns, claimtest.Host{NodeName: "host-1", AgentAddress: "127.0.0.1:1", MicroVMs: vms})
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); battery.Run(runCtx) }()
	defer func() { stop(); <-done }()

	phase := func() batteryv1alpha1.MicroVMClaimPhase {
		cl := &batteryv1alpha1.MicroVMClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "job-1"}, cl); err != nil {
			return ""
		}
		return cl.Status.Phase
	}
	eventually(t, "the claim to bind", func() bool { return phase() == batteryv1alpha1.MicroVMClaimBound })
	if vms.count() != 1 {
		t.Fatalf("%d microvms after the claim bound, want 1", vms.count())
	}

	battery.ExpireKeepingMicroVM("job-1")
	eventually(t, "the claim to expire", func() bool { return phase() == batteryv1alpha1.MicroVMClaimExpired })
	// A few reconciles later the MicroVM is still there.
	time.Sleep(200 * time.Millisecond)
	if vms.count() != 1 {
		t.Fatalf("%d microvms once the claim expired, want the claimed one kept", vms.count())
	}

	if err := c.Delete(ctx, &batteryv1alpha1.MicroVMClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "job-1"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the microvm to go with its claim", func() bool { return vms.count() == 0 })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
