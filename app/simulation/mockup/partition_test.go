package mockup

import (
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// A split brain is the general case of duplicate ownership: both halves elect
// an owner for the same zone and both accept writes, so on remerge two nodes
// claim the zone with diverged item sets. The protocol has no reconciliation
// subsystem and needs none — the duplicate is a placement question, and R3
// answers it. See protocol.md §4/Q and the crash matrix row "Partition heal".
//
// What this pins, which no unit test can: the two halves keep serving during
// the cut, and the remerge loses nothing and duplicates nothing.
func TestConvergence_PartitionRemergeKeepsTheUnion(t *testing.T) {
	ResetNetwork()

	const (
		delegation = 25
		seeded     = 120
		perSide    = 40
	)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	n1 := newConvergenceNode(t, delegation)
	n2 := newConvergenceNode(t, delegation, n1)
	n3 := newConvergenceNode(t, delegation, n1)
	n4 := newConvergenceNode(t, delegation, n1)
	nodes := []*core.Node{n1, n2, n3, n4}

	write := func(t *testing.T, target *core.Node, id string, i int) {
		t.Helper()
		loc := twoCharLocation(i)
		if err := target.New(&domain.Item{
			Collection: collection,
			Location:   loc,
			Id:         id,
			Metrics:    []float64{1, 2, 3, 4, 5},
		}, "@", nil); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}

	for i := 0; i < seeded; i++ {
		write(t, n1, fmt.Sprintf("seed-%d", i), i)
	}
	drainQueues(t, nodes, 5, 10*time.Second)
	converge(t, nodes, 5, 2, 60)
	if total := totalCount(t, nodes); total != seeded {
		t.Fatalf("seeding lost items: %d want %d", total, seeded)
	}

	left, right := []*core.Node{n1, n2}, []*core.Node{n3, n4}
	network.Partition(left, right)

	// Each half keeps running the protocol on its own view: with half the mesh
	// gone, both re-elect owners for the zones they can no longer reach.
	for i := 0; i < perSide; i++ {
		write(t, n1, fmt.Sprintf("left-%d", i), i)
		write(t, n3, fmt.Sprintf("right-%d", i), i)
	}
	drainQueues(t, nodes, 5, 10*time.Second)
	for round := 0; round < 6; round++ {
		for _, half := range [][]*core.Node{left, right} {
			for _, n := range half {
				if err := n.Observe(); err != nil {
					t.Fatalf("Observe %s: %v", n.Name(), err)
				}
				if err := n.Refresh(); err != nil {
					t.Fatalf("Refresh %s: %v", n.Name(), err)
				}
			}
		}
		drainQueues(t, nodes, 3, 10*time.Second)
	}

	expected := seeded + 2*perSide
	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("items lost during the split: %d want %d", total, expected)
	}

	network.Heal()
	converge(t, nodes, 6, 3, 80)

	if total := totalCount(t, nodes); total != expected {
		perNode := make([]int, len(nodes))
		for i, n := range nodes {
			perNode[i], _ = n.Count()
		}
		t.Fatalf("remerge changed the total: %d want %d (per-node %v)", total, expected, perNode)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	// Every id written on either side must still be readable through the mesh,
	// from a node that is not necessarily its owner.
	for i := 0; i < perSide; i++ {
		location := twoCharLocation(i)
		for _, id := range []string{fmt.Sprintf("left-%d", i), fmt.Sprintf("right-%d", i)} {
			if !readableFrom(t, n2, collection, location, id) {
				t.Fatalf("%s:%s written during the split is unreadable after the remerge", location, id)
			}
		}
	}
}

// readableFrom resolves a leaf through the mesh from one node. A leaf is filed
// under its parent zone and moves one level down each time that zone shrinks,
// so both candidates are asked before calling it lost.
func readableFrom(t *testing.T, from *core.Node, collection, location, id string) bool {
	t.Helper()
	leaf := location + ":" + id
	for _, zone := range []string{encoding.BASE64.Parent(location), location} {
		if zone == "" {
			continue
		}
		_, set, err := from.Get(collection, zone, true, nil, false)
		if err != nil || set == nil {
			continue
		}
		if _, ok := set.Get(leaf); ok {
			return true
		}
	}
	return false
}

// A cut link is not an empty answer. An unreachable owner must read as a miss,
// so the reader keeps looking and the zone is not remembered as empty — the
// mistake that would otherwise propagate into a parent stub and stick.
func TestPartitionedReadIsAMissNotAnEmptyZone(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 30
		collections = 20
		perColl     = 12
	)
	n1 := newConvergenceNode(t, delegation)
	n2 := newConvergenceNode(t, delegation, n1)
	nodes := []*core.Node{n1, n2}

	names := make([]string, collections)
	for i := range names {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		names[i] = name
		for j := 0; j < perColl; j++ {
			loc := encoding.BASE64.CharAt(j)
			if err := n1.New(&domain.Item{
				Collection: name,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", j),
				Metrics:    []float64{1},
			}, "@", nil); err != nil {
				t.Fatalf("seed: %v", err)
			}
		}
	}
	drainQueues(t, nodes, 5, 10*time.Second)
	converge(t, nodes, 5, 2, 60)

	// Any zone n2 owns and n1 can read is a link worth cutting.
	collection, location, before := remoteReadableZone(t, n1, n2)
	if collection == "" {
		t.Skip("no zone landed on the second node; nothing to cut")
	}

	network.Partition([]*core.Node{n1}, []*core.Node{n2})
	_, after, err := n1.Get(collection, location, true, nil, false)
	if err != nil {
		t.Fatalf("a partitioned read must degrade, not fail: %v", err)
	}
	if after != nil && after.Count() == 0 {
		t.Fatal("an unreachable owner was reported as an empty zone")
	}

	network.Heal()
	_, healed, err := n1.Get(collection, location, true, nil, false)
	if err != nil || healed == nil || healed.Count() != before {
		t.Fatalf("the read did not recover after the heal: set=%v err=%v want count=%d", healed, err, before)
	}
}

// remoteReadableZone finds a zone owned by owner that reader resolves to a
// non-empty set, and returns it with the count read before any cut.
func remoteReadableZone(t *testing.T, reader, owner *core.Node) (collection, location string, count int) {
	t.Helper()
	own, err := owner.Ownership()
	if err != nil {
		t.Fatalf("Ownership: %v", err)
	}
	for coll, locs := range own {
		for loc := range locs {
			_, set, err := reader.Get(coll, loc, true, nil, false)
			if err != nil || set == nil || set.Count() == 0 {
				continue
			}
			return coll, loc, set.Count()
		}
	}
	return "", "", 0
}

// twoCharLocation spreads writes over the alphabet so zones actually split.
func twoCharLocation(i int) string {
	base := encoding.BASE64
	return base.CharAt(i%base.Length()) + base.CharAt((i/base.Length())%base.Length())
}
