package proxy

import (
	"container/heap"
	"sync"
	"time"

	"github.com/grunyas/grunyas/internal/server/types"
)

type idleSweeper struct {
	mu      sync.Mutex
	timeout time.Duration
	heap    idleMinHeap
	entries map[types.Expirable]*idleEntry
}

type idleEntry struct {
	sess     types.Expirable
	deadline time.Time
	index    int
}

type idleMinHeap []*idleEntry

func newIdleSweeper(timeout time.Duration) *idleSweeper {
	return &idleSweeper{
		timeout: timeout,
		entries: make(map[types.Expirable]*idleEntry),
	}
}

func (is *idleSweeper) Track(sess types.Expirable) bool {
	if is.timeout <= 0 {
		return false
	}

	is.mu.Lock()
	defer is.mu.Unlock()

	deadline := time.Now().Add(is.timeout)
	entry := &idleEntry{
		sess:     sess,
		deadline: deadline,
	}

	heap.Push(&is.heap, entry)
	is.entries[sess] = entry

	return true
}

func (is *idleSweeper) Untrack(sess types.Expirable) {
	if is.timeout <= 0 {
		return
	}

	is.mu.Lock()
	defer is.mu.Unlock()

	if entry, ok := is.entries[sess]; ok {
		heap.Remove(&is.heap, entry.index)
		delete(is.entries, sess)
	}
}

func (is *idleSweeper) Expire() []types.Expirable {
	if is.timeout <= 0 {
		return nil
	}

	return is._expire(time.Now())
}

func (is *idleSweeper) _expire(now time.Time) []types.Expirable {
	is.mu.Lock()
	defer is.mu.Unlock()

	var expired []types.Expirable

	for len(is.heap) > 0 {
		entry := is.heap[0]

		// If the stored deadline is in the future, nothing else is expired
		// (min-heap property).
		if entry.deadline.After(now) {
			break
		}

		// The entry seems expired based on old deadline.
		// Check if it has been active recently.
		lastActive := entry.sess.LastActive()
		newDeadline := lastActive.Add(is.timeout)

		if newDeadline.After(now) {
			// It was active! Update deadline and fix heap.
			entry.deadline = newDeadline
			heap.Fix(&is.heap, 0)
		} else {
			// Really expired.
			heap.Pop(&is.heap)
			delete(is.entries, entry.sess)
			expired = append(expired, entry.sess)
		}
	}

	return expired
}

func (h idleMinHeap) Len() int { return len(h) }

func (h idleMinHeap) Less(i, j int) bool {
	return h[i].deadline.Before(h[j].deadline)
}

func (h idleMinHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}

func (h *idleMinHeap) Push(x any) {
	entry := x.(*idleEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}

func (h *idleMinHeap) Pop() any {
	old := *h
	n := len(old)
	entry := old[n-1]
	old[n-1] = nil
	entry.index = -1
	*h = old[:n-1]

	return entry
}
