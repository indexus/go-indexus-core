package domain

import "testing"

func TestCollectionAddIdempotentSameLocationID(t *testing.T) {
	base := NewBase(64, 96)
	root := base.Root()
	c := NewCollection("coll", root, base)

	const delegation = 1 << 20
	metrics := []float64{1, 2, 3}

	a1 := c.Add("aa", "same-id", metrics, delegation)
	if a1 == nil {
		t.Fatal("first Add returned nil")
	}
	a2 := c.Add("aa", "same-id", metrics, delegation)
	if a2 == nil {
		t.Fatal("second Add returned nil")
	}
	if len(a2) != 0 {
		t.Fatalf("duplicate Add should return empty ownership map, got %d keys", len(a2))
	}

	rootSet, ok := c.Get(root)
	if !ok {
		t.Fatal("missing root set")
	}
	if rootSet.Abelian().Count() != 1 {
		t.Fatalf("root aggregate count=%d want 1 (no double-count)", rootSet.Abelian().Count())
	}
}
