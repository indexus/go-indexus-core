package core

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// forwardFailPeer refuses every forwarded op, standing in for a peer that was
// terminated or is itself draining.
type forwardFailPeer struct {
	*dialPeer
	calls atomic.Int64
}

func (p *forwardFailPeer) New(*domain.Item, string, domain.Visited) error {
	p.calls.Add(1)
	return fmt.Errorf("simulated peer EOF")
}

func (p *forwardFailPeer) Delete(*domain.Item, string, domain.Visited) error {
	p.calls.Add(1)
	return fmt.Errorf("simulated peer EOF")
}

func (p *forwardFailPeer) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	p.calls.Add(1)
	return "", fmt.Errorf("simulated peer EOF")
}

// nearestFailPeer registers a peer that is XOR-nearest for collection/location,
// so find() routes there and the op has to be forwarded.
func nearestFailPeer(t *testing.T, n *Node, collection, location string) *forwardFailPeer {
	t.Helper()

	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, location, collection)
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	peer := &forwardFailPeer{dialPeer: newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 21000)}
	peer.id = id
	n.registered.Insert(0, peer.ID(), peer)
	return peer
}

// A failed forward has to surface as an error. Swallowing it and re-queuing in
// place left Feed with nothing to back off on, so it re-consumed the same
// element immediately and spun a core at 100% for as long as the peer was down.
func TestInsertReportsForwardFailure(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	peer := nearestFailPeer(t, n, "demo", root)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	before := n.queue.Length()

	if err := n.process(NewElement(item, root)); err == nil {
		t.Fatal("insert swallowed the forward failure: Feed cannot back off")
	}
	if peer.calls.Load() == 0 {
		t.Fatal("forward never attempted")
	}
	if got := n.queue.Length(); got != before {
		t.Fatalf("insert re-queued behind Feed's back: length=%d want %d", got, before)
	}
}

func TestDeleteReportsForwardFailure(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	peer := nearestFailPeer(t, n, "demo", root)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	before := n.queue.Length()

	if err := n.process(NewDeleteElement(item, root)); err == nil {
		t.Fatal("deleteOp swallowed the forward failure: Feed cannot back off")
	}
	if peer.calls.Load() == 0 {
		t.Fatal("forward never attempted")
	}
	if got := n.queue.Length(); got != before {
		t.Fatalf("deleteOp re-queued behind Feed's back: length=%d want %d", got, before)
	}
}

// The retry has to survive a queue sitting at its bound. The old bounded TryAdd
// discarded the element in that exact case, which is how a node under load
// acknowledged writes and then dropped them.
func TestFeedOnceKeepsElementAtQueueCapacity(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.settings.SetQueueMax(1)
	root := encoding.BASE64.Root()
	nearestFailPeer(t, n, "demo", root)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	n.queue.Add(NewElement(item, root))

	element, ok := n.queue.Consume()
	if !ok {
		t.Fatal("queue handed back nothing")
	}

	// A producer takes the slot back while the forward is still in flight, which
	// is the normal state of a node ingesting under a write spike.
	refill := &domain.Item{Collection: "demo", Location: "aa", Id: "y", Metrics: []float64{1}}
	if !n.queue.TryAdd(NewElement(refill, root), 1024) {
		t.Fatal("could not refill the freed slot")
	}

	if n.feedOnce(element) {
		t.Fatal("feedOnce reported success on a failing forward")
	}
	if got := n.queue.Length(); got != 2 {
		t.Fatalf("accepted write dropped at capacity: length=%d want 2", got)
	}
}

// While Refresh Transfer is in flight, Feed must not apply/create/own zones —
// same SoftLeave gate, keyed off rebalancing.
func TestFeedOncePausesWhileRebalancing(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	n.queue.Add(NewElement(item, root))

	element, ok := n.queue.Consume()
	if !ok {
		t.Fatal("queue handed back nothing")
	}

	n.rebalancing.Store(true)
	if n.feedOnce(element) {
		t.Fatal("feedOnce applied while rebalancing")
	}
	if got := n.queue.Length(); got != 1 {
		t.Fatalf("element not held on queue: length=%d want 1", got)
	}

	count, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("re-owned during Refresh Transfer: count=%d want 0", count)
	}
}

// Refresh Delegates then Transfers; on failure restoreDelegated puts items
// back. Recovering through the bounded ingress queue used to drop those items
// whenever the queue sat at its bound — exactly when a rebalance is under way.
func TestRefreshRestoresItemsWhenRebalanceTransferFails(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	before, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if before == 0 {
		t.Fatal("precondition: node owns nothing")
	}

	candidate := nearestFailPeer(t, n, "demo", encoding.BASE64.Root())
	n.subscribe([]domain.Contact{candidate})

	// Ingress at its bound: the recovery must not depend on queue space.
	n.settings.SetQueueMax(1)
	n.queue.Add(NewElement(
		&domain.Item{Collection: "demo", Location: "aa", Id: "pending", Metrics: []float64{1}},
		encoding.BASE64.Root(),
	))

	if err := n.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if candidate.calls.Load() == 0 {
		t.Fatal("precondition: control() never attempted the rebalance transfer")
	}

	after, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if after != before {
		t.Fatalf("failed rebalance lost items: count=%d want %d", after, before)
	}
}
