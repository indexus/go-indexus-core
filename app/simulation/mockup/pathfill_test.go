package mockup

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/storage"
)

func TestQueueBackpressureOnNode(t *testing.T) {
	ResetNetwork()
	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := core.NewSettings(name, 21100, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	settings.SetQueueMax(2)
	settings.SetAdvertise("127.0.0.1")
	n, err := core.NewNode(settings, NewContact, nil, storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}

	mk := func(id string) *domain.Item {
		return &domain.Item{Collection: "demo", Location: "aa", Id: id, Metrics: []float64{1}}
	}
	if err := n.New(mk("a"), encoding.BASE64.Root(), nil); err != nil {
		t.Fatal(err)
	}
	if err := n.New(mk("b"), encoding.BASE64.Root(), nil); err != nil {
		t.Fatal(err)
	}
	if err := n.New(mk("c"), encoding.BASE64.Root(), nil); err == nil {
		t.Fatal("expected queue full error")
	}
}

func TestPathFillFromOwnerNeighbor(t *testing.T) {
	ResetNetwork()

	n1 := newConvergenceNode(t, 8)
	n2 := newConvergenceNode(t, 8, n1)

	converge(t, []*core.Node{n1, n2}, 4, 2, 20)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	loc := "aa"
	item := &domain.Item{
		Collection: collection,
		Location:   loc,
		Id:         "x1",
		Metrics:    []float64{1, 2, 3, 4, 5},
	}
	if err := n1.New(item, encoding.BASE64.Root(), nil); err != nil {
		t.Fatal(err)
	}
	drainQueues(t, []*core.Node{n1, n2}, 3, 5*time.Second)
	converge(t, []*core.Node{n1, n2}, 2, 2, 15)

	c1, _ := n1.Count()
	c2, _ := n2.Count()
	if c1+c2 == 0 {
		t.Fatal("item was not stored on any node")
	}

	owner, other := n1, n2
	if c2 > 0 && c1 == 0 {
		owner, other = n2, n1
	}

	_, ownedSet, err := owner.Get(collection, "@", false, nil, false)
	if err != nil || ownedSet == nil {
		t.Fatalf("owner must serve locally: err=%v set=%v", err, ownedSet != nil)
	}

	_, filled, err := other.Get(collection, "@", true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if filled == nil {
		t.Fatal("non-owner should path-fill from owner neighbor")
	}

	_, cached, err := other.Get(collection, "@", false, nil, false)
	if err != nil || cached == nil {
		t.Fatalf("expected cache hit on edge after path-fill: err=%v set=%v", err, cached != nil)
	}
}
