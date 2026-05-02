package domain

import (
	"sync"
	"time"
)

type Set struct {
	list map[string]*Abelian
	last time.Time
	mu   sync.Mutex
}

func NewSet() *Set {
	return &Set{
		list: make(map[string]*Abelian),
		last: time.Now(),
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

// AddIfAbsent inserts the entry only when value is not already present.
// Returns (added, full): added=false means a duplicate was detected and
// the caller MUST NOT propagate aggregate Incrs to parent levels (doing
// so would double-count this item — the exact failure mode that
// inflated aggregates whenever a /item forward timed out at the origin
// while actually succeeding at the receiver, then was retried). full
// mirrors the standard Add contract so the caller can still trigger a
// shrink when the deepest shard reaches its capacity boundary.
func (s *Set) AddIfAbsent(value string, abelian *Abelian, max int) (added bool, full bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.list[value]; ok && existing != nil {
		return false, len(s.list) > max
	}
	s.list[value] = abelian
	return true, len(s.list) > max
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

// SetList replaces the set's internal list with the provided map.
// Used exclusively during cold restore from a snapshot header so that
// the owner-level aggregates are available without loading item data.
func (s *Set) SetList(list map[string]*Abelian) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.list = list
	s.last = time.Now()
}

func (s *Set) Expired(expiration time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return time.Since(s.last) > expiration
}

func (s *Set) Shrink(base Encoder, sets map[string]*Set, key string, max int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.shrink(base, sets, key, max)
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
	metricLen := 0
	for _, elm := range s.list {
		if elm == nil {
			continue
		}
		if em := elm.Metrics(); len(em) > metricLen {
			metricLen = len(em)
		}
	}
	metrics := make([]float64, metricLen)
	var count int
	for _, elm := range s.list {
		if elm == nil {
			continue
		}
		count += elm.Count()
		for idx, value := range elm.Metrics() {
			metrics[idx] += value
		}
	}
	return NewAbelian(count, metrics)
}

func (s *Set) incr(value string, delta *Abelian) *Abelian {
	if delta == nil {
		return s.list[value]
	}
	current, ok := s.list[value]
	if !ok || current == nil {
		s.list[value] = delta.Clone()
		return s.list[value]
	}
	current.Sum(delta)
	return current
}

func (s *Set) shrink(base Encoder, sets map[string]*Set, key string, max int) {

	type tmp struct {
		key     string
		abelian *Abelian
	}
	list := make(map[string]*Abelian)
	exist := make(map[string]tmp)

	child, precision := "", len(key)
	if key == base.Root() {
		precision = 0
	}

	for current, abelian := range s.list {
		if abelian == nil {
			continue
		}

		// Defensive guard: old/corrupted snapshots can contain unexpected keys
		// that are not deeper than the current parent precision.
		if len(current) <= precision {
			list[current] = abelian
			continue
		}

		if abelian.Count() > 1 {
			list[current] = abelian
			continue
		}

		child = current[:precision+1]

		if set, ok1 := sets[child]; ok1 {
			// Restore/shrink can encounter an already materialized child set
			// before this loop has initialized list[child] for the new parent
			// view; avoid nil dereference and recompute aggregate from child set.
			if set == nil {
				set = NewSet()
				sets[child] = set
			}
			full := set.add(current, abelian, max)

			if full {
				set.shrink(base, sets, child, max)
			}
			list[child] = set.Abelian()
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
