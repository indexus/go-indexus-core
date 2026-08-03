package domain

import (
	"sync"
)

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

func (q *Queue[T]) Consume() (T, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for q.cursor >= len(q.data) {
		q.cursor, q.data = 0, make([]T, 0)
		q.cond.Wait()
	}

	result := q.data[q.cursor]
	q.cursor++

	return result, true
}

func (q *Queue[T]) Length() int {
	q.mu.Lock()
	defer q.mu.Unlock()

	return len(q.data) - q.cursor
}
