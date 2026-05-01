package domain

import "testing"

func TestCollectionOwnerOfRootSubtree(t *testing.T) {
	base := NewBase(64, 96)
	c := NewCollection("coll", base.Root(), base)
	owner, ok := c.OwnerOf("a")
	if !ok || owner != base.Root() {
		t.Fatalf("OwnerOf(a) = %q %v want @ true", owner, ok)
	}
}
