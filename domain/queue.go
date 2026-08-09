package domain

import (
	"sync"
)

// compactFloor keeps the copy out of the way of short queues, where the
// consumed prefix is never big enough to be worth reclaiming.
const compactFloor = 1024

type Queue[T any] struct {
	cursor int
	mu     *sync.Mutex
	cond   *sync.Cond
	data   []T
}

func NewQueue[T any]() *Queue[T] {
	mu := &sync.Mutex{}
	return &Queue[T]{
		mu:   mu,
		cond: sync.NewCond(mu),
		data: make([]T, 0),
	}
}

func (q *Queue[T]) Add(e T) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.data = append(q.data, e)
	q.cond.Signal()
}

// TryAdd enqueues only if pending length < max. max<=0 means unbounded.
func (q *Queue[T]) TryAdd(e T, max int) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	pending := len(q.data) - q.cursor
	if max > 0 && pending >= max {
		return false
	}
	q.data = append(q.data, e)
	q.cond.Signal()
	return true
}

// TryConsume takes the next element, or reports false when none is pending.
// Unlike Consume it never blocks, so a caller holding a deadline can drain
// whatever is there and move on.
func (q *Queue[T]) TryConsume() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.cursor >= len(q.data) {
		var none T
		return none, false
	}

	result := q.data[q.cursor]
	q.cursor++
	q.compact()

	return result, true
}

func (q *Queue[T]) Consume() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.cursor >= len(q.data) {
		q.cursor, q.data = 0, make([]T, 0)
		q.cond.Wait()
	}

	result := q.data[q.cursor]
	q.cursor++
	q.compact()

	return result, true
}

// compact drops the consumed prefix. The reset in Consume only fires on an
// empty queue, so a queue that stays busy — the only kind that matters here —
// used to keep every element it had ever held alive in the backing array.
func (q *Queue[T]) compact() {
	if q.cursor < compactFloor || q.cursor*2 < len(q.data) {
		return
	}

	kept := copy(q.data, q.data[q.cursor:])

	var none T
	for i := kept; i < len(q.data); i++ {
		q.data[i] = none
	}

	q.data = q.data[:kept]
	q.cursor = 0
}

func (q *Queue[T]) Length() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.data) - q.cursor
}

// Snapshot copies the pending elements without consuming them. A durability
// checkpoint needs the backlog that has been ACKed but not yet applied, so the
// rotated log does not drop work the client already saw succeed.
func (q *Queue[T]) Snapshot() []T {
	q.mu.Lock()
	defer q.mu.Unlock()

	pending := len(q.data) - q.cursor
	if pending <= 0 {
		return nil
	}
	out := make([]T, pending)
	copy(out, q.data[q.cursor:])
	return out
}
