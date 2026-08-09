package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

func TestControlCollectThenDelegate(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 8)
	root := encoding.BASE64.Root()
	col := "ctrl-demo"

	for i := 0; i < 20; i++ {
		it := &domain.Item{
			Collection: col,
			Location:   "aa",
			Id:         string(rune('a' + i)),
			Metrics:    []float64{1},
		}
		if err := node.New(it, root, nil); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
	drain(t, node)

	before, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}

	peer := newDialPeer("PeerZZZZZZZZZZZZ", "10.0.0.9", 21009)
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, "aa", col)
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	peer.id = id
	node.register([]domain.Contact{peer})
	node.subscribe([]domain.Contact{peer})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			_, _, _ = node.Get(col, root, false, nil, false)
		}
	}()

	plan := node.control()
	wg.Wait()

	after, err := node.Count()
	if err != nil {
		t.Fatalf("Count after control: %v", err)
	}
	if after != before {
		t.Fatalf("control() mutated local data: count=%d want %d", after, before)
	}
	if len(plan) == 0 {
		t.Fatal("control() planned nothing for a registered peer")
	}
}

func TestRefreshKeepsOwnershipUntilTransferAck(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	before := len(n.listOwnedKeys())
	if before == 0 {
		t.Fatal("precondition: node owns nothing")
	}

	candidate := nearestFailPeer(t, n, "demo", encoding.BASE64.Root())
	n.subscribe([]domain.Contact{candidate})

	if err := n.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if candidate.calls.Load() == 0 {
		t.Fatal("precondition: rebalance never attempted a transfer")
	}

	after := len(n.listOwnedKeys())
	if after != before {
		t.Fatalf("failed transfer dropped ownership: owned=%d want %d", after, before)
	}
}

func TestRefreshTransfersInParallel(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	root := encoding.BASE64.Root()
	for i := 0; i < 4; i++ {
		it := &domain.Item{
			Collection: "demo",
			Location:   "zz",
			Id:         string(rune('0' + i)),
			Metrics:    []float64{1},
		}
		if err := n.New(it, root, nil); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
	drain(t, n)

	barrier := &transferBarrier{gate: make(chan struct{})}
	a := &barrierSink{transferSink: newTransferSink("PeerAAAAAAAAAAAA", "10.0.0.1", 21001), barrier: barrier}
	b := &barrierSink{transferSink: newTransferSink("PeerBBBBBBBBBBBB", "10.0.0.2", 21002), barrier: barrier}
	idA, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, "aa", "demo")
	if err != nil {
		t.Fatalf("MergeEncodings a: %v", err)
	}
	idB, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, "zz", "demo")
	if err != nil {
		t.Fatalf("MergeEncodings b: %v", err)
	}
	a.id = idA
	b.id = idB
	n.register([]domain.Contact{a, b})
	n.subscribe([]domain.Contact{a, b})

	plan := n.control()
	if len(plan) < 2 {
		t.Skipf("XOR plan only hit %d peer(s); cannot assert cross-peer overlap", len(plan))
	}

	done := make(chan error, 1)
	go func() { done <- n.Refresh() }()

	select {
	case <-barrier.gate:

	case err := <-done:
		t.Fatalf("Refresh finished without overlapping transfers: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("transfers stayed serial: barrier never released")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh did not finish after barrier")
	}
}

type transferBarrier struct {
	mu    sync.Mutex
	count int
	gate  chan struct{}
	once  sync.Once
}

func (b *transferBarrier) enter() {
	b.mu.Lock()
	b.count++
	n := b.count
	b.mu.Unlock()
	if n >= 2 {
		b.once.Do(func() { close(b.gate) })
	}
}

type barrierSink struct {
	*transferSink
	barrier *transferBarrier
}

func (s *barrierSink) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	s.barrier.enter()
	select {
	case <-s.barrier.gate:
	case <-time.After(2 * time.Second):
		return "", fmt.Errorf("barrier timeout: Refresh appears serial")
	}
	return s.transferSink.Transfer(origin, key, items)
}

type handoffPeer struct {
	*dialPeer
	mu      sync.Mutex
	batches [][]*domain.Item
	news    []*domain.Item
	created atomic.Int64
}

func (p *handoffPeer) Transfer(_ domain.Peer, _ domain.Key, items []*domain.Item) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, append([]*domain.Item(nil), items...))
	return p.Name(), nil
}

func (p *handoffPeer) New(item *domain.Item, _ string, _ domain.Visited) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.news = append(p.news, item)
	p.created.Add(1)
	return nil
}

func (p *handoffPeer) transferredIDs() map[string]struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ids := make(map[string]struct{})
	for _, batch := range p.batches {
		for _, it := range batch {
			if it != nil && !it.Tombstone {
				ids[it.Id] = struct{}{}
			}
		}
	}
	return ids
}

func donorAndNewOwner(t *testing.T) (*Node, *handoffPeer) {
	t.Helper()

	keyID, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, "@", "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	donorID := make([]byte, 12)
	for i, b := range keyID {
		donorID[i] = ^b
	}
	name := encoding.BASE64.Encode(donorID)

	settings, err := NewSettings(name, 0, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	donor, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	zoneID, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, "aa", "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	owner := &handoffPeer{dialPeer: newDialPeer("NewOwnerAAAAAAAA", "10.0.0.7", 21007)}
	owner.id = zoneID
	return donor, owner
}

func TestHandoffThenResidualWriteForwardsToNewOwner(t *testing.T) {
	donor, owner := donorAndNewOwner(t)

	for _, id := range []string{"x1", "x2", "x3"} {
		if err := donor.New(item(id), "@", nil); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	drain(t, donor)
	if got := len(donor.listOwnedKeys()); got == 0 {
		t.Fatal("precondition: donor owns nothing")
	}

	donor.cache.SetHops("demo", "aab", domain.NewSet(), 1)
	donor.cache.SetHops("other", "zz", domain.NewSet(), 1)

	donor.register([]domain.Contact{owner})
	donor.subscribe([]domain.Contact{owner})

	if err := donor.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	ids := owner.transferredIDs()
	for _, id := range []string{"x1", "x2", "x3"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("item %s never transferred to the new owner", id)
		}
	}
	if got := len(donor.listOwnedKeys()); got != 0 {
		t.Fatalf("donor still owns %d keys after Transfer ACK", got)
	}
	if _, exist := donor.cache.Get("demo", "aab"); exist {
		t.Fatal("donor kept cached Sets for the collection it handed off")
	}
	if _, exist := donor.cache.Get("other", "zz"); !exist {
		t.Fatal("purge overreached: cache of an unrelated collection dropped")
	}

	if err := donor.New(item("x4"), "@", nil); err != nil {
		t.Fatalf("residual New: %v", err)
	}
	drain(t, donor)

	if owner.created.Load() == 0 {
		t.Fatal("residual write never forwarded to the new owner")
	}
	if got := len(donor.listOwnedKeys()); got != 0 {
		t.Fatalf("residual write re-owned %d keys on the donor — ownership ping-pong", got)
	}
	count, err := donor.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 0 {
		t.Fatalf("donor holds %d items after handoff — a residual write was absorbed locally", count)
	}
}
