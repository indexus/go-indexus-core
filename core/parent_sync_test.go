package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

// summaryPeer returns a fixed abelian for Get(collection, location).
type summaryPeer struct {
	*dialPeer
	set *domain.Set
}

func (p *summaryPeer) Get(collection, location string, deep bool, hop int) (domain.Contact, *domain.Set, error) {
	return p, p.set, nil
}

func TestPullDelegatedAggregateUpdatesParent(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll = "demo"
	const child = "a" // Parent(child) == "@" under BASE64

	n.create(coll, "@")
	c, ok := n.collections.Get(coll)
	if !ok {
		t.Fatal("missing collection")
	}
	// Parent owns @ with child marked delegated (child not owned locally).
	c.Own("@", domain.Delegation{child: nil})
	parentSet, ok := c.Get("@")
	if !ok {
		t.Fatal("parent set missing")
	}
	parentSet.Put(child, domain.NewAbelian(5, []float64{5}))

	remote := domain.NewSet()
	remote.Put("x:1", domain.NewAbelian(42, []float64{42}))
	peer := &summaryPeer{
		dialPeer: newDialPeer("RemotePeerAAAAAA", "127.0.0.1", 21001),
		set:      remote,
	}
	n.acknowledge([]domain.Contact{peer})

	if !n.pullDelegatedAggregate(coll, child, peer) {
		t.Fatal("pullDelegatedAggregate failed")
	}
	got, ok := parentSet.Get(child)
	if !ok {
		t.Fatal("parent entry missing after pull")
	}
	if got.Count() != 42 {
		t.Fatalf("parent entry count=%d want 42 (converged to remote leaf truth)", got.Count())
	}
}

func TestReconcileDelegatedParentsRunsOnUpdate(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll = "demo"
	const child = "b"

	n.create(coll, "@")
	c, _ := n.collections.Get(coll)
	c.Own("@", domain.Delegation{child: nil})
	parentSet, _ := c.Get("@")
	parentSet.Put(child, domain.NewAbelian(1, []float64{1}))

	remote := domain.NewSet()
	remote.Put("y:1", domain.NewAbelian(7, []float64{7}))
	peer := &summaryPeer{
		dialPeer: newDialPeer("RemotePeerBBBBBB", "127.0.0.1", 21002),
		set:      remote,
	}
	n.acknowledge([]domain.Contact{peer})

	if err := n.Update(); err != nil {
		t.Fatal(err)
	}
	got, ok := parentSet.Get(child)
	if !ok || got.Count() != 7 {
		cnt := 0
		if got != nil {
			cnt = got.Count()
		}
		t.Fatalf("after Update parent entry count=%d want 7 ok=%v", cnt, ok)
	}
}
