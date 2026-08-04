package core

import (
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

type getPeer struct {
	*dialPeer
	set   *domain.Set
	calls atomic.Int64
}

func (p *getPeer) Get(string, string, int) (domain.Contact, *domain.Set, error) {
	p.calls.Add(1)
	return p, p.set, nil
}

func ownerPeerAt(t *testing.T, n *Node, collection, location string, count int) *getPeer {
	t.Helper()
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, location, collection)
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	set := domain.NewSet()
	set.Put(location+":agg", domain.NewAbelian(count, nil))
	peer := &getPeer{dialPeer: newDialPeer("Owner"+location+"AAAAAAAAA", "10.0.0.8", 21008), set: set}
	peer.id = id
	n.register([]domain.Contact{peer})
	return peer
}

func TestGetPathFillsOnceThenServesFromCache(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := ownerPeerAt(t, n, "demo", "qr", 1)

	_, set, err := n.Get("demo", "qr", 2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil {
		t.Fatal("path-fill returned no set")
	}
	if got := owner.calls.Load(); got != 1 {
		t.Fatalf("owner dialed %d times on first Get, want 1", got)
	}
	if got := n.cache.Hops("demo", "qr"); got != 1 {
		t.Fatalf("sparse leaf stamped hops=%d want 1", got)
	}

	if _, set, err = n.Get("demo", "qr", 2); err != nil || set == nil {
		t.Fatalf("cached Get: set=%v err=%v", set != nil, err)
	}
	if got := owner.calls.Load(); got != 1 {
		t.Fatalf("cache hit still dialed the owner: calls=%d", got)
	}
}

func TestGetDenseLeafStampsShorterTTLClass(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	ownerPeerAt(t, n, "demo", "qq", n.settings.leafRedirect)

	if _, set, err := n.Get("demo", "qq", 2); err != nil || set == nil {
		t.Fatalf("Get: set=%v err=%v", set != nil, err)
	}
	if got := n.cache.Hops("demo", "qq"); got != 2 {
		t.Fatalf("dense leaf stamped hops=%d want 2 (1 hop + dense bump)", got)
	}
}

func TestGetDepthZeroRedirectsWithoutPull(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := ownerPeerAt(t, n, "demo", "qt", 1)

	contact, set, err := n.Get("demo", "qt", 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set != nil {
		t.Fatal("depth=0 returned a set — client asked for a redirect")
	}
	if contact == nil || contact.Name() != owner.Name() {
		t.Fatal("depth=0 did not redirect to the owner")
	}
	if owner.calls.Load() != 0 {
		t.Fatalf("depth=0 dialed the owner %d times", owner.calls.Load())
	}
	if cached, exist := n.cache.Get("demo", "qt"); !exist || cached != nil {
		t.Fatalf("expected a nil placeholder, got exist=%v set=%v", exist, cached != nil)
	}
}
