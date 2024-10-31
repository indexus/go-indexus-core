package domain

import (
	"sync"
	"time"
)

type Set struct {
	list map[string]*Abelian
	last time.Time
	mu   *sync.Mutex
}

func NewSet() *Set {
	return &Set{
		list: make(map[string]*Abelian),
		last: time.Now(),
		mu:   &sync.Mutex{},
	}
}

func (s *Set) List() map[string]*Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	result := make(map[string]*Abelian)
	for key, value := range s.list {
		result[key] = value
	}
	return result
}

func (s *Set) Traverse(process func(string, *Abelian)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.traverse(process)
}

func (s *Set) Get(value string) (*Abelian, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.get(value)
}

func (s *Set) Add(value string, abelian *Abelian, max int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.add(value, abelian, max)
}

func (s *Set) Put(value string, abelian *Abelian) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.put(value, abelian)
}

func (s *Set) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.count()
}

func (s *Set) Abelian() *Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.abelian()
}

func (s *Set) Incr(value string, abelian *Abelian) *Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.incr(value, abelian)
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

func (s *Set) traverse(process func(string, *Abelian)) {
	for key, value := range s.list {
		process(key, value)
	}
}

func (s *Set) get(value string) (*Abelian, bool) {
	abelian, ok := s.list[value]
	return abelian, ok
}

func (s *Set) add(value string, abelian *Abelian, max int) bool {
	s.list[value] = abelian
	return len(s.list) > max
}

func (s *Set) put(value string, abelian *Abelian) {
	s.list[value] = abelian
}

func (s *Set) count() int {
	count := 0
	for _, elm := range s.list {
		count += elm.Count()
	}
	return count
}

func (s *Set) abelian() *Abelian {
	var count int
	var metrics []float64

	for _, elm := range s.list {
		count += elm.Count()

		if metrics == nil {
			metrics = make([]float64, len(elm.Metrics()))
		}
		for idx, value := range elm.Metrics() {
			metrics[idx] += value
		}
	}

	return NewAbelian(count, metrics)
}

func (s *Set) incr(value string, delta *Abelian) *Abelian {
	s.list[value].Sum(delta)
	return s.list[value]
}

func (s *Set) shrink(sets map[string]*Set, key string, max int) {

	type tmp struct {
		key     string
		abelian *Abelian
	}
	list := make(map[string]*Abelian)
	exist := make(map[string]tmp)

	child, precision := "", len(key)
	if key == root {
		precision = 0
	}

	for current, abelian := range s.list {

		if abelian.Count() > 1 {
			list[current] = abelian
			continue
		}

		child = current[:precision+1]

		if set, ok1 := sets[child]; ok1 {
			full := set.add(current, abelian, max)
			list[child].Sum(abelian)

			if full {
				set.shrink(sets, child, max)
			}
		} else if first, ok2 := exist[child]; ok2 {
			set := NewSet()
			set.add(first.key, first.abelian, max)
			set.add(current, abelian, max)

			sets[child] = set
			list[child] = set.Abelian()
			delete(exist, child)
		} else {
			exist[child] = tmp{
				key:     current,
				abelian: abelian,
			}
		}
	}

	for _, elm := range exist {
		list[elm.key] = elm.abelian
	}

	s.list = list
}
