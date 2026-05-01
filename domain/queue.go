package domain

import (
	"sync"
)

type Queue[T any] struct {
	cursor int
	mu     sync.Mutex
	cond   *sync.Cond
	data   []T
}

func NewQueue[T any]() *Queue[T] {
	q := &Queue[T]{
		data: make([]T, 0),
	}
	// cond holds a *Locker pointing into q.mu; *Queue must never be copied.
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *Queue[T]) Add(e T) {
	q.mu.Lock()
	defer q.mu.Unlock()

	q.data = append(q.data, e)
	q.cond.Signal()
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
