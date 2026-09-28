package claimtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"
)

// tick is how often the fake reconciles.
const tick = 20 * time.Millisecond

// Host is a fake Host the fake battery places MicroVMs on: the name of its
// Node and the address of its Exec Agent.
type Host struct {
	NodeName     string
	AgentAddress string
}

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//# The claim backend SHALL be tested against a fake battery that
//# serves the `Pool` and `MicroVMClaim` resources

//= docs/requirements/12-cluster-fleet.md#claim-test-doubles
//# and binds claims from warm
//# MicroVMs on the fake Hosts.

// Battery stands in for battery-operator's Pool and Claim Controllers and
// for battery behind them, in one namespace of the test API server. It
// keeps each Pool at its size with warm MicroVMs, placed on the fake Host
// with the fewest, and reports the counts in the Pool's status. It binds a
// claim from a warm MicroVM, or leaves it Pending with the reason
// PoolExhausted when the Pool has none, or PoolNotFound when there is no
// Pool. It extends a Bound claim's Lease each time the Holder renews it, and
// expires a claim whose Lease lapses. A claimed MicroVM is deleted when its
// claim is deleted or expires, and a new warm one takes its place, as
// battery's replenishment does.
//
// It adds no finalizer to a claim, so a deleted claim is gone at once. It
// runs no hook and never quarantines.
type Battery struct {
	c         client.Client
	namespace string
	hosts     []Host

	mu      sync.Mutex
	pools   map[string]*fakePool
	held    map[types.UID]heldVM
	expire  map[string]bool
	nextID  int
	onHost  map[int]int
	pending map[string]string
}

// fakePool is one Pool's MicroVMs.
type fakePool struct {
	size   int32
	warm   int32
	leased int32
}

// heldVM is the MicroVM a Bound claim holds.
type heldVM struct {
	pool string
	host int
}

// NewBattery returns a fake battery for the namespace, with the Hosts to
// place MicroVMs on. Run starts it.
func NewBattery(c client.Client, namespace string, hosts ...Host) *Battery {
	if len(hosts) == 0 {
		hosts = []Host{{NodeName: "host-1", AgentAddress: "10.0.0.1:9443"}}
	}
	return &Battery{
		c:         c,
		namespace: namespace,
		hosts:     hosts,
		pools:     map[string]*fakePool{},
		held:      map[types.UID]heldVM{},
		expire:    map[string]bool{},
		onHost:    map[int]int{},
		pending:   map[string]string{},
	}
}

// Run reconciles until ctx ends.
func (f *Battery) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		f.reconcile(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Expire makes the named claim's Lease lapse on the next reconcile, as if
// the Holder had stopped renewing it.
func (f *Battery) Expire(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expire[name] = true
}

// Pending reports the reasons the fake has given for leaving claims
// Pending, by claim name, so that a test can see a claim that is gone now.
func (f *Battery) Pending() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.pending))
	for k, v := range f.pending {
		out[k] = v
	}
	return out
}

func (f *Battery) reconcile(ctx context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pools := &batteryv1alpha1.PoolList{}
	if err := f.c.List(ctx, pools, client.InNamespace(f.namespace)); err != nil {
		return
	}
	claims := &batteryv1alpha1.MicroVMClaimList{}
	if err := f.c.List(ctx, claims, client.InNamespace(f.namespace)); err != nil {
		return
	}
	f.sizePools(pools.Items)

	live := map[types.UID]bool{}
	for i := range claims.Items {
		cl := &claims.Items[i]
		live[cl.UID] = true
		f.reconcileClaim(ctx, cl)
	}
	for uid, vm := range f.held {
		if !live[uid] {
			f.replace(uid, vm)
		}
	}
	f.writePools(ctx, pools.Items)
}

// sizePools brings each Pool's warm MicroVMs to its size, at once.
func (f *Battery) sizePools(pools []batteryv1alpha1.Pool) {
	seen := map[string]bool{}
	for i := range pools {
		p := &pools[i]
		seen[p.Name] = true
		fp, ok := f.pools[p.Name]
		if !ok {
			fp = &fakePool{}
			f.pools[p.Name] = fp
		}
		fp.size = p.Spec.Size
		fp.warm = max(fp.size-fp.leased, 0)
	}
	for name := range f.pools {
		if !seen[name] {
			delete(f.pools, name)
		}
	}
}

func (f *Battery) reconcileClaim(ctx context.Context, cl *batteryv1alpha1.MicroVMClaim) {
	if cl.DeletionTimestamp != nil {
		return
	}
	switch cl.Status.Phase {
	case "", batteryv1alpha1.MicroVMClaimPending:
		fp, ok := f.pools[cl.Spec.PoolRef.Name]
		switch {
		case !ok:
			f.setPending(ctx, cl, batteryv1alpha1.ReasonPoolNotFound, "the pool does not exist")
		case fp.warm <= 0:
			f.setPending(ctx, cl, batteryv1alpha1.ReasonPoolExhausted, "the pool has no available microvm")
		default:
			f.bind(ctx, cl, fp)
		}
	case batteryv1alpha1.MicroVMClaimBound:
		f.renew(ctx, cl)
	}
}

func (f *Battery) setPending(ctx context.Context, cl *batteryv1alpha1.MicroVMClaim, reason, message string) {
	if cl.Status.Phase == batteryv1alpha1.MicroVMClaimPending {
		if c := meta.FindStatusCondition(cl.Status.Conditions, batteryv1alpha1.ConditionBound); c != nil && c.Reason == reason {
			return
		}
	}
	cl.Status.Phase = batteryv1alpha1.MicroVMClaimPending
	meta.SetStatusCondition(&cl.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionFalse, Reason: reason, Message: message,
	})
	if f.c.Status().Update(ctx, cl) == nil {
		f.pending[cl.Name] = reason
	}
}

// bind takes a warm MicroVM from the Pool for the claim, on the Host with
// the fewest MicroVMs.
func (f *Battery) bind(ctx context.Context, cl *batteryv1alpha1.MicroVMClaim, fp *fakePool) {
	host := 0
	for i := range f.hosts {
		if f.onHost[i] < f.onHost[host] {
			host = i
		}
	}
	f.nextID++
	now := metav1.Now()
	cl.Status.Phase = batteryv1alpha1.MicroVMClaimBound
	cl.Status.LeaseID = fmt.Sprintf("lease-%d", f.nextID)
	cl.Status.MicroVM = &batteryv1alpha1.MicroVMReference{UID: fmt.Sprintf("microvm-%d", f.nextID)}
	cl.Status.Host = &batteryv1alpha1.HostReference{
		NodeName: f.hosts[host].NodeName, AgentAddress: f.hosts[host].AgentAddress,
	}
	cl.Status.BoundTime = &now
	cl.Status.ObservedRenewTime = cl.Spec.RenewTime
	cl.Status.LeaseExpiresAt = &metav1.Time{Time: now.Add(f.expiry(cl))}
	meta.SetStatusCondition(&cl.Status.Conditions, metav1.Condition{
		Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionTrue,
		Reason: batteryv1alpha1.ReasonBound, Message: "the claim holds a lease",
	})
	if err := f.c.Status().Update(ctx, cl); err != nil {
		return
	}
	fp.warm--
	fp.leased++
	f.onHost[host]++
	f.held[cl.UID] = heldVM{pool: cl.Spec.PoolRef.Name, host: host}
}

// renew extends a Bound claim's Lease when the Holder has renewed it, and
// expires it when its Lease has lapsed or a test asked for it.
func (f *Battery) renew(ctx context.Context, cl *batteryv1alpha1.MicroVMClaim) {
	lapsed := cl.Status.LeaseExpiresAt != nil && time.Now().After(cl.Status.LeaseExpiresAt.Time)
	if f.expire[cl.Name] || lapsed {
		cl.Status.Phase = batteryv1alpha1.MicroVMClaimExpired
		meta.SetStatusCondition(&cl.Status.Conditions, metav1.Condition{
			Type: batteryv1alpha1.ConditionBound, Status: metav1.ConditionFalse,
			Reason: batteryv1alpha1.ReasonLeaseExpired, Message: "the lease lapsed",
		})
		if f.c.Status().Update(ctx, cl) != nil {
			return
		}
		delete(f.expire, cl.Name)
		if vm, ok := f.held[cl.UID]; ok {
			f.replace(cl.UID, vm)
		}
		return
	}
	renewed := cl.Spec.RenewTime
	if renewed == nil || (cl.Status.ObservedRenewTime != nil && renewed.Equal(cl.Status.ObservedRenewTime)) {
		return
	}
	cl.Status.ObservedRenewTime = renewed
	cl.Status.LeaseExpiresAt = &metav1.Time{Time: renewed.Add(f.expiry(cl))}
	_ = f.c.Status().Update(ctx, cl)
}

// expiry is the expiry threshold of the claim's Pool.
func (f *Battery) expiry(cl *batteryv1alpha1.MicroVMClaim) time.Duration {
	pool := &batteryv1alpha1.Pool{}
	key := client.ObjectKey{Namespace: f.namespace, Name: cl.Spec.PoolRef.Name}
	if err := f.c.Get(context.Background(), key, pool); err == nil {
		if d := pool.Spec.Lease.ExpiryThreshold; d != nil && d.Duration > 0 {
			return d.Duration
		}
	}
	return 30 * time.Second
}

// replace deletes a claimed MicroVM and puts a warm one in its place.
func (f *Battery) replace(uid types.UID, vm heldVM) {
	delete(f.held, uid)
	f.onHost[vm.host]--
	if fp, ok := f.pools[vm.pool]; ok {
		fp.leased--
		fp.warm = max(fp.size-fp.leased, 0)
	}
}

// writePools reports each Pool's counts in its status.
func (f *Battery) writePools(ctx context.Context, pools []batteryv1alpha1.Pool) {
	for i := range pools {
		p := &pools[i]
		fp, ok := f.pools[p.Name]
		if !ok {
			continue
		}
		want := batteryv1alpha1.PoolStatus{
			ObservedGeneration: p.Generation,
			Available:          fp.warm,
			Leased:             fp.leased,
			Conditions:         p.Status.Conditions,
		}
		if p.Status.ObservedGeneration == want.ObservedGeneration &&
			p.Status.Available == want.Available && p.Status.Leased == want.Leased {
			continue
		}
		p.Status = want
		_ = f.c.Status().Update(ctx, p)
	}
}
