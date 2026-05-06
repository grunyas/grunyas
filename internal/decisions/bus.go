package decisions

import (
	"sync"
	"sync/atomic"
	"time"
)

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

	droppedOTelOverflow atomic.Int64
	droppedSubOverflow  atomic.Int64
}

func NewBus(maxSubscribers, perSubscriberBuffer int) *Bus {
	return &Bus{
		subscribers: make(map[uint64]*sub),
		maxSubs:     maxSubscribers,
		bufSize:     perSubscriberBuffer,
	}
}

func (b *Bus) Publish(event interface{}) {
	now := time.Now().UTC()
	switch e := event.(type) {
	case Event:
		e.EventID = NewULID()
		e.Timestamp = now
		e.SchemaVersion = SchemaVersion
		event = e
	case TransitionEvent:
		e.EventID = NewULID()
		e.Timestamp = now
		e.SchemaVersion = SchemaVersion
		event = e
	}

	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
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
