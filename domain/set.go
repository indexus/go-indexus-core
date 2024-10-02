package domain

import (
	"sync"
	"time"
)

type Set struct {
	list map[string]int
	last time.Time
	mu   *sync.Mutex
}

func NewSet() *Set {
	return &Set{
		list: make(map[string]int),
		last: time.Now(),
		mu:   &sync.Mutex{},
	}
}

func (s *Set) List() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make(map[string]int)
	for key, value := range s.list {
		result[key] = value
	}
	return result
}

func (s *Set) Traverse(process func(string, int)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.traverse(process)
}

func (s *Set) Get(value string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.get(value)
}

func (s *Set) Add(value string, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.add(value, max)
}

func (s *Set) Put(value string, count int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.put(value, count)
}

func (s *Set) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.count()
}

func (s *Set) Incr(value string, n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.incr(value, n)
}

func (s *Set) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.last = time.Now()
}

func (s *Set) Expired(expiration time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return time.Since(s.last) > expiration
}

func (s *Set) Shrink(sets map[string]*Set, key string, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.shrink(sets, key, max)
}

func (s *Set) traverse(process func(string, int)) {
	for key, value := range s.list {
		process(key, value)
	}
}

func (s *Set) get(value string) (int, bool) {
	count, ok := s.list[value]
	return count, ok
}

func (s *Set) add(value string, max int) bool {
	s.list[value] = 1
	return len(s.list) > max
}

func (s *Set) put(value string, count int) {
	s.list[value] = count
}

func (s *Set) count() int {
	count := 0
	for _, elm := range s.list {
		count += elm
	}
	return count
}

func (s *Set) incr(value string, n int) int {
	result := s.list[value] + n
	s.list[value] = result
	return result
}

func (s *Set) shrink(sets map[string]*Set, key string, max int) {

	list := make(map[string]int)
	exist := make(map[string]string)

	child, precision := "", len(key)
	if key == root {
		precision = 0
	}

	for current, count := range s.list {

		if count > 1 {
			list[current] = count
			continue
		}

		child = current[:precision+1]

		if set, ok1 := sets[child]; ok1 {
			full := set.add(current, max)
			list[child] = list[child] + 1

			if full {
				set.shrink(sets, child, max)
			}
		} else if first, ok2 := exist[child]; ok2 {
			set := NewSet()
			set.add(first, max)
			set.add(current, max)

			sets[child] = set
			list[child] = 2
			delete(exist, child)
		} else {
			exist[child] = current
		}
	}

	for _, elm := range exist {
		list[elm] = 1
	}

	s.list = list
}
