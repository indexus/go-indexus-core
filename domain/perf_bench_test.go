package domain

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// findLeafScan is the pre-index O(#sets) implementation, kept for benchmarks.
func (c *Collection) findLeafScan(entry string) (setKey string, abelian *Abelian, ok bool) {
	for key, set := range c.sets {
		if ab, found := set.Get(entry); found && ab.Count() == 1 {
			return key, ab, true
		}
	}
	return "", nil, false
}

func benchCollection(sets int, leavesPerSet int) *Collection {
	base := fakeBase{length: 4}
	c := NewCollection("coll", "@", base)
	// Force many sibling sets under root so findLeafScan has work to do.
	for i := 0; i < sets; i++ {
		key := fmt.Sprintf("@%c", 'a'+rune(i%26))
		if i >= 26 {
			key = fmt.Sprintf("@%c%c", 'a'+rune(i/26), 'a'+rune(i%26))
		}
		c.sets[key] = NewSet()
		for j := 0; j < leavesPerSet; j++ {
			entry := fmt.Sprintf("%s:id-%d", key, j)
			c.sets[key].Put(entry, NewAbelian(1, []float64{1}))
			c.leaves[entry] = key
		}
	}
	return c
}

func BenchmarkFindLeafScan(b *testing.B) {
	c := benchCollection(64, 8)
	entry := "@a:id-0"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.mu.Lock()
		_, _, ok := c.findLeafScan(entry)
		c.mu.Unlock()
		if !ok {
			b.Fatal("missing")
		}
	}
}

func BenchmarkFindLeafIndexed(b *testing.B) {
	c := benchCollection(64, 8)
	entry := "@a:id-0"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.mu.Lock()
		_, _, ok := c.findLeaf(entry)
		c.mu.Unlock()
		if !ok {
			b.Fatal("missing")
		}
	}
}

func BenchmarkCollectionAddRemove(b *testing.B) {
	base := fakeBase{length: 4}
	c := NewCollection("coll", "@", base)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loc := fmt.Sprintf("a%c", 'a'+rune(i%26))
		id := fmt.Sprintf("%d", i)
		c.Add(loc, id, []float64{1}, 1000)
		c.Remove(loc, id)
	}
}

func BenchmarkSetAbelian(b *testing.B) {
	s := NewSet()
	for i := 0; i < 256; i++ {
		s.Put(fmt.Sprintf("k-%d", i), NewAbelian(1, []float64{float64(i), 1, 2, 3}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.Abelian()
		_ = s.Count()
	}
}

func BenchmarkCacheEvict(b *testing.B) {
	c := NewCache()
	c.SetMax(1000)
	mk := func(k string) *Set {
		s := NewSet()
		s.Put(k, NewAbelian(1, []float64{1}))
		return s
	}
	// Fill to capacity.
	for i := 0; i < 1000; i++ {
		c.SetHops("coll", fmt.Sprintf("loc-%d", i), mk("x"), 0)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.SetHops("coll", fmt.Sprintf("new-%d", i), mk("y"), 0)
	}
}

// legacyEvictLocked mirrors the old O(n²) scan eviction for comparison.
func (c *Cache) legacyEvictLocked() {
	if c.maxEntries <= 0 {
		return
	}
	for {
		total := 0
		var oldestColl, oldestLoc string
		var oldest time.Time
		first := true
		for coll, sets := range c.collections {
			for loc, entry := range sets {
				total++
				t := time.Time{}
				if entry != nil {
					t = entry.last
				}
				if first || t.Before(oldest) {
					first = false
					oldest = t
					oldestColl, oldestLoc = coll, loc
				}
			}
		}
		if total <= c.maxEntries {
			return
		}
		if entry, ok := c.collections[oldestColl][oldestLoc]; ok {
			c.removeLocked(oldestColl, oldestLoc, entry)
		} else {
			delete(c.collections[oldestColl], oldestLoc)
		}
	}
}

func BenchmarkCacheEvictLegacy(b *testing.B) {
	mk := func(k string) *Set {
		s := NewSet()
		s.Put(k, NewAbelian(1, []float64{1}))
		return s
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		c := NewCache()
		c.maxEntries = 500
		for j := 0; j < 500; j++ {
			entry := &cacheEntry{set: mk("x"), last: time.Now(), coll: "coll", loc: fmt.Sprintf("loc-%d", j)}
			entry.elem = c.lru.PushFront(entry)
			if _, ok := c.collections["coll"]; !ok {
				c.collections["coll"] = make(map[string]*cacheEntry)
			}
			c.collections["coll"][entry.loc] = entry
			c.total++
		}
		// Over-fill without using LRU path, then legacy-evict.
		entry := &cacheEntry{set: mk("y"), last: time.Now(), coll: "coll", loc: "overflow"}
		entry.elem = c.lru.PushFront(entry)
		c.collections["coll"][entry.loc] = entry
		c.total++
		b.StartTimer()
		c.mu.Lock()
		c.legacyEvictLocked()
		c.mu.Unlock()
	}
}

func BenchmarkBSTTraverse(b *testing.B) {
	bst := NewBST[int]()
	id := make([]byte, 12)
	for i := 0; i < 256; i++ {
		id[0] = byte(i)
		bst.Insert(0, append([]byte(nil), id...), i)
	}
	buf := make([]byte, 12)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		bst.Traverse(0, buf, func(int, []byte, int) { n++ })
		if n == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkBSTTraverseLegacy(b *testing.B) {
	bst := NewBST[int]()
	id := make([]byte, 12)
	for i := 0; i < 256; i++ {
		id[0] = byte(i)
		bst.Insert(0, append([]byte(nil), id...), i)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		n := 0
		buf := make([]byte, 12)
		bst.mu.Lock()
		legacyTraverse(bst.tree, 0, buf, func(int, []byte, int) { n++ })
		bst.mu.Unlock()
		if n == 0 {
			b.Fatal("empty")
		}
	}
}

func legacyTraverse[N any](t *tree[N], idx int, value []byte, process func(int, []byte, N)) {
	if t.left != nil {
		tmp := make([]byte, len(value))
		copy(tmp, value)
		legacyTraverse(t.left, idx+1, tmp, process)
	}
	if t.right != nil {
		tmp := make([]byte, len(value))
		copy(tmp, value)
		tmp[idx/8] |= 1 << (7 - uint(idx%8))
		legacyTraverse(t.right, idx+1, tmp, process)
	}
	if idx > 0 && t.left == nil && t.right == nil {
		process(idx, value, t.node)
	}
}

func BenchmarkSetMarshalJSON(b *testing.B) {
	s := NewSet()
	for i := 0; i < 256; i++ {
		s.Put(fmt.Sprintf("k-%d", i), NewAbelian(1, []float64{float64(i), 1, 2, 3}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetMarshalViaList(b *testing.B) {
	s := NewSet()
	for i := 0; i < 256; i++ {
		s.Put(fmt.Sprintf("k-%d", i), NewAbelian(1, []float64{float64(i), 1, 2, 3}))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(s.List()); err != nil {
			b.Fatal(err)
		}
	}
}
