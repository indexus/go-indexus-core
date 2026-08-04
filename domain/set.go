package domain

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

type Set struct {
	list map[string]*Abelian
	// agg is the running sum of list entries; nil means empty.
	agg  *Abelian
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

// MarshalJSON takes a shallow snapshot under the lock then encodes unlocked
// so /set responses do not hold Set.mu across json.Marshal.
func (s *Set) MarshalJSON() ([]byte, error) {
	if s == nil {
		return []byte("null"), nil
	}
	s.mu.Lock()
	snap := make(map[string]*Abelian, len(s.list))
	for k, v := range s.list {
		snap[k] = v
	}
	s.mu.Unlock()
	return json.Marshal(snap)
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

func (s *Set) Decr(value string, abelian *Abelian) *Abelian {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.decr(value, abelian)
}

func (s *Set) Delete(value string) (*Abelian, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.delete(value)
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

// Shrink splits oversized leaf sets. Optional leaves[0] is an entry→setKey
// index updated as leaves migrate (nil is fine for standalone use).
func (s *Set) Shrink(base Encoder, sets map[string]*Set, key string, max int, leaves ...map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var leafIdx map[string]string
	if len(leaves) > 0 {
		leafIdx = leaves[0]
	}
	s.shrink(base, sets, key, max, leafIdx)
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

func (s *Set) addToAgg(abelian *Abelian) {
	if abelian == nil {
		return
	}
	if s.agg == nil {
		s.agg = abelian.Clone()
		return
	}
	s.agg.Sum(abelian)
}

func (s *Set) subFromAgg(abelian *Abelian) {
	if abelian == nil || s.agg == nil {
		return
	}
	s.agg.Substract(abelian)
}

func (s *Set) add(value string, abelian *Abelian, max int) bool {
	if old, ok := s.list[value]; ok {
		s.subFromAgg(old)
	}
	s.list[value] = abelian
	s.addToAgg(abelian)
	return len(s.list) > max
}

func (s *Set) put(value string, abelian *Abelian) {
	if old, ok := s.list[value]; ok {
		s.subFromAgg(old)
	}
	s.list[value] = abelian
	s.addToAgg(abelian)
}

func (s *Set) count() int {
	if s.agg == nil {
		return 0
	}
	return s.agg.Count()
}

func (s *Set) abelian() *Abelian {
	if s.agg == nil {
		return NewAbelian(0, nil)
	}
	return s.agg.Clone()
}

// abelianScan recomputes from scratch (used after bulk list replacement).
func (s *Set) abelianScan() *Abelian {
	var count int
	var metrics []float64

	for _, elm := range s.list {
		count += elm.Count()

		// Entries do not all carry the same metrics: a counter raised by incr
		// starts with none, while an item arrives with its own. Sizing the
		// total on whichever entry the map hands over first drops the rest of
		// a wider one — or walks off the end of a narrower one.
		values := elm.Metrics()
		if len(values) > len(metrics) {
			widened := make([]float64, len(values))
			copy(widened, metrics)
			metrics = widened
		}
		for idx, value := range values {
			metrics[idx] += value
		}
	}

	return NewAbelian(count, metrics)
}

func (s *Set) incr(value string, delta *Abelian) *Abelian {
	if _, ok := s.list[value]; !ok {
		s.list[value] = NewAbelian(0, nil)
	}
	s.list[value].Sum(delta)
	s.addToAgg(delta)
	return s.list[value]
}

func (s *Set) decr(value string, delta *Abelian) *Abelian {
	if _, ok := s.list[value]; !ok {
		s.list[value] = NewAbelian(0, nil)
	}
	s.list[value].Substract(delta)
	s.subFromAgg(delta)
	return s.list[value]
}

func (s *Set) delete(value string) (*Abelian, bool) {
	abelian, ok := s.list[value]
	if !ok {
		return nil, false
	}
	s.subFromAgg(abelian)
	delete(s.list, value)
	return abelian, true
}

func (s *Set) shrink(base Encoder, sets map[string]*Set, key string, max int, leaves map[string]string) {

	type tmp struct {
		key     string
		abelian *Abelian
	}
	list := make(map[string]*Abelian)
	exist := make(map[string]tmp)
	moved := make(map[string]any)

	precision := len(key)
	if key == base.Root() {
		precision = 0
	}

	for current, abelian := range s.list {

		if abelian.Count() > 1 {
			list[current] = abelian
			continue
		}

		// Entries are "location:id"; only the location part is splittable.
		location := current
		if idx := strings.IndexByte(current, ':'); idx >= 0 {
			location = current[:idx]
		}

		if len(location) <= precision {
			list[current] = abelian
			if leaves != nil {
				leaves[current] = key
			}
			continue
		}
		child := location[:precision+1]

		if set, ok := sets[child]; ok {
			if set.add(current, abelian, max) {
				set.shrink(base, sets, child, max, leaves)
			} else if leaves != nil {
				leaves[current] = child
			}
			moved[child] = nil
		} else if first, ok := exist[child]; ok {
			set := NewSet()
			set.add(first.key, first.abelian, max)
			overflow := set.add(current, abelian, max)
			if leaves != nil {
				leaves[first.key] = child
				leaves[current] = child
			}
			sets[child] = set
			moved[child] = nil
			delete(exist, child)
			if overflow {
				set.shrink(base, sets, child, max, leaves)
			}
		} else {
			exist[child] = tmp{
				key:     current,
				abelian: abelian,
			}
		}
	}

	for _, elm := range exist {
		list[elm.key] = elm.abelian
		if leaves != nil && elm.abelian.Count() == 1 {
			leaves[elm.key] = key
		}
	}

	// Last word: an aggregate copied above would otherwise be stale.
	for child := range moved {
		list[child] = sets[child].abelian()
	}

	s.list = list
	if len(list) == 0 {
		s.agg = nil
	} else {
		s.agg = s.abelianScan()
	}
}
