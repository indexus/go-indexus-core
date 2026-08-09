package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// Phase U (background convergence) and both sides of phase D — §4/U, §4/D.

// delegatedParent owns @ with child marked delegated, i.e. held by a peer. The
// stub is the only thing the parent has, and only the child's owner may move it.
func delegatedParent(t *testing.T, n *Node, coll, child string, stub int) *domain.Set {
	t.Helper()
	n.create(coll, "@")
	c, ok := n.collections.Get(coll)
	if !ok {
		t.Fatal("missing collection")
	}
	c.Own("@", domain.Delegation{child: domain.Handoff{Peer: "named-holder", At: 1}})
	parentSet, ok := c.Get("@")
	if !ok {
		t.Fatal("parent set missing")
	}
	parentSet.Put(child, domain.NewAbelian(stub, []float64{float64(stub)}))
	return parentSet
}

func TestPullDelegatedAggregateUpdatesParent(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll, child = "demo", "a" // Parent(child) == "@" under BASE64

	parentSet := delegatedParent(t, n, coll, child, 5)
	peer := &ownerPeer{readPeer: atZone(t, coll, child, setOf("x:1", 42))}
	n.acknowledge([]domain.Contact{peer})

	if !n.pullDelegatedAggregate(coll, child, peer) {
		t.Fatal("pullDelegatedAggregate failed")
	}
	got, ok := parentSet.Get(child)
	if !ok {
		t.Fatal("parent entry missing after pull")
	}
	if got.Count() != 42 {
		t.Fatalf("parent entry count=%d want 42 (converged to the owner's truth)", got.Count())
	}
}

// A /set response can come from a cache or a path-fill. Only the owner, which
// is what answering /aggregates certifies, may move a stub. See protocol.md §2.
func TestParentStubIgnoresNonAuthoritativeSource(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll, child = "demo", "a"

	parentSet := delegatedParent(t, n, coll, child, 5)
	// A readPeer answers /set and nothing else — no /aggregates, no authority.
	peer := atZone(t, coll, child, setOf("x:1", 42))
	n.acknowledge([]domain.Contact{peer})

	if n.pullDelegatedAggregate(coll, child, peer) {
		t.Fatal("a peer that does not answer /aggregates must not satisfy a parent sync")
	}
	if got, ok := parentSet.Get(child); !ok || got.Count() != 5 {
		t.Fatalf("stub moved on a non-authoritative read: %v", got)
	}
}

// An owner may legitimately report less than the stub held — half the zone can
// have split away. The previous epoch ordering rejected this.
func TestParentStubAcceptsOwnerShrink(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll, child = "demo", "a"

	parentSet := delegatedParent(t, n, coll, child, 90000)
	peer := &ownerPeer{readPeer: atZone(t, coll, child, setOf("x:1", 45000))}
	n.acknowledge([]domain.Contact{peer})

	if !n.pullDelegatedAggregate(coll, child, peer) {
		t.Fatal("pullDelegatedAggregate failed")
	}
	got, _ := parentSet.Get(child)
	if got.Count() != 45000 {
		t.Fatalf("parent entry count=%d want 45000 (owner shrink rejected)", got.Count())
	}
}

// No authority reachable is not the same as an authority reporting zero. A stub
// keeps its last known value rather than decaying toward an empty mesh.
func TestParentStubHoldsWhenNoAuthorityAnswers(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll, child = "demo", "a"

	parentSet := delegatedParent(t, n, coll, child, 4200)
	n.acknowledge([]domain.Contact{atZone(t, coll, child, nil)})

	if n.pullDelegatedAggregate(coll, child, nil) {
		t.Fatal("a sync reported success with nobody to certify it")
	}
	if got, ok := parentSet.Get(child); !ok || got.Count() != 4200 {
		t.Fatalf("stub decayed to %v when the owner was unreachable, want 4200", got)
	}
}

func TestReconcileDelegatedParentsRunsOnUpdate(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll, child = "demo", "b"

	parentSet := delegatedParent(t, n, coll, child, 1)
	peer := &ownerPeer{readPeer: atZone(t, coll, child, setOf("y:1", 7))}
	n.acknowledge([]domain.Contact{peer})

	if err := n.Update(); err != nil {
		t.Fatal(err)
	}
	if got, ok := parentSet.Get(child); !ok || got.Count() != 7 {
		t.Fatalf("after Update parent entry=%v want 7", got)
	}
}

// While ownership moves, a cache pull would race the handoff and cache a value
// that is about to be wrong. Parent reconciliation still runs: it reads from
// the owner, whoever that now is.
func TestUpdateSkipsCachePullsWhileTransferBusy(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	n.settings.SetUpdateEta(0)
	n.settings.updateBatch = false

	owner := zonePeer(t, n, "demo", "qr", setOf("qr:new", 9))
	n.cache.SetHops("demo", "qr", setOf("qr:old", 1), 1)
	n.cache.Get("demo", "qr")

	n.rebalancing.Store(true)
	if !n.TransferBusy() {
		t.Fatal("fixture broken: the node is not busy")
	}
	if err := n.Update(); err != nil {
		t.Fatal(err)
	}
	if got := owner.refreshs.Load(); got != 0 {
		t.Fatalf("Update pulled %d cache refreshes while a transfer was in flight", got)
	}

	n.rebalancing.Store(false)

	if err := n.Update(); err != nil {
		t.Fatal(err)
	}
	if owner.refreshs.Load() == 0 {
		t.Fatal("Update never resumed cache refreshes once the transfer ended")
	}
}

// The debounce is what keeps Claim pushes and parent-stub pulls from
// re-asking the same peer on every Update tick.
func TestParentSyncDebounceHoldsForOneWindow(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	if !n.allowParentSync("demo", "aa") {
		t.Fatal("a first sync must be allowed")
	}
	n.markParentSync("demo", "aa")
	if n.allowParentSync("demo", "aa") {
		t.Fatal("a second sync inside the window must be refused")
	}
	if !n.allowParentSync("demo", "ab") {
		t.Fatal("the debounce leaked onto a neighbouring location")
	}
	if !n.allowParentSync("other", "aa") {
		t.Fatal("the debounce leaked onto another collection")
	}
}

func TestListChildrenOwnedAndDelegated(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll = "demo"
	n.create(coll, encoding.BASE64.Root())
	c, _ := n.collections.Get(coll)
	c.EnsureSet("a")
	c.Own("@", domain.Delegation{"a": domain.Handoff{}})
	c.Own("a", domain.Delegation{})
	setA, _ := c.Get("a")
	setA.Put("a:x", domain.NewAbelian(4, []float64{4}))
	c.MarkDelegated("@", "b")
	parentSet, _ := c.Get("@")
	parentSet.Put("b", domain.NewAbelian(2, []float64{2}))

	entries, err := n.Children(coll, "@")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entries["a"]; !ok || e.Abelian.Count() != 4 {
		t.Fatalf("owned child a: %#v", entries["a"])
	}
	if e, ok := entries["b"]; !ok || e.Abelian.Count() != 2 {
		t.Fatalf("delegated child b: %#v", entries["b"])
	}
}

func TestChildrenEmptyCollection(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	entries, err := n.Children("missing", "@")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("want empty map, got %d", len(entries))
	}
}

// Path-filled /set keys must appear on /children even before Own/MarkDelegated.
func TestListChildrenIncludesSetKeysWithoutOwnership(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll = "demo"
	n.create(coll, encoding.BASE64.Root())
	c, _ := n.collections.Get(coll)
	c.Own("@", domain.Delegation{})
	root, _ := c.Get("@")
	root.Put("7", domain.NewAbelian(8000, []float64{1}))
	root.Put("q", domain.NewAbelian(100, []float64{2}))
	// Item row must not become a discovery child.
	root.Put("7abc:item-1", domain.NewAbelian(1, []float64{1}))
	// Self-key must be ignored.
	root.Put("@", domain.NewAbelian(0, nil))

	entries, err := n.Children(coll, "@")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := entries["7"]; !ok || e.Abelian.Count() != 8000 {
		t.Fatalf("set-key child 7: %#v", entries["7"])
	}
	if e, ok := entries["q"]; !ok || e.Abelian.Count() != 100 {
		t.Fatalf("set-key child q: %#v", entries["q"])
	}
	if _, ok := entries["7abc:item-1"]; ok {
		t.Fatal("item row must not appear in /children")
	}
	if _, ok := entries["@"]; ok {
		t.Fatal("self-key must not appear in /children")
	}
}
