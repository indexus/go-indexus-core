package mockup

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// A delete entering the mesh on any node must reach the owner and remove the
// item, whichever node happens to own the key.
func TestDeleteReachesOwnerFromAnyEdge(t *testing.T) {
	ResetNetwork()

	n1 := newConvergenceNode(t, 8)
	n2 := newConvergenceNode(t, 8, n1)
	nodes := []*core.Node{n1, n2}

	converge(t, nodes, 4, 2, 20)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	item := &domain.Item{
		Collection: collection,
		Location:   "aa",
		Id:         "x1",
		Metrics:    []float64{1, 2, 3, 4, 5},
	}

	if err := n1.New(item, encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatal(err)
	}
	drainQueues(t, nodes, 3, 5*time.Second)
	converge(t, nodes, 2, 2, 15)

	if got := totalCount(t, nodes); got != 1 {
		t.Fatalf("expected exactly 1 item in the mesh, got %d", got)
	}

	// Enter on n2 regardless of ownership: deleteOp forwards XOR-wise if needed.
	if err := n2.Delete(item, encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatal(err)
	}
	drainQueues(t, nodes, 3, 5*time.Second)

	if got := totalCount(t, nodes); got != 0 {
		t.Fatalf("item still present after delete, total count %d", got)
	}
}

// Deleting an id that was never added is a successful no-op and must not
// corrupt the aggregates of the items that are there.
func TestDeleteUnknownIdIsNoOp(t *testing.T) {
	ResetNetwork()

	n1 := newConvergenceNode(t, 8)
	nodes := []*core.Node{n1}

	converge(t, nodes, 2, 2, 10)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	kept := &domain.Item{
		Collection: collection,
		Location:   "aa",
		Id:         "keep",
		Metrics:    []float64{1},
	}
	if err := n1.New(kept, encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatal(err)
	}
	drainQueues(t, nodes, 3, 5*time.Second)

	if got := totalCount(t, nodes); got != 1 {
		t.Fatalf("setup: expected 1 item, got %d", got)
	}

	ghost := &domain.Item{Collection: collection, Location: "aa", Id: "never-added"}
	if err := n1.Delete(ghost, encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatal(err)
	}
	drainQueues(t, nodes, 3, 5*time.Second)

	if got := totalCount(t, nodes); got != 1 {
		t.Fatalf("deleting an unknown id changed the count: got %d want 1", got)
	}
}
