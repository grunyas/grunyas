package decisions

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/grunyas/grunyas/internal/policy"
)

const ringCapacity = 1024

type DroppedEvent struct {
	Count int64  `json:"count"`
	Since string `json:"since"`
}

type sub struct {
	ch    chan interface{}
	drops atomic.Int64
}

type Bus struct {
	mu          sync.RWMutex
	subscribers map[uint64]*sub
	nextSubID   uint64
	closed      bool
	maxSubs     int
	bufSize     int

	ring     []interface{}
	ringPos  int
	ringFull bool

	droppedOTelOverflow atomic.Int64
	droppedBusOverflow  atomic.Int64
	droppedSubOverflow  atomic.Int64
}

func NewBus(maxSubscribers, perSubscriberBuffer int) *Bus {
	return &Bus{
		subscribers: make(map[uint64]*sub),
		maxSubs:     maxSubscribers,
		bufSize:     perSubscriberBuffer,
		ring:        make([]interface{}, ringCapacity),
	}
}

func (b *Bus) writeRing(event interface{}) {
	if b.ringFull {
		b.droppedBusOverflow.Add(1)
	}
	b.ring[b.ringPos] = event
	b.ringPos++
	if b.ringPos == ringCapacity {
		b.ringPos = 0
		b.ringFull = true
	}
}

func (b *Bus) drainRing() []interface{} {
	if !b.ringFull && b.ringPos == 0 {
		return nil
	}
	count := b.ringPos
	if b.ringFull {
		count = ringCapacity
	}
	out := make([]interface{}, 0, count)
	if b.ringFull {
		out = append(out, b.ring[b.ringPos:]...)
	}
	out = append(out, b.ring[:b.ringPos]...)
	return out
}

func (b *Bus) Publish(event Event) {
	event.EventID = NewULID()
	event.Timestamp = time.Now().UTC()
	event.SchemaVersion = SchemaVersion

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.writeRing(event)
	b.mu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, s := range b.subscribers {
		select {
		case s.ch <- event:
		default:
			s.drops.Add(1)
			b.droppedSubOverflow.Add(1)
		}
	}
}

func (b *Bus) PublishTransition(t policy.Transition) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.writeRing(t)
	b.mu.Unlock()

	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, s := range b.subscribers {
		select {
		case s.ch <- t:
		default:
			s.drops.Add(1)
			b.droppedSubOverflow.Add(1)
		}
	}
}

type Subscription struct {
	Ch           <-chan interface{}
	Unsub        func()
	DrainDropped func() int64
}

func (b *Bus) Subscribe() (*Subscription, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return nil, false
	}
	if len(b.subscribers) >= b.maxSubs {
		return nil, false
	}

	id := b.nextSubID
	b.nextSubID++
	s := &sub{
		ch: make(chan interface{}, b.bufSize),
	}
	b.subscribers[id] = s

	// Seed new subscriber with a snapshot of the ring for initial context.
	for _, e := range b.drainRing() {
		select {
		case s.ch <- e:
		default:
			s.drops.Add(1)
			b.droppedSubOverflow.Add(1)
			break
		}
	}

	unsub := func() {
		b.mu.Lock()
		delete(b.subscribers, id)
		b.mu.Unlock()
	}

	drain := func() int64 {
		return s.drops.Swap(0)
	}

	return &Subscription{
		Ch:           s.ch,
		Unsub:        unsub,
		DrainDropped: drain,
	}, true
}

func (b *Bus) SubscriberCount() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subscribers)
}

func (b *Bus) DroppedOTelOverflow() int64 {
	return b.droppedOTelOverflow.Load()
}

func (b *Bus) DroppedBusOverflow() int64 {
	return b.droppedBusOverflow.Load()
}

func (b *Bus) DroppedSubOverflow() int64 {
	return b.droppedSubOverflow.Load()
}

func (b *Bus) MarkOTelDropped(n int64) {
	b.droppedOTelOverflow.Add(n)
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	for _, s := range b.subscribers {
		close(s.ch)
	}
	b.subscribers = make(map[uint64]*sub)
}
