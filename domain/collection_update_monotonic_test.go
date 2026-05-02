package domain

import "testing"

func TestCollectionUpdateMonotonicRejectsStaleLowerCount(t *testing.T) {
	base := NewBase(64, 96)
	root := base.Root()
	c := NewCollection("coll", root, base)

	c.Update("b", NewAbelian(10, []float64{1}))
	set, ok := c.Get(root)
	if !ok {
		t.Fatal("missing root set")
	}
	prev, ok := set.Get("b")
	if !ok || prev.Count() != 10 {
		t.Fatalf("after first Update: b count=%v ok=%v", prev, ok)
	}

	c.Update("b", NewAbelian(5, []float64{1}))
	after, ok := set.Get("b")
	if !ok || after.Count() != 10 {
		t.Fatalf("stale lower count must be ignored: got count=%v ok=%v want 10", after, ok)
	}

	c.Update("b", NewAbelian(15, []float64{2}))
	after2, ok := set.Get("b")
	if !ok || after2.Count() != 15 {
		t.Fatalf("after monotonic raise: got count=%v ok=%v want 15", after2, ok)
	}
}
