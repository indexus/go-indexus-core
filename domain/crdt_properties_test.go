package domain

// CRDT properties of the collection state (docs/protocol.md §P2).
//
// Strong Eventual Consistency needs apply to be commutative, associative and
// idempotent for everything the mesh may deliver out of order or twice:
// duplicated Transfers, residual Handoffs, WAL replay after a crash.
//
//   - Adds of distinct ids commute (order-independence, tested here).
//   - The same add twice is a no-op; same id with new metrics replaces with
//     delta (idempotence, collection_crud_test.go and here).
//   - Tombstones join on max generation: commutative and idempotent.
//   - Add vs tombstone of the SAME id does NOT commute: the later arrival
//     wins ("add-wins on re-add"). That is a deliberate choice — a client
//     re-adding a deleted id must succeed — and the tests below pin it so a
//     change is loud. The consequence (a duplicated stale add can resurrect
//     a deleted id) is an accepted window, documented in the protocol spec.

import (
	"fmt"
	"math/rand"
	"testing"
)

type crdtItem struct {
	location string
	id       string
	metric   float64
}

func crdtDataset() []crdtItem {
	// Locations spread over several zones and depths so Shrink materialises
	// different intermediate sets depending on arrival order.
	items := make([]crdtItem, 0, 60)
	for i := 0; i < 60; i++ {
		loc := fmt.Sprintf("%c%c", 'a'+rune(i%5), 'a'+rune(i%3))
		if i%7 == 0 {
			loc += "c" // a deeper leaf every few items
		}
		items = append(items, crdtItem{location: loc, id: fmt.Sprintf("id-%02d", i), metric: float64(i%9) + 1})
	}
	return items
}

// state flattens what matters for convergence: the item set and the root
// aggregate. The internal set structure is an index and may legitimately
// differ; the observable state must not.
func crdtState(t *testing.T, c *Collection) (map[string]float64, int, []float64) {
	t.Helper()

	items := make(map[string]float64)
	c.Traverse("@",
		func(string, *Abelian) {},
		func(_ string, location, id string, a *Abelian) {
			items[location+":"+id] = a.Metrics()[0]
		},
	)
	root, ok := c.Get("@")
	if !ok {
		t.Fatal("collection lost its root set")
	}
	agg := root.Abelian()
	return items, agg.Count(), agg.Metrics()
}

// Convergence: any application order of the same add set yields the same
// observable state (item set + root aggregate).
func TestAddOrderIndependence(t *testing.T) {
	base := fakeBase{length: 4}
	dataset := crdtDataset()

	reference := NewCollection("demo", "@", base)
	for _, it := range dataset {
		// Delegation high enough that no zone is delegated away mid-test:
		// a delegated zone refuses local adds by design (it belongs to a
		// peer), which is placement, not merge.
		reference.Add(it.location, it.id, []float64{it.metric}, 1000)
	}
	wantItems, wantCount, wantMetrics := crdtState(t, reference)
	if len(wantItems) != len(dataset) {
		t.Fatalf("reference lost items: %d/%d", len(wantItems), len(dataset))
	}

	for seed := int64(1); seed <= 10; seed++ {
		shuffled := append([]crdtItem(nil), dataset...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		c := NewCollection("demo", "@", base)
		for _, it := range shuffled {
			c.Add(it.location, it.id, []float64{it.metric}, 1000)
		}

		gotItems, gotCount, gotMetrics := crdtState(t, c)
		if gotCount != wantCount {
			t.Fatalf("seed %d: root count %d want %d", seed, gotCount, wantCount)
		}
		if len(gotMetrics) > 0 && len(wantMetrics) > 0 && gotMetrics[0] != wantMetrics[0] {
			t.Fatalf("seed %d: root metric %v want %v", seed, gotMetrics[0], wantMetrics[0])
		}
		if len(gotItems) != len(wantItems) {
			t.Fatalf("seed %d: %d items want %d", seed, len(gotItems), len(wantItems))
		}
		for k, v := range wantItems {
			if gotItems[k] != v {
				t.Fatalf("seed %d: item %s metric %v want %v", seed, k, gotItems[k], v)
			}
		}
	}
}

// Duplicated delivery: applying the very same adds a second time (transfer
// retried after a lost ACK, WAL replay) must not change the state.
func TestAddSecondPassIsNoOp(t *testing.T) {
	base := fakeBase{length: 4}
	dataset := crdtDataset()

	c := NewCollection("demo", "@", base)
	for pass := 0; pass < 2; pass++ {
		for _, it := range dataset {
			c.Add(it.location, it.id, []float64{it.metric}, 1000)
		}
	}

	items, count, _ := crdtState(t, c)
	if len(items) != len(dataset) || count != len(dataset) {
		t.Fatalf("duplicate pass changed state: items=%d count=%d want %d", len(items), count, len(dataset))
	}
}

// Tombstones join on max generation: any order and any duplication of
// ApplyTombstone converges to the highest generation seen.
func TestTombstoneMaxGenCommutes(t *testing.T) {
	gens := []uint64{3, 1, 2, 3, 1}

	for seed := int64(1); seed <= 5; seed++ {
		shuffled := append([]uint64(nil), gens...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) {
			shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
		})

		c := NewCollection("demo", "@", fakeBase{length: 4})
		c.Add("aa", "x", []float64{1}, 1000)
		for _, g := range shuffled {
			c.ApplyTombstone("aa", "x", g)
		}

		if !c.IsTombstoned("aa", "x") {
			t.Fatalf("seed %d: id not tombstoned", seed)
		}
		tombs := c.TombstonesUnder("@")
		if len(tombs) != 1 || tombs[0].Gen != 3 {
			t.Fatalf("seed %d: tombstones=%v want single gen 3", seed, tombs)
		}
	}
}

// Pinned semantics: add and tombstone of the same id do NOT commute.
//
// tombstone then add → alive (a client may re-create what it deleted);
// add then tombstone → deleted. The mesh depends on this being stable:
// change it and delete/re-add flows break, keep it and a duplicated stale
// add can resurrect a deleted id until its writer stops retrying.
func TestAddTombstoneOrderDependence(t *testing.T) {
	// Order 1: tombstone arrives first (even for a never-seen id), the add
	// arrives later — the id lives.
	c1 := NewCollection("demo", "@", fakeBase{length: 4})
	c1.ApplyTombstone("aa", "x", 1)
	if !c1.IsTombstoned("aa", "x") {
		t.Fatal("tombstone-first: id should be tombstoned before the add")
	}
	if areas := c1.Add("aa", "x", []float64{1}, 1000); areas == nil {
		t.Fatal("tombstone-first: re-add refused")
	}
	if c1.IsTombstoned("aa", "x") {
		t.Fatal("tombstone-first: add did not clear the tombstone (add-wins broken)")
	}
	if _, count, _ := crdtState(t, c1); count != 1 {
		t.Fatalf("tombstone-first: count=%d want 1", count)
	}

	// Order 2: add first, tombstone after — the id is deleted.
	c2 := NewCollection("demo", "@", fakeBase{length: 4})
	c2.Add("aa", "x", []float64{1}, 1000)
	c2.ApplyTombstone("aa", "x", 1)
	if !c2.IsTombstoned("aa", "x") {
		t.Fatal("add-first: tombstone did not delete the id")
	}
	if _, count, _ := crdtState(t, c2); count != 0 {
		t.Fatalf("add-first: count=%d want 0", count)
	}
}

// Remove of an id this node never held plants a tombstone (gen 1) so the
// delete survives a later ownership transfer — the "delete raced ahead of
// its add" edge, common when a client writes through two different edges.
func TestRemoveAbsentPlantsTombstone(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 4})

	_, gen, ok := c.Remove("aa", "ghost")
	if !ok || gen != 1 {
		t.Fatalf("remove absent: ok=%v gen=%d want ok gen 1", ok, gen)
	}
	if !c.IsTombstoned("aa", "ghost") {
		t.Fatal("remove absent: no tombstone planted")
	}

	// The late add then wins (same pinned semantics as above).
	if areas := c.Add("aa", "ghost", []float64{1}, 1000); areas == nil {
		t.Fatal("late add refused")
	}
	if c.IsTombstoned("aa", "ghost") {
		t.Fatal("late add did not clear the tombstone")
	}
}
