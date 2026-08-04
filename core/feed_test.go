package core

import (
	"errors"
	"fmt"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"sync/atomic"
	"testing"
	"time"
)

type forwardFailPeer struct {
	*dialPeer
	calls atomic.Int64
}

func (p *forwardFailPeer) New(*domain.Item, string, string) error {
	p.calls.Add(1)
	return fmt.Errorf("simulated peer EOF")
}

func (p *forwardFailPeer) Delete(*domain.Item, string, string) error {
	p.calls.Add(1)
	return fmt.Errorf("simulated peer EOF")
}

func (p *forwardFailPeer) Transfer(domain.Peer, domain.Key, []*domain.Item) error {
	p.calls.Add(1)
	return fmt.Errorf("simulated peer EOF")
}

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

func TestInsertReportsForwardFailure(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	peer := nearestFailPeer(t, n, "demo", root)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	before := n.queue.Length()

	if err := n.process(NewElement(item, root, root)); err == nil {
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

	if err := n.process(NewDeleteElement(item, root, root)); err == nil {
		t.Fatal("deleteOp swallowed the forward failure: Feed cannot back off")
	}
	if peer.calls.Load() == 0 {
		t.Fatal("forward never attempted")
	}
	if got := n.queue.Length(); got != before {
		t.Fatalf("deleteOp re-queued behind Feed's back: length=%d want %d", got, before)
	}
}

func TestFeedOnceKeepsElementAtQueueCapacity(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.settings.SetQueueMax(1)
	root := encoding.BASE64.Root()
	nearestFailPeer(t, n, "demo", root)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	n.queue.Add(NewElement(item, root, root))

	element, ok := n.queue.Consume()
	if !ok {
		t.Fatal("queue handed back nothing")
	}

	refill := &domain.Item{Collection: "demo", Location: "aa", Id: "y", Metrics: []float64{1}}
	if !n.queue.TryAdd(NewElement(refill, root, root), n.settings.queueMax) {
		t.Fatal("could not refill the freed slot")
	}

	if n.feedOnce(element) {
		t.Fatal("feedOnce reported success on a failing forward")
	}
	if got := n.queue.Length(); got != 2 {
		t.Fatalf("accepted write dropped at capacity: length=%d want 2", got)
	}
}

func TestFeedOncePausesWhileRebalancing(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	n.queue.Add(NewElement(item, root, root))

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

	n.settings.SetQueueMax(1)
	n.queue.Add(NewElement(
		&domain.Item{Collection: "demo", Location: "aa", Id: "pending", Metrics: []float64{1}},
		encoding.BASE64.Root(), encoding.BASE64.Root(),
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

type blockedPeer struct {
	*dialPeer
	inFlight atomic.Int64
	peak     atomic.Int64
	release  chan struct{}
}

func (p *blockedPeer) New(*domain.Item, string, string) error {
	current := p.inFlight.Add(1)
	for {
		peak := p.peak.Load()
		if current <= peak || p.peak.CompareAndSwap(peak, current) {
			break
		}
	}

	<-p.release
	p.inFlight.Add(-1)
	return nil
}

func nearestBlockedPeer(t *testing.T, n *Node, collection, location string) *blockedPeer {
	t.Helper()

	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, location, collection)
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	peer := &blockedPeer{
		dialPeer: newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 21000),
		release:  make(chan struct{}),
	}
	peer.id = id
	n.registered.Insert(0, peer.ID(), peer)
	return peer
}

func TestFeedForwardsInParallel(t *testing.T) {
	const parallel = 8

	n := newNodeOn(t, &memStorage{}, 64)
	n.settings.SetFeedWorkers(parallel)

	root := encoding.BASE64.Root()
	peer := nearestBlockedPeer(t, n, "demo", root)

	go func() { _ = n.Feed() }()
	defer close(peer.release)

	for i := 0; i < parallel; i++ {
		n.queue.Add(NewElement(&domain.Item{
			Collection: "demo",
			Location:   "aa",
			Id:         fmt.Sprintf("x%d", i),
			Metrics:    []float64{1},
		}, root, root))
	}

	deadline := time.Now().Add(5 * time.Second)
	for peer.peak.Load() < parallel && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}

	if peak := peer.peak.Load(); peak < parallel {
		t.Fatalf("ingress serialised the forwards: peak in flight=%d want %d", peak, parallel)
	}
}

func TestFeedBackoffStaysBounded(t *testing.T) {
	if got := feedBackoff(1); got > 5*time.Millisecond {
		t.Fatalf("first retry waits %v, too long to keep ingress moving", got)
	}
	if got := feedBackoff(10_000); got != 100*time.Millisecond {
		t.Fatalf("backoff unbounded: %v want 100ms", got)
	}
}

func TestForwardFailFastSkipsBusyPeer(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.settings.SetForwardRate(0)
	root := encoding.BASE64.Root()

	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, root, "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	peer := &busyPeer{
		forwardFailPeer: &forwardFailPeer{dialPeer: newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 21000)},
		err:             domain.ErrPeerBusy,
	}
	peer.id = id

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	if err := n.forwardNew(peer, item, root, root); !errors.Is(err, domain.ErrPeerBusy) {
		t.Fatalf("first forward: %v want ErrPeerBusy", err)
	}
	firstCalls := peer.calls.Load()
	if firstCalls != 1 {
		t.Fatalf("first forward calls=%d want 1", firstCalls)
	}

	if err := n.forwardNew(peer, item, root, root); !errors.Is(err, domain.ErrPeerBusy) {
		t.Fatalf("busy forward: %v want ErrPeerBusy", err)
	}
	if got := peer.calls.Load(); got != firstCalls {
		t.Fatalf("busy peer still dialed: calls=%d want %d", got, firstCalls)
	}

	time.Sleep(peerBusyWindow + 20*time.Millisecond)
	_ = n.forwardNew(peer, item, root, root)
	if got := peer.calls.Load(); got != firstCalls+1 {
		t.Fatalf("after busy window calls=%d want %d", got, firstCalls+1)
	}
}

type busyPeer struct {
	*forwardFailPeer
	err error
}

func (p *busyPeer) New(*domain.Item, string, string) error {
	p.calls.Add(1)
	return p.err
}
