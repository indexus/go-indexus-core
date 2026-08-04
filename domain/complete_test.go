package domain

import "testing"

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
