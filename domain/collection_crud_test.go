package domain

import "testing"

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
