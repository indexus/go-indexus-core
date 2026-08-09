package domain

import (
	"fmt"
	"sync"
	"testing"
)

// fakeBase is a minimal Encoder: single characters address the hierarchy, so a
// location of length N sits N levels below the root.
type fakeBase struct {
	length int
}

func (fakeBase) Packing() int                    { return 6 }
func (fakeBase) Encode(data []byte) string       { return string(data) }
func (fakeBase) Decode(s string) ([]byte, error) { return []byte(s), nil }
func (fakeBase) Root() string                    { return "@" }
func (b fakeBase) Length() int                   { return b.length }
func (b fakeBase) CharAt(idx int) string {
	alphabet := "abcdefghijklmnopqrstuvwxyz0123456789"
	if idx < 0 || idx >= b.length || idx >= len(alphabet) {
		return ""
	}
	return string(alphabet[idx])
}
func (fakeBase) NewID() []byte               { return make([]byte, 16) }
func (fakeBase) RandomName() (string, error) { return "x", nil }
func (fakeBase) Parent(hash string) string {
	if hash == "@" {
		return ""
	}
	if len(hash) == 1 {
		return "@"
	}
	return hash[:len(hash)-1]
}

func TestCollectionConcurrentAddAndGetMultiple(t *testing.T) {
	const (
		writers    = 4
		readers    = 4
		ops        = 500
		delegation = 8
	)

	c := NewCollection("coll", "@", fakeBase{length: 4})

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				location := fmt.Sprintf("%c%c", 'a'+w, 'a'+(i%26))
				c.Add(location, fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, delegation)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			locations := []string{"@", "a", "b", "c", "aa", "ab"}
			properties := []func(*Abelian) int{
				func(a *Abelian) int { return a.Count() },
			}
			for i := 0; i < ops; i++ {
				_, _ = c.GetMultiple(locations, 4, properties)
				_, _ = c.Get("@")
				c.Browse(func(string) {}, func(_, _ string, _ Handoff) {})
			}
		}()
	}

	wg.Wait()
}

func TestCollectionConcurrentAddSameLocation(t *testing.T) {
	const (
		writers    = 8
		ops        = 250
		delegation = 8
	)

	c := NewCollection("coll", "@", fakeBase{length: 4})

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				c.Add("aa", fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, delegation)
			}
		}()
	}
	wg.Wait()
}

func TestCollectionAddIdempotent(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	if areas := c.Add("aa", "x", []float64{1, 2}, 64); areas == nil {
		t.Fatal("first add should succeed")
	}
	root, _ := c.Get("@")
	if got := root.Count(); got != 1 {
		t.Fatalf("count after add: got %d want 1", got)
	}
	if areas := c.Add("aa", "x", []float64{1, 2}, 64); areas == nil {
		t.Fatal("idempotent add should return empty ownership, not nil")
	}
	if got := root.Count(); got != 1 {
		t.Fatalf("count after idempotent add: got %d want 1", got)
	}
}

func TestCollectionAddReplaceMetrics(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.Add("aa", "x", []float64{1}, 64)
	c.Add("aa", "x", []float64{5}, 64)
	root, _ := c.Get("@")
	if got := root.Count(); got != 1 {
		t.Fatalf("count after replace: got %d want 1", got)
	}
	ab, ok := root.Get("aa:x")
	if !ok {
		t.Fatal("leaf missing")
	}
	if ab.Metrics()[0] != 5 {
		t.Fatalf("metrics: got %v want [5]", ab.Metrics())
	}
}

func TestCollectionRemoveAndReadd(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.Add("aa", "x", []float64{1}, 64)
	removed, _, ok := c.Remove("aa", "x")
	if !ok || removed == nil {
		t.Fatalf("remove: ok=%v removed=%v", ok, removed != nil)
	}
	root, _ := c.Get("@")
	if got := root.Count(); got != 0 {
		t.Fatalf("count after remove: got %d want 0", got)
	}
	if !c.IsTombstoned("aa", "x") {
		t.Fatal("expected tombstone")
	}
	// idempotent delete
	if _, _, ok := c.Remove("aa", "x"); !ok {
		t.Fatal("second remove should succeed as no-op")
	}
	// re-add allowed
	if areas := c.Add("aa", "x", []float64{2}, 64); areas == nil {
		t.Fatal("re-add after delete should succeed")
	}
	if c.IsTombstoned("aa", "x") {
		t.Fatal("tombstone should clear on re-add")
	}
	if got := root.Count(); got != 1 {
		t.Fatalf("count after re-add: got %d want 1", got)
	}
}

// Add opens an ownership area as soon as the parent's running count reaches the
// delegation size, while only Shrink ever materialises a set. Complete walked
// that area up to the root and read the set that was never created, which took
// the node down in the middle of a rebalance.
func TestCompleteHandlesOwnedZoneWithoutSet(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 4})

	root, exist := c.Get("@")
	if !exist {
		t.Fatal("precondition: collection has no root set")
	}
	root.Put("a", NewAbelian(40, []float64{40}))
	c.Own("a", Delegation{})

	areas := c.Complete("@")

	if _, owned := areas["@"]["a"]; !owned {
		t.Fatal("Complete did not report the zone under the root")
	}
	if _, exist := c.Get("a"); !exist {
		t.Fatal("Complete left the owned zone without a set")
	}

	// The items are still counted in the parent: recomputing the aggregate from
	// a set that has never held anything would drop them from every read above.
	held, counted := root.Get("a")
	if !counted {
		t.Fatal("Complete removed the zone from the root aggregate")
	}
	if held.Count() != 40 {
		t.Fatalf("Complete overwrote the parent count: %d want 40", held.Count())
	}
}

func TestLeafIndexSurvivesShrinkAndRemove(t *testing.T) {
	base := fakeBase{length: 64}
	c := NewCollection("demo", "@", base)
	const n, del, delegation = 120, 40, 25
	alpha := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

	for i := 0; i < n; i++ {
		loc := string([]byte{alpha[i%64], alpha[(i/64)%64]})
		if areas := c.Add(loc, fmt.Sprintf("id-%d", i), []float64{1}, delegation); areas == nil {
			t.Fatalf("add %d refused", i)
		}
	}
	root, _ := c.Get("@")
	if got := root.Count(); got != n {
		t.Fatalf("after add count=%d want %d sets=%d leaves=%d", got, n, len(c.List()), len(c.leaves))
	}

	miss := 0
	for i := 0; i < del; i++ {
		loc := string([]byte{alpha[i%64], alpha[(i/64)%64]})
		removed, _, ok := c.Remove(loc, fmt.Sprintf("id-%d", i))
		if !ok {
			t.Fatalf("remove %d not ok", i)
		}
		if removed == nil {
			miss++
		}
	}
	if miss != 0 {
		t.Fatalf("%d removes missed the leaf (tombstone-only)", miss)
	}
	if got := root.Count(); got != n-del {
		t.Fatalf("after remove count=%d want %d", got, n-del)
	}
}

func TestLeafIndexMatchesScanAfterShrink(t *testing.T) {
	base := fakeBase{length: 4}
	c := NewCollection("demo", "@", base)
	for i := 0; i < 40; i++ {
		loc := fmt.Sprintf("a%c", 'a'+rune(i%4))
		c.Add(loc, fmt.Sprintf("id-%d", i), []float64{1}, 3)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for entry, setKey := range c.leaves {
		ab, ok := c.sets[setKey].get(entry)
		if !ok || ab.Count() != 1 {
			t.Fatalf("index points to missing leaf %s -> %s", entry, setKey)
		}
		scanKey, _, ok := c.findLeafScan(entry)
		if !ok || scanKey != setKey {
			t.Fatalf("index/scan mismatch for %s: index=%s scan=%s ok=%v", entry, setKey, scanKey, ok)
		}
	}
	// Every real leaf must be indexed.
	for setKey, set := range c.sets {
		for entry, ab := range set.list {
			if ab.Count() != 1 {
				continue
			}
			if idx, ok := c.leaves[entry]; !ok || idx != setKey {
				t.Fatalf("leaf %s in %s not indexed (got %s ok=%v)", entry, setKey, idx, ok)
			}
		}
	}
}
