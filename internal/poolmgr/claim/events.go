package claim

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batteryv1alpha1 "github.com/phoban01/battery-operator/api/v1alpha1"

	"github.com/phoban01/flintlock-runner/internal/poolmgr"
)

const (
	// subscriberBuffer is how many events a subscriber may fall behind before
	// its stream is dropped. A dropped stream is not a lost count: the
	// Tracker polls GetPool and subscribes again (PL-051).
	subscriberBuffer = 1024
	// minRewatch and maxRewatch bound the wait before the watch is made
	// again after it failed.
	minRewatch = 100 * time.Millisecond
	maxRewatch = 10 * time.Second
)

// hub turns the watch on the Runner's Pools into the poolmgr.v1alpha1
// events the PoolTracker already counts from, so the available count and
// the wake-up of a Job waiting on an exhausted Pool (SC-021) need no second
// mechanism.
//
// battery-operator keeps its MicroVMs internal, and a Pool's status carries
// counts, not MicroVMs. So the hub names the available MicroVMs itself: a
// Pool with n available has the stand-in uids 1 to n. When the count rises
// from n to m, the hub reports the uids n+1 to m as VM_AVAILABLE, which is
// the event the Tracker raises its count on and closes its waiters for.
// When it falls, it reports the uids above the new count as
// VM_DELETED_ON_RELEASE, which takes a named MicroVM out of every count and
// nothing else. The Tracker counts by uid, so its count follows the Pool's
// status. The other counts, leased, provisioning and quarantined, reach the
// Tracker through GetPool when it polls.
type hub struct {
	b *Backend

	mu     sync.Mutex
	nextID int64
	// available is the available count last reported for each Pool, by
	// the name of its Pool resource, with the PoolRef it was reported
	// under.
	available map[string]reported
	subs      map[*stream]struct{}
	closed    bool
}

// reported is what the hub last said about a Pool, and whether the Pool
// reported Ready in its status when the hub last saw it (OB-031).
type reported struct {
	ref   poolmgr.PoolRef
	count int32
	ready bool
}

func newHub(b *Backend) *hub {
	return &hub{b: b, available: map[string]reported{}, subs: map[*stream]struct{}{}}
}

// standIn is the uid the hub gives the i-th available MicroVM of a Pool.
func standIn(ref poolmgr.PoolRef, i int32) string {
	return fmt.Sprintf("%s/available-%d", ref.Name, i)
}

// watch lists and watches the Runner's Pools until ctx ends. A watch that
// fails or ends is made again from a new list, after a wait that grows
// while it keeps failing.
func (h *hub) watch(ctx context.Context) {
	wait := minRewatch
	for {
		if h.watchOnce(ctx) {
			wait = minRewatch
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, maxRewatch)
	}
}

// watchOnce lists the Pools, reports what changed since the last list, and
// follows the watch from that list until it ends. It reports whether the
// list succeeded.
func (h *hub) watchOnce(ctx context.Context) bool {
	b := h.b
	opts := []client.ListOption{client.InNamespace(b.opts.Namespace), client.MatchingLabels{LabelRunner: b.opts.RunnerName}}
	list := &batteryv1alpha1.PoolList{}
	listCtx, cancel := b.call(ctx)
	err := b.kube.List(listCtx, list, opts...)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			b.log.Debug("listing pools failed, retrying", "error", err)
		}
		return false
	}
	h.resync(list.Items)

	w, err := b.kube.Watch(ctx, &batteryv1alpha1.PoolList{}, append(opts,
		&client.ListOptions{Raw: &metav1.ListOptions{ResourceVersion: list.ResourceVersion}})...)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Debug("watching pools failed, retrying", "error", err)
		}
		return true
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return true
		case ev, open := <-w.ResultChan():
			if !open {
				return true
			}
			pool, ok := ev.Object.(*batteryv1alpha1.Pool)
			switch {
			case ev.Type == watch.Error:
				return true
			case !ok:
			case ev.Type == watch.Deleted:
				h.gone(pool.Name)
			case ev.Type == watch.Added, ev.Type == watch.Modified:
				h.observe(pool)
			}
		}
	}
}

// resync reports the Pools of a new list, and the end of each Pool the list
// no longer has.
func (h *hub) resync(pools []batteryv1alpha1.Pool) {
	seen := make(map[string]bool, len(pools))
	for i := range pools {
		seen[pools[i].Name] = true
		h.observe(&pools[i])
	}
	h.mu.Lock()
	var missing []string
	for name := range h.available {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	h.mu.Unlock()
	for _, name := range missing {
		h.gone(name)
	}
}

// observe reports the change in a Pool's available count.
func (h *hub) observe(pool *batteryv1alpha1.Pool) {
	ref := refOf(pool, h.b.opts.RunnerNamespace)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.moveLocked(pool.Name, ref, pool.Status.Available)
	last := h.available[pool.Name]
	last.ready = meta.IsStatusConditionTrue(pool.Status.Conditions, batteryv1alpha1.PoolConditionReady)
	h.available[pool.Name] = last
}

// gone reports a deleted Pool as having no available MicroVM.
func (h *hub) gone(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	last, known := h.available[name]
	if h.closed || !known {
		return
	}
	h.moveLocked(name, last.ref, 0)
	delete(h.available, name)
}

// moveLocked publishes the events that take a Pool's available count from
// what was last reported to count.
func (h *hub) moveLocked(name string, ref poolmgr.PoolRef, count int32) {
	last := h.available[name]
	if last.ref != ref {
		// Declared again under another Runner namespace: what was said
		// under the old PoolRef is withdrawn first.
		for i := last.count; i > 0; i-- {
			h.publishLocked(last.ref, standIn(last.ref, i), poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
		}
		last = reported{ref: ref}
	}
	for i := last.count + 1; i <= count; i++ {
		h.publishLocked(ref, standIn(ref, i), poolmgrv1.EventType_VM_AVAILABLE)
	}
	for i := last.count; i > count; i-- {
		h.publishLocked(ref, standIn(ref, i), poolmgrv1.EventType_VM_DELETED_ON_RELEASE)
	}
	h.available[name] = reported{ref: ref, count: max(count, 0)}
}

// publishLocked sends one event to every subscriber that wants it. A
// subscriber that has fallen a whole buffer behind is dropped instead of
// being waited for, because this runs on the watch's goroutine.
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
// MicroVMs that are available now, as battery replays recent events to a
// new subscriber, so that a Tracker that subscribes after they became
// available still counts them; the Tracker counts by uid, so a replay of a
// MicroVM it already knows changes nothing.
func (b *Backend) Subscribe(ctx context.Context, filter poolmgr.EventFilter) (poolmgr.EventStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("claim: subscribe: %w", err)
	}
	h := b.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, fmt.Errorf("claim: subscribe: %w: the backend is closed", poolmgr.ErrUnavailable)
	}
	s := &stream{hub: h, events: make(chan poolmgr.Event, subscriberBuffer), dropped: make(chan struct{})}
	if filter.Pool != nil {
		ref := *filter.Pool
		s.filter = &ref
	}
	for _, last := range h.available {
		if s.filter != nil && *s.filter != last.ref {
			continue
		}
		for i := int32(1); i <= last.count; i++ {
			h.nextID++
			select {
			case s.events <- poolmgr.Event{
				ID: h.nextID, Pool: last.ref, VMUID: standIn(last.ref, i),
				Type: poolmgrv1.EventType_VM_AVAILABLE, At: b.opts.Clock.Now(),
			}:
			default:
				// More available MicroVMs than the buffer holds: the
				// Tracker's poll on Track counts what the replay could not
				// carry.
			}
		}
	}
	h.subs[s] = struct{}{}
	return s, nil
}

// PoolReadiness is one of the Runner's Pools and whether its status
// reports Ready.
type PoolReadiness struct {
	Pool  poolmgr.PoolRef
	Ready bool
}

//= docs/requirements/08-observability.md#health
//# except that where the claim backend is configured, at least one of the
//# Runner's Pools reporting Ready in its status takes the place of the
//# healthy Host.

// PoolsReady reports each of the Runner's Pools and whether battery-operator
// marks it Ready, sorted by Pool. It reads what the watch last saw, so a
// readiness probe makes no call to the API server. Before the first list of
// the watch it reports no Pool.
func (b *Backend) PoolsReady() []PoolReadiness {
	h := b.hub
	h.mu.Lock()
	out := make([]PoolReadiness, 0, len(h.available))
	for _, last := range h.available {
		out = append(out, PoolReadiness{Pool: last.ref, Ready: last.ready})
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Pool.String() < out[j].Pool.String() })
	return out
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
		return nil, fmt.Errorf("claim: event stream ended: %w", poolmgr.ErrUnavailable)
	case <-ctx.Done():
		return nil, fmt.Errorf("claim: event stream: %w", ctx.Err())
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
