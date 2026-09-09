package fake

import (
	"encoding/json"
	"sync"

	poolmgrv1 "github.com/liquidmetal-dev/battery/api/proto/poolmgr/v1alpha1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// subscriberBuffer is how many events a subscriber may fall behind before
// its stream is dropped and it has to re-subscribe and replay.
const subscriberBuffer = 256

//= docs/requirements/10-test-doubles.md#fake-pool-manager
//# The fake Pool Manager SHALL emit every `Event` type defined in the proto
//# at the corresponding phase transition.

// emitLocked appends an Event for a Pool (TD-006) and delivers it to every
// subscriber. Ids are monotonic per Pool, as the proto says; payload is
// rendered to payload_json so that the Runner can read counts from it
// (PL-055). It requires p.mu because it assigns the Pool's next id.
//
// The transitions and their events are: reservation created on a Host,
// VM_PROVISIONED; create hooks done, VM_AVAILABLE; Lease granted,
// VM_CLAIMED; ReleaseVM accepted, VM_RELEASED; the deletion that follows,
// VM_DELETED_ON_RELEASE; a Lease within one warning window of expiry,
// VM_EXPIRING_SOON; a Lease expired and its MicroVM deleted,
// VM_DELETED_DUE_TO_EXPIRY; a create or pre-lease hook failed, VM_HOOK_FAILED;
// provisioning started, POOL_REPLENISHING; a tick found a Pool short of its
// target, POOL_SIZE_BELOW_TARGET.
func (p *PoolManager) emitLocked(key poolKey, vmUID string, typ poolmgrv1.EventType, payload map[string]any) {
	ps, ok := p.pools[key]
	if !ok {
		return
	}
	ps.nextEvent++
	var body string
	if len(payload) > 0 {
		if b, err := json.Marshal(payload); err == nil {
			body = string(b)
		}
	}
	p.events.publish(key, &poolmgrv1.Event{
		Id:            ps.nextEvent,
		PoolName:      key.name,
		PoolNamespace: key.namespace,
		VmUid:         vmUID,
		Type:          typ,
		CreatedAt:     timestamppb.New(p.cfg.Clock.Now()),
		PayloadJson:   body,
	})
}

// eventBus fans events out to Subscribe streams and keeps the last replay
// events of every Pool for new subscribers.
type eventBus struct {
	replay int

	mu      sync.Mutex
	recent  map[poolKey][]*poolmgrv1.Event
	subs    map[int64]*subscriber
	nextSub int64
}

func newEventBus(replay int) *eventBus {
	return &eventBus{replay: replay, recent: make(map[poolKey][]*poolmgrv1.Event), subs: make(map[int64]*subscriber)}
}

// subscriber is one open Subscribe stream.
type subscriber struct {
	id     int64
	filter *poolKey
	ch     chan *poolmgrv1.Event
	// dropped is closed when the fake ends the stream: on the injected drop
	// (TD-010) or because the subscriber fell too far behind.
	dropped chan struct{}
	once    sync.Once
}

func (s *subscriber) matches(key poolKey) bool { return s.filter == nil || *s.filter == key }

func (s *subscriber) drop() { s.once.Do(func() { close(s.dropped) }) }

// publish records e and delivers it to matching subscribers without
// blocking; a subscriber whose buffer is full is dropped.
func (b *eventBus) publish(key poolKey, e *poolmgrv1.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	log := b.recent[key]
	log = append(log, e)
	if len(log) > b.replay {
		log = log[len(log)-b.replay:]
	}
	b.recent[key] = log
	for _, s := range b.subs {
		if !s.matches(key) {
			continue
		}
		select {
		case s.ch <- e:
		default:
			s.drop()
		}
	}
}

// subscribe registers a subscriber and returns the events to replay to it
// before live delivery: the recent events of the filtered Pool, or of every
// Pool, in id order per Pool.
func (b *eventBus) subscribe(filter *poolKey) (*subscriber, []*poolmgrv1.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSub++
	s := &subscriber{id: b.nextSub, filter: filter, ch: make(chan *poolmgrv1.Event, subscriberBuffer), dropped: make(chan struct{})}
	b.subs[s.id] = s
	var backlog []*poolmgrv1.Event
	if filter != nil {
		backlog = append(backlog, b.recent[*filter]...)
	} else {
		for _, log := range b.recent {
			backlog = append(backlog, log...)
		}
	}
	return s, backlog
}

func (b *eventBus) unsubscribe(id int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, id)
}

// dropAll ends every open stream once (Faults.DropEventsStream).
func (b *eventBus) dropAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		s.drop()
	}
}

// open reports how many streams are subscribed.
func (b *eventBus) open() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
