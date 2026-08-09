package core

import (
	"fmt"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type refusingSink struct {
	*dialPeer
}

func (s *refusingSink) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	return "", fmt.Errorf("simulated transfer failure")
}

type overlapSink struct {
	*transferSink
	mu     sync.Mutex
	active int
	peak   int
}

func newOverlapSink(name, ip string, port int) *overlapSink {
	return &overlapSink{transferSink: newTransferSink(name, ip, port)}
}

func (s *overlapSink) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.peak {
		s.peak = s.active
	}
	s.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()

	return s.transferSink.Transfer(origin, key, items)
}

func (s *overlapSink) peakConcurrency() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

func queueWrite(n *Node, id string) {
	root := encoding.BASE64.Root()
	item := &domain.Item{Collection: "demo", Location: "aa", Id: id, Metrics: []float64{1}}
	n.queue.Add(NewElement(item, root))
}

func TestSoftLeaveHandsOffQueuedWrites(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, node, "owned")
	queueWrite(node, "queued")

	res := node.SoftLeave(10 * time.Second)
	if !res.OK {
		t.Fatalf("SoftLeave: %+v", res)
	}
	if got := node.Queue(); got != 0 {
		t.Fatalf("queued write stranded on a node about to terminate: queue=%d", got)
	}
	if got := boot.receivedItems(); got != 2 {
		t.Fatalf("bootstrap received %d items want 2 (owned + queued)", got)
	}
}

func TestSoftLeaveKeepsQueuedWriteWhenPeerRefuses(t *testing.T) {
	leaveEnv(t)
	boot := &refusingSink{dialPeer: newDialPeer("BootstrapAAAAAA", "10.0.0.1", 21000)}
	node := nodeWithBootstrap(t, boot, &memStorage{})
	queueWrite(node, "queued")

	res := node.SoftLeave(2 * time.Second)
	if res.OK {
		t.Fatalf("SoftLeave OK with an undrainable queue: %+v", res)
	}
	if got := node.Queue(); got != 1 {
		t.Fatalf("queued write lost: queue=%d want 1", got)
	}
	if node.leaving.Load() {
		t.Fatal("node stayed in leaving mode after a failed drain")
	}
}

func TestSoftLeaveSerializesConcurrentCalls(t *testing.T) {
	leaveEnv(t)
	boot := newOverlapSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})
	for _, id := range []string{"a", "b", "c"} {
		seedOwned(t, node, id)
	}

	results := make([]LeaveResult, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = node.SoftLeave(10 * time.Second)
		}(i)
	}
	wg.Wait()

	for i, res := range results {
		if !res.OK {
			t.Fatalf("SoftLeave %d: %+v", i, res)
		}
	}
	if got := boot.peakConcurrency(); got > 1 {
		t.Fatalf("%d drains transferred at once; leaves are not serialized", got)
	}
	if got := boot.receivedItems(); got != 3 {
		t.Fatalf("bootstrap received %d items want 3", got)
	}
	if got := ownedCount(t, node); got != 0 {
		t.Fatalf("leftover ownership after both drains: count=%d", got)
	}
}

type bouncingSink struct {
	*transferSink
	target  *Node
	bounced atomic.Int64
}

func (s *bouncingSink) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	ackedPeer, err := s.transferSink.Transfer(origin, key, items)
	if err != nil {
		return "", err
	}
	for _, item := range items {
		if s.target.New(item, key.Location, nil) == nil {
			s.bounced.Add(1)
		}
	}
	return ackedPeer, nil
}

func TestSoftLeaveConvergesWhenPeerPushesItemsBack(t *testing.T) {
	leaveEnv(t)
	sink := &bouncingSink{transferSink: newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)}
	node := nodeWithBootstrap(t, sink, &memStorage{})
	sink.target = node
	seedOwned(t, node, "x")

	start := time.Now()
	res := node.SoftLeave(5 * time.Second)

	if !res.OK {
		t.Fatalf("drain did not converge: %+v", res)
	}
	if got := sink.bounced.Load(); got != 0 {
		t.Fatalf("draining node took back %d items", got)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("drain burned its timeout instead of converging: %v", elapsed)
	}
}

func TestLeavingNodeDropsOutOfTheRing(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, node, "x")

	if res := node.SoftLeave(5 * time.Second); !res.OK {
		t.Fatalf("SoftLeave: %+v", res)
	}

	if _, err := node.Ping(boot); err == nil {
		t.Fatal("a draining node still answers Ping, so peers keep routing to it")
	}

	root := encoding.BASE64.Root()
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "late", Metrics: []float64{1}}
	if err := node.New(item, root, nil); err == nil {
		t.Fatal("a draining node accepted a write and re-owned the zone")
	}
	if got := ownedCount(t, node); got != 0 {
		t.Fatalf("zone re-owned during the drain: count=%d", got)
	}
}

func TestAutoscaleDownReleasesDrainLockWhenLeaveFails(t *testing.T) {
	leaveEnv(t)
	t.Setenv("INDEXUS_INSTANCE_ID", "local-test")
	t.Setenv("INDEXUS_LEAVE_TIMEOUT", "300ms")

	var mu sync.Mutex
	seen := map[string]int{}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path]++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer issuer.Close()

	count := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return seen[path]
	}

	dead := newDialPeer("DeadBootAAAAAAAA", "10.0.0.1", 0)
	node := nodeWithBootstrap(t, dead, &memStorage{})
	seedOwned(t, node, "x")

	node.EnableAutoscale(AutoscaleConfig{
		Enabled:       true,
		Role:          "spawned",
		IssuerURL:     issuer.URL,
		DownThreshold: 5,
		DownHold:      time.Nanosecond,
		Window:        2 * time.Second,
	})

	for i := 0; i < 6; i++ {
		node.autoscale.RecordInsert()
	}
	node.AutoscaleTick()
	deadlineHeat := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadlineHeat) {
		if node.autoscale.Snapshot()["inserts_window"].(int64) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	node.AutoscaleTick()
	node.AutoscaleTick()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && count("/v1/drain-unlock") == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	if count("/v1/drain-lock") == 0 {
		t.Fatal("drain never took the lock")
	}
	if count("/v1/drain-unlock") == 0 {
		t.Fatal("failed drain kept the cluster-wide lock")
	}
	if got := count("/v1/downscale"); got != 0 {
		t.Fatalf("terminated a node that still owns items: %d downscale calls", got)
	}
	if got := ownedCount(t, node); got != 1 {
		t.Fatalf("ownership lost by a failed drain: count=%d want 1", got)
	}
}

func TestSoftLeaveEmptyIsOK(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})

	res := node.SoftLeave(2 * time.Second)
	if !res.OK {
		t.Fatalf("empty soft-leave should OK: %+v", res)
	}
	if res.RemainingOwned != 0 || res.RemainingQueue != 0 {
		t.Fatalf("remaining: %+v", res)
	}
}

func TestSoftLeaveTransfersOwnedItems(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, node, "x")

	if got := ownedCount(t, node); got != 1 {
		t.Fatalf("precondition count=%d want 1", got)
	}

	res := node.SoftLeave(5 * time.Second)
	if !res.OK {
		t.Fatalf("SoftLeave: %+v", res)
	}
	if res.TransferredItems < 1 {
		t.Fatalf("TransferredItems=%d want >=1", res.TransferredItems)
	}
	if res.RemainingOwned != 0 {
		t.Fatalf("RemainingOwned=%d want 0", res.RemainingOwned)
	}
	if boot.receivedItems() < 1 {
		t.Fatal("bootstrap received no items")
	}
	if got := ownedCount(t, node); got != 0 {
		t.Fatalf("local count after leave=%d want 0", got)
	}
}

func TestSoftLeaveRestoresOnTransferFailure(t *testing.T) {
	leaveEnv(t)
	boot := &refusingSink{dialPeer: newDialPeer("BootstrapAAAAAA", "10.0.0.1", 21000)}
	node := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, node, "x")

	res := node.SoftLeave(400 * time.Millisecond)
	if res.OK {
		t.Fatalf("SoftLeave succeeded despite transfer failure: %+v", res)
	}
	if len(res.FailedKeys) == 0 {
		t.Fatal("expected FailedKeys after transfer error")
	}
	if got := ownedCount(t, node); got != 1 {
		t.Fatalf("items not restored locally: count=%d want 1", got)
	}
}

func TestSoftLeaveSkipsUndialablePeers(t *testing.T) {
	leaveEnv(t)
	dead := newDialPeer("DeadBootAAAAAAAA", "10.0.0.1", 0)
	node := nodeWithBootstrap(t, dead, &memStorage{})
	seedOwned(t, node, "x")

	res := node.SoftLeave(2 * time.Second)
	if res.OK {
		t.Fatalf("SoftLeave OK with only undialable peers: %+v", res)
	}
	if res.RemainingOwned == 0 {
		t.Fatal("ownership dropped with no live peer")
	}
	if got := ownedCount(t, node); got != 1 {
		t.Fatalf("count=%d want 1 still owned", got)
	}
}

func TestSoftLeaveSkipsLeavingPeers(t *testing.T) {
	leaveEnv(t)

	leaving := &leavingSink{dialPeer: newDialPeer("LeavingPeerAAAAA", "10.0.0.2", 21000)}
	good := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	settings, err := NewSettings(name, 21000, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	settings.SetAdvertise("127.0.0.1")
	node, err := NewNode(settings, func(n string, ips map[string]any, port int) domain.Contact {
		ip := "127.0.0.1"
		for k := range ips {
			ip = k
			break
		}
		return newDialPeer(n, ip, port)
	}, []domain.Contact{leaving, good}, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	seedOwned(t, node, "x")

	res := node.SoftLeave(5 * time.Second)
	if !res.OK {
		t.Fatalf("SoftLeave: %+v", res)
	}
	if got := leaving.calls.Load(); got == 0 {
		t.Fatal("leaving peer was never tried")
	}
	if got := good.receivedItems(); got < 1 {
		t.Fatalf("healthy peer received %d items want >=1", got)
	}
	if ownedCount(t, node) != 0 {
		t.Fatalf("leftover ownership: %d", ownedCount(t, node))
	}
}

type leavingSink struct {
	*dialPeer
	calls atomic.Int64
}

func (s *leavingSink) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	s.calls.Add(1)
	return "", domain.ErrLeaving
}

func TestSoftLeaveRetriesTransientFailure(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	boot.failOnce.Store(true)
	node := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, node, "x")

	res := node.SoftLeave(3 * time.Second)
	if !res.OK {
		t.Fatalf("SoftLeave should recover after one failure: %+v", res)
	}
	if got := boot.receivedItems(); got < 1 {
		t.Fatalf("bootstrap received %d items want >=1", got)
	}
	if ownedCount(t, node) != 0 {
		t.Fatalf("leftover ownership: %d", ownedCount(t, node))
	}
}

func TestTransferRefusedWhileLeaving(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)
	node := nodeWithBootstrap(t, boot, &memStorage{})

	res := node.SoftLeave(2 * time.Second)
	if !res.OK {
		t.Fatalf("SoftLeave: %+v", res)
	}

	_, err := node.Transfer(nil, domain.Key{Collection: "demo", Location: "a"}, []*domain.Item{
		{Collection: "demo", Location: "aa", Id: "z", Metrics: []float64{1}},
	})
	if err == nil {
		t.Fatal("Transfer accepted on a node that has left")
	}
}

func TestSoftLeaveConcurrentTwoNodes(t *testing.T) {
	leaveEnv(t)
	boot := newTransferSink("BootstrapAAAAAA", "10.0.0.1", 21000)

	a := nodeWithBootstrap(t, boot, &memStorage{})
	b := nodeWithBootstrap(t, boot, &memStorage{})
	seedOwned(t, a, "a1")
	seedOwned(t, b, "b1")

	var ra, rb LeaveResult
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ra = a.SoftLeave(5 * time.Second) }()
	go func() { defer wg.Done(); rb = b.SoftLeave(5 * time.Second) }()
	wg.Wait()

	if !ra.OK {
		t.Fatalf("node A SoftLeave: %+v", ra)
	}
	if !rb.OK {
		t.Fatalf("node B SoftLeave: %+v", rb)
	}
	if got := boot.receivedItems(); got < 2 {
		t.Fatalf("bootstrap received %d items want >=2", got)
	}
	if ownedCount(t, a) != 0 || ownedCount(t, b) != 0 {
		t.Fatalf("leftover ownership A=%d B=%d", ownedCount(t, a), ownedCount(t, b))
	}
}

func TestRestoreDelegatedKeepsTombstones(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	key := domain.Key{Collection: "demo", Location: root}

	live := &domain.Item{Collection: "demo", Location: "aa", Id: "alive", Metrics: []float64{1}}
	tomb := &domain.Item{Collection: "demo", Location: "aa", Id: "gone", Tombstone: true, Gen: 3}

	n.restoreDelegated(key, []*domain.Item{live, tomb})

	c, ok := n.collections.Get("demo")
	if !ok {
		t.Fatal("collection not created")
	}
	if !c.IsTombstoned("aa", "gone") {
		t.Fatal("tombstone restored as live leaf (or dropped)")
	}
	count, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("live count=%d want 1 (tombstone must not inflate)", count)
	}
}

type gatedSink struct {
	*transferSink
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
	entries atomic.Int64
}

func (s *gatedSink) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	s.entries.Add(1)
	s.once.Do(func() { close(s.entered) })
	<-s.gate
	return s.transferSink.Transfer(origin, key, items)
}

func newGatedNearestSink(t *testing.T, n *Node) *gatedSink {
	t.Helper()

	sink := &gatedSink{
		transferSink: newTransferSink("PeerAAAAAAAAAAAA", "10.0.0.1", 21001),
		entered:      make(chan struct{}),
		gate:         make(chan struct{}),
	}
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, encoding.BASE64.Root(), "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	sink.id = id
	n.register([]domain.Contact{sink})
	n.subscribe([]domain.Contact{sink})
	return sink
}

func TestRefreshSingleFlight(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	sink := newGatedNearestSink(t, n)

	first := make(chan error, 1)
	go func() { first <- n.Refresh() }()

	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first Refresh never attempted a transfer")
	}

	if err := n.Refresh(); err != nil {
		t.Fatalf("overlapping Refresh: %v", err)
	}
	if got := sink.entries.Load(); got != 1 {
		t.Fatalf("overlapping Refresh moved zones too: transfer entries=%d want 1", got)
	}

	close(sink.gate)
	if err := <-first; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := sink.receivedItems(); got != 1 {
		t.Fatalf("item delivered %d times, want exactly 1", got)
	}
	if owned := len(n.listOwnedKeys()); owned != 0 {
		t.Fatalf("ownership not dropped after ACK: owned=%d", owned)
	}
}

func TestSoftLeaveWaitsForInflightRefresh(t *testing.T) {
	leaveEnv(t)
	n := ownedNode(t, &memStorage{})
	sink := newGatedNearestSink(t, n)

	refreshDone := make(chan error, 1)
	go func() { refreshDone <- n.Refresh() }()

	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Refresh never attempted a transfer")
	}

	leaveDone := make(chan LeaveResult, 1)
	go func() { leaveDone <- n.SoftLeave(3 * time.Second) }()

	time.Sleep(150 * time.Millisecond)
	if got := sink.entries.Load(); got != 1 {
		t.Fatalf("drain moved zones while Refresh was mid-transfer: entries=%d", got)
	}

	close(sink.gate)
	if err := <-refreshDone; err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	res := <-leaveDone
	if !res.OK {
		t.Fatalf("SoftLeave after Refresh should succeed: %+v", res)
	}
	if got := sink.receivedItems(); got != 1 {
		t.Fatalf("item delivered %d times across Refresh+drain, want exactly 1", got)
	}
}

func TestTransferToPeerYieldsToLeaving(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	sink := newTransferSink("PeerAAAAAAAAAAAA", "10.0.0.1", 21001)
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, encoding.BASE64.Root(), "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	sink.id = id
	n.register([]domain.Contact{sink})
	n.subscribe([]domain.Contact{sink})

	plan := n.control()
	if len(plan) == 0 {
		t.Fatal("precondition: control() planned nothing")
	}

	n.leaving.Store(true)
	defer n.leaving.Store(false)

	for candidate, keys := range plan {
		if handed := n.transferToPeer(candidate, keys); handed != 0 {
			t.Fatalf("transferToPeer moved %d items while leaving", handed)
		}
	}
	if got := sink.calls.Load(); got != 0 {
		t.Fatalf("transfer attempted while leaving: calls=%d", got)
	}
	if owned := len(n.listOwnedKeys()); owned == 0 {
		t.Fatal("ownership dropped while leaving")
	}
}
