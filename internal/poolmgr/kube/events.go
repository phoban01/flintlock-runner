package kube

import (
	"context"
	"fmt"
	"sync"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

// subscriberBuffer is how many events a subscriber may fall behind before its
// stream is dropped. A dropped stream is not a lost count: the Tracker polls
// GetPool and subscribes again (PL-051).
const subscriberBuffer = 1024

// podState is where the watch last saw a pod, in the terms the Tracker
// counts in.
type podState int

const (
	// podGone is a pod that is not in its Pool's counts: deleted, finished,
	// or an idle pod of a previous template on its way out.
	podGone podState = iota
	// podProvisioning is an idle pod of the current template that is not
	// ready yet.
	podProvisioning
	// podAvailable is a pod a claim could take (KF-044).
	podAvailable
	// podLeased is a claimed pod.
	podLeased
)

//= docs/requirements/12-cluster-fleet.md#kube-pools
//# The Scheduler SHALL derive each Pool's available count from a
//# watch on the Pool's ready idle pods and SHALL wake Jobs waiting on an
//# exhausted Pool when that count rises.

// hub turns the watch on the Runner's pods into the poolmgr.v1alpha1 events
// the PoolTracker already counts from, so the available count and the
// wake-up of a Job waiting on an exhausted Pool (SC-021) need no second
// mechanism: a pod that becomes ready, idle and current is VM_AVAILABLE,
// which is the event the Tracker raises the count on and closes its waiters
// for. It remembers the state each pod was last reported in and publishes
// only transitions, so the informer's periodic resync and the status updates
// of a pod that stays as it is produce nothing.
type hub struct {
	b *Backend

	mu     sync.Mutex
	nextID int64
	states map[types.UID]trackedPod
	subs   map[*stream]struct{}
	closed bool
}

// trackedPod is the last reported state of a pod and the Pool it was
// reported under, which a deletion needs after the ReplicaSet is gone.
type trackedPod struct {
	state podState
	pool  poolmgr.PoolRef
}

func newHub(b *Backend) *hub {
	return &hub{b: b, states: map[types.UID]trackedPod{}, subs: map[*stream]struct{}{}}
}

// podHandler feeds pod changes to the hub.
func (h *hub) podHandler() cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { h.observe(podOf(obj), false) },
		UpdateFunc: func(_, obj any) { h.observe(podOf(obj), false) },
		DeleteFunc: func(obj any) { h.observe(podOf(obj), true) },
	}
}

// replicaSetHandler re-evaluates a Pool's pods when its ReplicaSet appears
// or changes: a new template makes every pod of the previous one
// unclaimable without any of those pods changing.
func (h *hub) replicaSetHandler() cache.ResourceEventHandler {
	reobserve := func(obj any) {
		rs, ok := obj.(*appsv1.ReplicaSet)
		if !ok {
			return
		}
		pods, err := h.b.pods.List(labels.SelectorFromSet(poolLabelsOf(rs)))
		if err != nil {
			return
		}
		for _, pod := range pods {
			h.observe(pod, false)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    reobserve,
		UpdateFunc: func(_, obj any) { reobserve(obj) },
	}
}

// podOf unwraps the object of an informer notification, which for a deletion
// the watch missed is a tombstone around the last known pod.
func podOf(obj any) *corev1.Pod {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	pod, _ := obj.(*corev1.Pod)
	return pod
}

// observe records a pod's state and publishes the event of the transition.
func (h *hub) observe(pod *corev1.Pod, deleted bool) {
	if pod == nil {
		return
	}
	rs := h.replicaSetFor(pod)

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	previous, known := h.states[pod.UID]
	next := trackedPod{state: podGone, pool: previous.pool}
	if rs != nil {
		next.pool = poolSpecFromReplicaSet(rs).Ref
		if !deleted {
			next.state = stateOf(rs, pod)
		}
	} else if !deleted && known && pod.Labels[LabelState] == StateClaimed && !terminal(pod) {
		// A Lease outlives its ReplicaSet (KF-051).
		next.state = podLeased
	}
	if next.state == podGone {
		delete(h.states, pod.UID)
	} else {
		h.states[pod.UID] = next
	}
	if next.pool == (poolmgr.PoolRef{}) || next.state == previous.state {
		return
	}
	h.publishLocked(next.pool, string(pod.UID), eventType(previous.state, next.state, pod))
}

// stateOf classifies a live pod of a Pool.
func stateOf(rs *appsv1.ReplicaSet, pod *corev1.Pod) podState {
	switch {
	case terminal(pod):
		return podGone
	case pod.Labels[LabelState] == StateClaimed:
		return podLeased
	case claimable(rs, pod):
		return podAvailable
	case pod.Labels[LabelState] == StateIdle && pod.Labels[LabelTemplateHash] == currentHash(rs):
		return podProvisioning
	default:
		return podGone
	}
}

// eventType names a transition in battery's vocabulary. A pod that leaves
// the available set for any reason other than a claim, because it was
// deleted, stopped being ready or belongs to a previous template, is
// reported as deleted, which is the event that takes it out of every count;
// a pod that comes back is simply available again.
func eventType(from, to podState, pod *corev1.Pod) poolmgr.EventType {
	switch to {
	case podAvailable:
		return poolmgrv1.EventType_VM_AVAILABLE
	case podLeased:
		return poolmgrv1.EventType_VM_CLAIMED
	case podProvisioning:
		if from == podAvailable {
			return poolmgrv1.EventType_VM_DELETED_ON_RELEASE
		}
		return poolmgrv1.EventType_VM_PROVISIONED
	default:
		if from == podLeased && pod.Status.Reason == "DeadlineExceeded" {
			return poolmgrv1.EventType_VM_DELETED_DUE_TO_EXPIRY
		}
		return poolmgrv1.EventType_VM_DELETED_ON_RELEASE
	}
}

// replicaSetFor finds the ReplicaSet of a pod's Pool in the watch cache.
func (h *hub) replicaSetFor(pod *corev1.Pod) *appsv1.ReplicaSet {
	sets, err := h.b.replicaSets.List(labels.SelectorFromSet(labels.Set{
		LabelRunner:  pod.Labels[LabelRunner],
		LabelProfile: pod.Labels[LabelProfile],
	}))
	if err != nil || len(sets) == 0 {
		return nil
	}
	return sets[0]
}

// publishLocked sends one event to every subscriber that wants it. A
// subscriber that has fallen a whole buffer behind is dropped instead of
// being waited for, because this runs on the informer's goroutine.
func (h *hub) publishLocked(pool poolmgr.PoolRef, uid string, typ poolmgr.EventType) {
	h.nextID++
	event := poolmgr.Event{ID: h.nextID, Pool: pool, VMUID: uid, Type: typ, At: h.b.opts.Clock.Now()}
	for s := range h.subs {
		if s.filter != nil && *s.filter != pool {
			continue
		}
		select {
		case s.events <- event:
		default:
			h.b.log.Warn("event subscriber fell behind, dropping its stream", "buffered", len(s.events))
			delete(h.subs, s)
			close(s.dropped)
		}
	}
}

// Subscribe implements poolmgr.Events. A new stream is first replayed the
// pods that are available now, as battery replays recent events to a new
// subscriber, so that a Tracker that subscribes after the pods became ready
// still counts them; the Tracker counts by uid, so a replay of a pod it
// already knows changes nothing.
func (b *Backend) Subscribe(ctx context.Context, filter poolmgr.EventFilter) (poolmgr.EventStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("kube: subscribe: %w", err)
	}
	h := b.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, fmt.Errorf("kube: subscribe: %w: the backend is closed", poolmgr.ErrUnavailable)
	}
	s := &stream{hub: h, events: make(chan poolmgr.Event, subscriberBuffer), dropped: make(chan struct{})}
	if filter.Pool != nil {
		ref := *filter.Pool
		s.filter = &ref
	}
	for uid, tracked := range h.states {
		if tracked.state != podAvailable || (s.filter != nil && *s.filter != tracked.pool) {
			continue
		}
		h.nextID++
		select {
		case s.events <- poolmgr.Event{
			ID: h.nextID, Pool: tracked.pool, VMUID: string(uid),
			Type: poolmgrv1.EventType_VM_AVAILABLE, At: b.opts.Clock.Now(),
		}:
		default:
			// More available pods than the buffer holds: the Tracker's
			// poll on Track counts what the replay could not carry.
		}
	}
	h.subs[s] = struct{}{}
	return s, nil
}

// close ends every stream.
func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for s := range h.subs {
		delete(h.subs, s)
		close(s.dropped)
	}
}

// stream is one subscription.
type stream struct {
	hub     *hub
	filter  *poolmgr.PoolRef
	events  chan poolmgr.Event
	dropped chan struct{}
}

// Recv implements poolmgr.EventStream. Events already buffered are delivered
// before the end of a dropped stream is reported.
func (s *stream) Recv(ctx context.Context) (*poolmgr.Event, error) {
	select {
	case event := <-s.events:
		return &event, nil
	default:
	}
	select {
	case event := <-s.events:
		return &event, nil
	case <-s.dropped:
		return nil, fmt.Errorf("kube: event stream ended: %w", poolmgr.ErrUnavailable)
	case <-ctx.Done():
		return nil, fmt.Errorf("kube: event stream: %w", ctx.Err())
	}
}

// Close implements poolmgr.EventStream.
func (s *stream) Close() error {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	if _, open := s.hub.subs[s]; open {
		delete(s.hub.subs, s)
		close(s.dropped)
	}
	return nil
}
