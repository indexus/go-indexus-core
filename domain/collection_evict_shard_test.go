package domain

import "testing"

func TestCollectionEvictShardRespectsImmediateOwner(t *testing.T) {
	base := NewBase(64, 96)
	root := base.Root()
	c := NewCollection("coll", root, base)

	c.mu.Lock()
	c.owned["1"] = Delegation{}
	c.owned["7"] = Delegation{}
	c.sets["1"] = NewSet()
	c.sets["1A"] = NewSet()
	c.sets["1B"] = NewSet()
	c.sets["7"] = NewSet()
	c.sets["7C"] = NewSet()
	// Leaf data whose immediate owner is @ (not a separate shard owner).
	c.sets["zz"] = NewSet()
	c.mu.Unlock()

	removed := c.EvictShard(root)
	if removed < 1 {
		t.Fatalf("expected EvictShard(@) to remove zz-only subtree, removed=%d", removed)
	}

	c.mu.Lock()
	_, hasZZ := c.sets["zz"]
	_, has1A := c.sets["1A"]
	_, has1B := c.sets["1B"]
	_, has7 := c.sets["7"]
	_, has7C := c.sets["7C"]
	c.mu.Unlock()
	if hasZZ {
		t.Fatal("EvictShard(@) should remove zz (immediate owner @)")
	}
	if !has1A || !has1B || !has7 || !has7C {
		t.Fatalf("EvictShard(@) must not wipe deeper-owned shards: 1A=%v 1B=%v 7=%v 7C=%v", has1A, has1B, has7, has7C)
	}

	n2 := c.EvictShard("1")
	if n2 < 2 {
		t.Fatalf("EvictShard(1) expected to remove 1A+1B sets, removed=%d", n2)
	}
	c.mu.Lock()
	_, still1A := c.sets["1A"]
	_, still1B := c.sets["1B"]
	c.mu.Unlock()
	if still1A || still1B {
		t.Fatalf("EvictShard(1) should remove 1A/1B: still1A=%v still1B=%v", still1A, still1B)
	}
}
