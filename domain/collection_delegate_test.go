package domain

import "testing"

// Regression: cluster restore replays delegation lines before shard replay
// may create owned edges without a materialized *Set at that location.
func TestDelegateWithoutSetDoesNotPanic(t *testing.T) {
	base := NewBase(64, 96)
	c := NewCollection("coll", base.Root(), base)

	child := string(base.CharAt(0)) // one-char child under root
	if child == "" {
		t.Fatal("base alphabet empty")
	}

	d := Delegation{}
	d[child] = nil
	c.Own(base.Root(), d)

	if _, ok := c.sets[child]; ok {
		t.Fatal("expected no *Set for delegated child before items")
	}

	items, _ := c.Delegate(child)
	if len(items) != 0 {
		t.Fatalf("expected no items, got %d", len(items))
	}
}
