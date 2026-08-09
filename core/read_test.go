package core

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// Phases K (reads) and X (client ingress hint) — protocol.md §4.

// An owner keeps answering for its zone even when a nearer peer is registered:
// ownership, not XOR proximity, is the SoT verdict for a local read.
func TestGetServesLocalWhileOwnedEvenIfNearerPeer(t *testing.T) {
	donor := newNodeOn(t, &memStorage{}, 64)
	nearer := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	if err := donor.New(item("local"), root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, donor)

	owned := donor.listOwnedKeys()
	if len(owned) == 0 {
		t.Fatal("expected owned keys")
	}
	donor.subscribe([]domain.Contact{nearer})

	contact, set, err := donor.Get(owned[0].Collection, owned[0].Location, true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() == 0 {
		t.Fatalf("expected local set at %q", owned[0].Location)
	}
	if contact == nil || contact.Name() != donor.Name() {
		t.Fatalf("want self as contact while owned, got %v", contact)
	}
}

func countProps() []func(*domain.Abelian) int {
	return []func(*domain.Abelian) int{func(a *domain.Abelian) int { return a.Count() }}
}

// staleLocalSet plants a local copy the node does not own — what a donor keeps
// after a zone handoff. It must never short-circuit a deep client read.
func staleLocalSet(t *testing.T, n *Node, collection, location string, count int) {
	t.Helper()
	c := n.collections.Ensure(collection, encoding.BASE64.Root(), encoding.BASE64)
	c.EnsureSet(location)
	set, ok := c.Get(location)
	if !ok {
		t.Fatalf("EnsureSet(%s) did not create a set", location)
	}
	set.Put(location+":stale", domain.NewAbelian(count, nil))
	if n.ownsLocation(collection, location) {
		t.Fatalf("fixture broken: node owns %s/%s", collection, location)
	}
}

// ownZone makes the node the source of truth for location, with one leaf in it.
func ownZone(t *testing.T, n *Node, collection, location string, count int) *domain.Collection {
	t.Helper()
	n.create(collection, encoding.BASE64.Root())
	c, ok := n.collections.Get(collection)
	if !ok {
		t.Fatalf("missing collection %s", collection)
	}
	c.EnsureSet(location)
	c.Own(location, domain.Delegation{})
	n.owned.Upsert(0, zoneID(t, collection, location), map[domain.Key]any{}, func(_ int, _ []byte, m map[domain.Key]any) {
		m[domain.Key{Collection: collection, Location: location}] = struct{}{}
	})
	set, _ := c.Get(location)
	set.Put(location+":leaf", domain.NewAbelian(count, []float64{float64(count)}))
	return c
}

// --- K1/K3: path-fill and cache ---------------------------------------------

func TestGetPathFillsOnceThenServesFromCache(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 1))

	_, set, err := n.Get("demo", "qr", true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil {
		t.Fatal("path-fill returned no set")
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("owner dialed %d times on first Get, want 1", got)
	}
	if got := n.cache.Hops("demo", "qr"); got != 1 {
		t.Fatalf("sparse leaf stamped hops=%d want 1", got)
	}

	if _, set, err = n.Get("demo", "qr", true, nil, false); err != nil || set == nil {
		t.Fatalf("cached Get: set=%v err=%v", set != nil, err)
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("cache hit still dialed the owner: calls=%d", got)
	}
}

func TestGetDenseLeafStampsShorterTTLClass(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	zonePeer(t, n, "demo", "qq", setOf("qq:agg", n.settings.leafRedirect))

	if _, set, err := n.Get("demo", "qq", true, nil, false); err != nil || set == nil {
		t.Fatalf("Get: set=%v err=%v", set != nil, err)
	}
	if got := n.cache.Hops("demo", "qq"); got != 2 {
		t.Fatalf("dense leaf stamped hops=%d want 2 (1 hop + dense bump)", got)
	}
}

func TestGetDeepFalseRedirectsWithoutPull(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := zonePeer(t, n, "demo", "qt", setOf("qt:agg", 1))

	contact, set, err := n.Get("demo", "qt", false, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set != nil {
		t.Fatal("deep=false returned a set — client asked for a redirect")
	}
	if contact == nil || contact.Name() != owner.Name() {
		t.Fatal("deep=false did not redirect to the owner")
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("deep=false dialed the owner %d times", owner.gets.Load())
	}
	if cached, exist := n.cache.Get("demo", "qt"); !exist || cached != nil {
		t.Fatalf("expected a nil placeholder, got exist=%v set=%v", exist, cached != nil)
	}
}

// --- termination: the path, not a budget -------------------------------------

func TestGetCancelsWhenThePathReturnsToUs(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := zonePeer(t, n, "demo", "qu", setOf("qu:agg", 1))

	_, set, err := n.Get("demo", "qu", true, domain.Visited{"OtherAAAAAAAAAAAA", n.Name()}, false)
	if !errors.Is(err, errVisited) {
		t.Fatalf("a read closing on itself returned err=%v set=%v, want cancellation", err, set)
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("cancelled read still dialed the owner %d times", owner.gets.Load())
	}
}

func TestGetSkipsPeersAlreadyOnThePath(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := zonePeer(t, n, "demo", "qu", setOf("qu:agg", 1))

	_, set, err := n.Get("demo", "qu", true, domain.Visited{owner.Name()}, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set != nil {
		t.Fatalf("the only owner was already on the path, got %v", set)
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("dialed a peer already on the path %d times", owner.gets.Load())
	}
}

// Two nodes that only know each other each name the other XOR-nearest for every
// zone, so a read for a zone nobody owns is a routing cycle. Every node on the
// path answers once and the read ends there.
func TestDeepReadTerminatesOnATwoNodeRoutingCycle(t *testing.T) {
	n1 := newNodeOn(t, &memStorage{}, 64)
	n2 := newNodeOn(t, &memStorage{}, 64)
	n1.create("demo", encoding.BASE64.Root())
	n2.create("demo", encoding.BASE64.Root())
	onN1, onN2 := link(t, n1, n2)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, set, err := n1.Get("demo", "zz", true, nil, false); err != nil || set != nil {
			t.Errorf("read of an unowned zone returned set=%v err=%v, want a miss", set, err)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("deep read never came back — the cycle did not terminate")
	}

	if got := onN1.gets.Load(); got != 1 {
		t.Fatalf("n2 was asked %d times, want exactly 1", got)
	}
	if got := onN2.gets.Load(); got != 0 {
		t.Fatalf("the read bounced back to n1 %d times", got)
	}
}

// The forwarded path carries the caller, so the peer can refuse to route back.
func TestGetForwardsThePathItWalked(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := zonePeer(t, n, "demo", "qu", setOf("qu:agg", 1))

	if _, _, err := n.Get("demo", "qu", true, nil, false); err != nil {
		t.Fatalf("Get: %v", err)
	}
	via := owner.lastVia()
	if len(via) != 1 || via[0] != n.Name() {
		t.Fatalf("owner received via=%v, want the caller alone", via)
	}
}

// --- K4: a stale local copy is a fallback, never an answer -------------------

func TestGetPrefersOwnerOverStaleLocalCopy(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 1)
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 7))

	_, set, err := n.Get("demo", "qr", true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil {
		t.Fatal("deep Get returned no set")
	}
	if owner.gets.Load() != 1 {
		t.Fatalf("owner dialed %d times, want 1", owner.gets.Load())
	}
	if set.Count() != 7 {
		t.Fatalf("served count=%d, want the owner copy (7)", set.Count())
	}
}

func TestGetFallsBackToStaleLocalCopyWhenNoPeerAnswers(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 3)

	_, set, err := n.Get("demo", "qr", true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() != 3 {
		t.Fatalf("expected the local copy as fallback, got %v", set)
	}
}

func TestGetServesStaleLocalCopyWhenNotDeep(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 2)
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 7))

	_, set, err := n.Get("demo", "qr", false, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() != 2 {
		t.Fatalf("shallow Get should stay local, got %v", set)
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("shallow Get dialed owner %d times", owner.gets.Load())
	}
}

// A stale copy is served under the owner's name, so the client re-routes on its
// next call instead of pinning itself to a node that no longer owns the zone.
func TestServeStaleCreditsTheOwnerNotUs(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 3)
	owner := zonePeer(t, n, "demo", "qr", nil)

	contact, set, err := n.Get("demo", "qr", true, nil, false)
	if err != nil || set == nil {
		t.Fatalf("Get: set=%v err=%v", set != nil, err)
	}
	if contact == nil || contact.Name() != owner.Name() {
		t.Fatalf("stale answer credited %v, want the owner", contact)
	}
}

// --- K2: a refresh reads the source of truth and leaves the LRU alone --------

func TestRefreshBypassesLocalCacheAndPullsOwner(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:new", 9))

	n.cache.SetHops("demo", "qr", setOf("qr:old", 1), 1)
	_, clientSet, err := n.Get("demo", "qr", false, nil, false)
	if err != nil || clientSet == nil {
		t.Fatalf("client cache hit: set=%v err=%v", clientSet != nil, err)
	}
	if _, ok := clientSet.Get("qr:old"); !ok {
		t.Fatal("client should see the stale cache entry")
	}
	if owner.gets.Load() != 0 {
		t.Fatal("client cache hit dialed the peer")
	}

	_, got, err := n.Get("demo", "qr", true, nil, true)
	if err != nil {
		t.Fatalf("refresh Get: %v", err)
	}
	if got == nil || got.Count() != 9 {
		t.Fatalf("refresh set=%v want the owner's 9", got)
	}
	if owner.refreshs.Load() != 1 {
		t.Fatalf("owner refresh calls=%d want 1", owner.refreshs.Load())
	}
}

func TestRefreshDoesNotTouchLRU(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")

	n.cache.SetHops("demo", "cold", setOf("b", 1), 1)
	n.cache.SetHops("demo", "hot", setOf("a", 1), 1)
	n.cache.Get("demo", "hot")
	before := n.cache.LastTouch("demo", "cold")

	zonePeer(t, n, "demo", "zz", setOf("zz:x", 3))
	_, _, _ = n.Get("demo", "zz", true, nil, true)
	if after := n.cache.LastTouch("demo", "cold"); !after.Equal(before) {
		t.Fatalf("refresh touched an unrelated LRU entry: before=%v after=%v", before, after)
	}
}

// Serving a refresh from the source of truth must not touch a cache entry that
// happens to sit on the same key, while a plain client read must.
func TestRefreshOnOwnedZoneLeavesCacheEntryCold(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	ownZone(t, n, "demo", "aa", 2)

	n.cache.SetHops("demo", "aa", setOf("aa:stale", 1), 1)
	before := n.cache.LastTouch("demo", "aa")

	time.Sleep(2 * time.Millisecond)
	_, set, err := n.Get("demo", "aa", true, nil, true)
	if err != nil || set == nil {
		t.Fatalf("owner refresh: %v set=%v", err, set != nil)
	}
	if _, ok := set.Get("aa:leaf"); !ok {
		t.Fatal("refresh should return the ownership SoT, not the cache")
	}
	if after := n.cache.LastTouch("demo", "aa"); !after.Equal(before) {
		t.Fatalf("refresh touched the owner's LRU: before=%v after=%v", before, after)
	}

	n.cache.SetHops("demo", "bb", setOf("bb:x", 1), 1)
	cold := n.cache.LastTouch("demo", "bb")
	time.Sleep(2 * time.Millisecond)
	_, _, _ = n.Get("demo", "bb", false, nil, false)
	if !n.cache.LastTouch("demo", "bb").After(cold) {
		t.Fatal("a client Get must touch the LRU for a cache-only key")
	}
}

func TestUpdateRefreshWritesLocalCache(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	n.settings.SetUpdateEta(0)
	n.settings.updateBatch = false
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:v2", 5))

	n.cache.SetHops("demo", "qr", setOf("qr:v1", 1), 1)
	n.cache.Get("demo", "qr") // touch, so Refresh proposes a pull

	if err := n.Update(); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if owner.refreshs.Load() < 1 {
		t.Fatalf("Update did not refresh-pull the owner: refresh=%d gets=%d",
			owner.refreshs.Load(), owner.gets.Load())
	}
	got, ok := n.cache.Peek("demo", "qr")
	if !ok || got == nil {
		t.Fatal("local cache missing after Update")
	}
	if _, ok := got.Get("qr:v2"); !ok {
		t.Fatalf("local cache not updated to the SoT: %#v", got)
	}
}

// --- K6: the fan-out of a miss is bounded ------------------------------------

func registerReadPeers(t *testing.T, n *Node, count int) []*readPeer {
	t.Helper()
	peers := make([]*readPeer, 0, count)
	for i := 0; i < count; i++ {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		p := &readPeer{dialPeer: newDialPeer(name, "10.0.0.1", 21000+i)}
		peers = append(peers, p)
		n.register([]domain.Contact{p})
	}
	return peers
}

func totalGets(peers []*readPeer) int64 {
	total := int64(0)
	for _, p := range peers {
		total += p.gets.Load()
	}
	return total
}

// A miss must not walk the mesh. Ordering candidates by XOR distance to the
// zone key is what allows the cap: the peers most likely to own it come first.
func TestClientReadFanoutIsBounded(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	peers := registerReadPeers(t, n, 12)

	if _, _, err := n.Get("demo", "ab", true, nil, false); err != nil {
		t.Fatalf("Get: %v", err)
	}

	total := totalGets(peers)
	if total == 0 {
		t.Fatal("no peer was asked at all")
	}
	// The XOR nearest is asked first, then at most zoneFanout others.
	if total > int64(zoneFanout+1) {
		t.Fatalf("client read asked %d peers, cap is %d", total, zoneFanout+1)
	}
}

func TestRefreshReadAsksOneExtraPeerAtMost(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	peers := registerReadPeers(t, n, 12)

	if _, _, err := n.Get("demo", "ab", true, nil, true); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if total := totalGets(peers); total > 2 {
		t.Fatalf("refresh read asked %d peers, cap is 2", total)
	}
}

func TestTryPeersGetRefreshCapsOnePeer(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	peers := registerReadPeers(t, n, 4)
	zonePeer(t, n, "demo", "zz", nil)

	if _, _ = n.tryPeersGet("demo", "zz", nil, true, nil); totalGets(peers) > 1 {
		t.Fatalf("refresh tryPeersGet asked %d peers, want at most 1", totalGets(peers))
	}
}

// While a zone is moving, a refresh pull would race the handoff for no gain.
func TestTryPeersGetIsOffWhileTransferBusy(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	peers := registerReadPeers(t, n, 4)
	n.rebalancing.Store(true)

	if set, _ := n.tryPeersGet("demo", "zz", nil, true, nil); set != nil {
		t.Fatal("refresh pulled while a transfer was in flight")
	}
	if got := totalGets(peers); got != 0 {
		t.Fatalf("refresh dialed %d peers while TransferBusy", got)
	}
}

// The XOR-nearest peer is a hint, not the source of truth: PreferNear names
// drift from data prefixes, so the nearest node routinely holds nothing. An
// empty answer from it is a miss to route around, never an empty truth to
// serve or to cache.
func TestEmptyAnswerFromNearestIsAMissNotATruth(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")

	nearest := zonePeer(t, n, "demo", "ab", domain.NewSet())
	holder := &readPeer{dialPeer: newDialPeer("HolderAAAAAAAAAAAA", "10.0.0.3", 21003), set: setOf("ab:agg", 12)}
	n.register([]domain.Contact{holder})

	_, set, err := n.Get("demo", "ab", true, nil, false)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() != 12 {
		t.Fatalf("read stopped at the empty nearest answer: %v", set)
	}
	if nearest.gets.Load() == 0 {
		t.Fatal("fixture broken: the nearest peer was never asked")
	}
	if cached, ok := n.cache.Peek("demo", "ab"); !ok || cached == nil || cached.Count() != 12 {
		t.Fatalf("cache holds %v, want the answer that carried data", cached)
	}
}

func TestZoneCandidatesOrderedByDistanceToKey(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	registerReadPeers(t, n, 12)
	id := zoneID(t, "demo", "ab")

	all := n.zoneCandidates("demo", "ab", 100, nil)
	if len(all) != 12 {
		t.Fatalf("candidates=%d want 12", len(all))
	}
	for i := 1; i < len(all); i++ {
		if bytes.Compare(xorDistance(all[i-1].ID(), id), xorDistance(all[i].ID(), id)) > 0 {
			t.Fatalf("candidate %d is closer to the key than candidate %d", i, i-1)
		}
	}

	capped := n.zoneCandidates("demo", "ab", zoneFanout, nil)
	if len(capped) != zoneFanout {
		t.Fatalf("capped=%d want %d", len(capped), zoneFanout)
	}
	for i, c := range capped {
		if c.Name() != all[i].Name() {
			t.Fatalf("cap dropped the nearest peers: got %s want %s", c.Name(), all[i].Name())
		}
	}

	if got := n.zoneCandidates("demo", "ab", zoneFanout, nil, all[0]); got[0].Name() == all[0].Name() {
		t.Fatal("skip was not honoured")
	}

	// A peer already on the read's path is dropped for the same reason as skip.
	onPath := domain.Visited{all[0].Name(), all[1].Name()}
	for _, c := range n.zoneCandidates("demo", "ab", zoneFanout, onPath) {
		if onPath.Has(c.Name()) {
			t.Fatalf("%s is on the path and was offered as a candidate", c.Name())
		}
	}
}

// A quarantined peer is not a candidate: it answers, but a suspension means the
// mesh stopped believing what it says about ownership.
func TestZoneCandidatesSkipSuspendedPeers(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	peers := registerReadPeers(t, n, 4)
	for _, p := range peers {
		n.suspend(p.Name(), time.Minute)
	}
	if got := n.zoneCandidates("demo", "ab", zoneFanout, nil); len(got) != 0 {
		t.Fatalf("suspended peers offered as candidates: %d", len(got))
	}
}

// --- GetMultiple -------------------------------------------------------------

func TestGetMultiplePathFillsAndCaches(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 3))

	body, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, false, false)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("deep GetMultiple returned empty binary")
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("owner dialed %d times, want 1", got)
	}
	if cached, ok := n.cache.Get("demo", "qr"); !ok || cached == nil {
		t.Fatal("path-fill did not populate the LRU")
	}

	body2, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, false, false)
	if err != nil || len(body2) == 0 {
		t.Fatalf("cached GetMultiple: len=%d err=%v", len(body2), err)
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("cache hit still dialed the owner: calls=%d", got)
	}
}

// A batch read is the only way an aggregate client reads. Dropping refresh here
// left it pinned to whatever this node cached first: the zone could move, the
// owner could double its count, and the client would keep serving the old
// number with no way to ask for a newer one.
func TestGetMultipleRefreshReachesPastTheCache(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 3))

	if _, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, false, false); err != nil {
		t.Fatalf("warm GetMultiple: %v", err)
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("owner dialed %d times warming the cache, want 1", got)
	}

	// Without refresh the cache answers and the owner is never asked again.
	if _, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, false, false); err != nil {
		t.Fatalf("cached GetMultiple: %v", err)
	}
	if got := owner.gets.Load(); got != 1 {
		t.Fatalf("a non-refresh batch bypassed the cache: calls=%d", got)
	}

	if _, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, true, false); err != nil {
		t.Fatalf("refresh GetMultiple: %v", err)
	}
	if got := owner.gets.Load(); got != 2 {
		t.Fatalf("refresh did not re-ask the owner: calls=%d, want 2", got)
	}
}

func TestGetMultipleDeepFalseSkipsMiss(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := zonePeer(t, n, "demo", "qs", setOf("qs:agg", 2))

	body, err := n.GetMultiple("demo", []string{"qs"}, 6, countProps(), false, nil, false, false)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("deep=false should omit a miss, got %d bytes", len(body))
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("deep=false dialed the owner %d times", owner.gets.Load())
	}
}

func TestGetMultiplePrefersOwnerOverStaleLocalCopy(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 1)
	owner := zonePeer(t, n, "demo", "qr", setOf("qr:agg", 7))

	if _, err := n.GetMultiple("demo", []string{"qr"}, 6, countProps(), true, nil, false, false); err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if owner.gets.Load() != 1 {
		t.Fatalf("GetMultiple dialed the owner %d times, want 1", owner.gets.Load())
	}
}

// A cold batch resolves in parallel: sequential path-fill makes latency linear
// in the batch size, which is what a map view of 96 tiles produces.
func TestGetMultipleResolvesBatchInParallel(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")

	var inFlight, peak atomic.Int64
	locations := make([]string, 0, batchParallelism*2)
	for i := 0; i < batchParallelism*2; i++ {
		loc := "q" + encoding.BASE64.CharAt(i)
		locations = append(locations, loc)
		p := &slowPeer{readPeer: atZone(t, "demo", loc, setOf(loc+":agg", 1)), inFlight: &inFlight, peak: &peak}
		n.register([]domain.Contact{p})
	}

	if _, err := n.GetMultiple("demo", locations, 6, countProps(), true, nil, false, false); err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if peak.Load() < 2 {
		t.Fatalf("batch resolved serially: peak concurrency=%d", peak.Load())
	}
	if peak.Load() > int64(batchParallelism) {
		t.Fatalf("batch opened %d concurrent reads, cap is %d", peak.Load(), batchParallelism)
	}
}

type slowPeer struct {
	*readPeer
	inFlight *atomic.Int64
	peak     *atomic.Int64
}

func (p *slowPeer) Get(collection, location string, deep bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {
	cur := p.inFlight.Add(1)
	for {
		old := p.peak.Load()
		if cur <= old || p.peak.CompareAndSwap(old, cur) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	p.inFlight.Add(-1)
	return p.readPeer.Get(collection, location, deep, via, refresh)
}

// --- /aggregates: answering is a claim of ownership --------------------------

func TestGetAggregatesServesOwnedLocationsOnly(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	ownZone(t, n, "demo", "bb", 5)

	aggs, err := n.GetAggregates("demo", []string{"bb", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if aggs["bb"] == nil || aggs["bb"].Count() != 5 {
		t.Fatalf("owned aggregate=%v want 5", aggs["bb"])
	}
	if _, ok := aggs["missing"]; ok {
		t.Fatal("a location with no data was answered")
	}
}

// Holding a copy is not owning it. A set left behind by a handoff has the shape
// of an answer and none of the authority — and that distinction is the whole
// reason a parent stub can trust this route.
func TestGetAggregatesRefusesALocalCopyItDoesNotOwn(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	staleLocalSet(t, n, "demo", "qr", 3)

	aggs, err := n.GetAggregates("demo", []string{"qr"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := aggs["qr"]; ok {
		t.Fatal("a local copy this node does not own was served as authoritative")
	}
}

// A donor that handed a zone over must fall silent on it, even though it still
// owns the parent above it: answering would keep certifying a value it no
// longer holds, and freeze the parent stub on the pre-handoff number. It keeps
// answering for the parent, which it does still own.
func TestGetAggregatesFallsSilentOnADelegatedChild(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	c := ownZone(t, n, "demo", "bb", 5)
	root, _ := c.Get(encoding.BASE64.Root())
	root.Put("bb", domain.NewAbelian(5, []float64{5}))

	c.Delegate("bb")
	n.removeOwnedKey(domain.Key{Collection: "demo", Location: "bb"})

	aggs, err := n.GetAggregates("demo", []string{"bb", encoding.BASE64.Root()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := aggs["bb"]; ok {
		t.Fatal("a former owner still certified the zone it handed off")
	}
	if _, ok := aggs[encoding.BASE64.Root()]; !ok {
		t.Fatal("the donor stopped answering for the parent it still owns")
	}
}

// --- X: the client ingress hint ----------------------------------------------

func TestRoutingHintReturnsCloserPeer(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)

	// A peer ID closer to the all-zero key than any random self name.
	peerID, err := encoding.BASE64.Decode("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	other := newDialPeer("AAAAAAAAAAAAAAAAAAAAAA", "10.0.0.2", 21002)
	other.id = peerID
	n.registered.Insert(0, other.ID(), other)

	hint := n.RoutingHint(make([]byte, len(peerID)))
	if hint == nil {
		t.Fatal("expected a routing hint toward the zero-key peer")
	}
	if hint.Name() != other.Name() {
		t.Fatalf("hint name=%s want=%s", hint.Name(), other.Name())
	}
}

func TestRoutingHintNilWhenSelfNearest(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.registered.Insert(0, n.ID(), n)

	if hint := n.RoutingHint(append([]byte(nil), n.ID()...)); hint != nil {
		t.Fatalf("expected nil when self is nearest, got %s", hint.Name())
	}
}

func TestRoutingHintNilEmptyKey(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	if hint := n.RoutingHint(nil); hint != nil {
		t.Fatal("expected nil for an empty key")
	}
}

func TestGetMultipleEnvelopeRedirectsWithoutPull(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := zonePeer(t, n, "demo", "qs", setOf("qs:agg", 2))

	body, err := n.GetMultiple("demo", []string{"qs"}, 6, countProps(), false, nil, false, true)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	redirects, inner, ok, err := domain.DecodeSetsEnvelope(body)
	if err != nil || !ok {
		t.Fatalf("envelope: ok=%v err=%v", ok, err)
	}
	if len(inner) != 0 {
		t.Fatalf("deep=false miss should carry empty sets body, got %d bytes", len(inner))
	}
	if len(redirects) != 1 || redirects[0].Location != "qs" || redirects[0].Name != owner.Name() {
		t.Fatalf("redirects=%+v want qs→%s", redirects, owner.Name())
	}
	if owner.gets.Load() != 0 {
		t.Fatalf("envelope deep=false dialed the owner %d times", owner.gets.Load())
	}
}

func TestGetMultipleEnvelopeAbsentKeepsLegacyBody(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	_ = zonePeer(t, n, "demo", "qs", setOf("qs:agg", 2))

	body, err := n.GetMultiple("demo", []string{"qs"}, 6, countProps(), false, nil, false, false)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if domain.IsSetsEnvelope(body) {
		t.Fatal("envelope=false must not wrap the legacy EncodeSets body")
	}
}
