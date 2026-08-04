package domain

import (
	"fmt"
	"testing"
)

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
