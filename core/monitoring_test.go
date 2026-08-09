package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// Parent holds delegated-child Abelian stubs (cache / SoT pointer); Items()
// must count residual leaves only. Status.zones still lists the parent shell.
func TestItemsExcludesDelegatedChildLeavesButParentStubRemains(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 4)
	const coll = "demo"
	n.create(coll, encoding.BASE64.Root())
	c, _ := n.collections.Get(coll)

	root := encoding.BASE64.Root()
	child := "a" // Parent(a)==@ under BASE64
	c.EnsureSet(child)
	// Parent owns @ with child delegated; child also owned locally (pre-handoff).
	c.Own(root, domain.Delegation{child: domain.Handoff{}})
	c.Own(child, domain.Delegation{})
	idRoot := zoneID(t, coll, root)
	idChild := zoneID(t, coll, child)
	n.owned.Upsert(0, idRoot, map[domain.Key]any{}, func(int, []byte, map[domain.Key]any) {
		// keep existing
	})
	n.owned.Upsert(0, idChild, map[domain.Key]any{domain.Key{Collection: coll, Location: child}: struct{}{}}, func(int, []byte, map[domain.Key]any) {})
	// Ensure BST has both keys with Key maps.
	n.owned.Upsert(0, idRoot, map[domain.Key]any{}, func(_ int, _ []byte, m map[domain.Key]any) {
		m[domain.Key{Collection: coll, Location: root}] = struct{}{}
	})

	rootSet, _ := c.Get(root)
	childSet, _ := c.Get(child)
	rootSet.Put(child, domain.NewAbelian(10, []float64{10})) // stub
	rootSet.Put(root+":residual", domain.NewAbelian(1, []float64{1}))
	childSet.Put(child+":x", domain.NewAbelian(1, []float64{1}))
	childSet.Put(child+":y", domain.NewAbelian(1, []float64{1}))

	before, err := n.Count()
	if err != nil {
		t.Fatal(err)
	}
	// Residual @ leaf + 2 child leaves = 3 (stub must not count).
	if before != 3 {
		t.Fatalf("Count=%d want 3 (1 residual + 2 child leaves; stub excluded)", before)
	}
	if rootSet.Abelian().Count() < 10 {
		t.Fatalf("parent Abelian=%d want ≥10 from stub — cache summary visible on set", rootSet.Abelian().Count())
	}

	items, _ := c.Delegate(child)
	if len(items) < 2 {
		t.Fatalf("Delegate returned %d items", len(items))
	}
	n.owned.Remove(0, idChild)

	after, _ := n.Count()
	if after != 1 {
		t.Fatalf("after Delegate Count=%d want 1 residual only", after)
	}
	stub, ok := rootSet.Get(child)
	if !ok || stub.Count() < 1 {
		t.Fatalf("parent stub missing after Delegate ok=%v", ok)
	}
	own, _ := n.Ownership()
	if _, ok := own[coll][child]; ok {
		t.Fatalf("child still in Ownership")
	}
	if _, ok := own[coll][root][child]; !ok {
		t.Fatalf("parent missing delegation mark for %q", child)
	}
	t.Logf("before=%d after=%d stub=%d parentAbelian=%d", before, after, stub.Count(), rootSet.Abelian().Count())
}

func TestStatusZonesCountParentShellsWithZeroResidualItems(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 4)
	const coll = "demo"
	n.create(coll, encoding.BASE64.Root())
	c, _ := n.collections.Get(coll)

	root := encoding.BASE64.Root()
	child := "b"
	c.EnsureSet(child)
	c.Own(root, domain.Delegation{child: domain.Handoff{}})
	idRoot := zoneID(t, coll, root)
	n.owned.Upsert(0, idRoot, map[domain.Key]any{}, func(_ int, _ []byte, m map[domain.Key]any) {
		m[domain.Key{Collection: coll, Location: root}] = struct{}{}
	})
	rootSet, _ := c.Get(root)
	rootSet.Put(child, domain.NewAbelian(50, []float64{50}))

	total, _ := n.Count()
	own, _ := n.Ownership()
	zones := len(own[coll])
	residual := n.zoneItemCount(domain.Key{Collection: coll, Location: root})
	t.Logf("Items=%d zones=%d residualLeaves=%d parentAbelian=%d", total, zones, residual, rootSet.Abelian().Count())
	if total != 0 {
		t.Fatalf("Items=%d want 0 (pure parent shell)", total)
	}
	if zones < 1 {
		t.Fatal("parent shell missing from Ownership")
	}
	if residual != 0 {
		t.Fatalf("residual=%d want 0", residual)
	}
	if rootSet.Abelian().Count() != 50 {
		t.Fatalf("parent Abelian=%d want 50 stub — looks like owned load if misread", rootSet.Abelian().Count())
	}
}

func TestStickyItemsReasonClearsWhenCool(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 4)
	n.EnableAutoscale(AutoscaleConfig{Enabled: true, Role: "spawned", ItemsLimit: 100})
	n.autoscale.mu.Lock()
	n.autoscale.lastReason = "items"
	n.autoscale.mu.Unlock()

	n.autoscale.Tick(PressureInput{OwnedItems: 0, SelfName: n.Name()}, nil, nil)

	snap := n.AutoscaleSnapshot()
	if snap["last_reason"] != "" {
		t.Fatalf("last_reason=%v want cleared when under items_limit", snap["last_reason"])
	}
	p, _ := snap["pressure"].(map[string]any)
	if p != nil && p["hot_signal"] == "items" {
		t.Fatalf("hot_signal still items with OwnedItems=0: %v", p["hot_signal"])
	}
}
