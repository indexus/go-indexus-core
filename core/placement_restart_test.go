package core

// Placement without a walk memo: every attempt climbs from item.Location.
// via names peers that already declined on this attempt so divergent
// membership tables cannot trade a write forever. On requeue via is cleared.

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

const (
	restartCol      = "DvFMV2020idx0001"
	donorName       = "_vSrV2020idx0001"
	childOwnerName  = "_x_rV2020idx0001"
	pinnedAddress   = "7xmD4zoqo5mppmvz"
	delegatedZone   = "7x"
	donorParentZone = "7"
)

type writeSink struct {
	*dialPeer
	news atomic.Int64
	dels atomic.Int64
	via  atomic.Value
}

func (s *writeSink) New(item *domain.Item, root string, via domain.Visited) error {
	s.news.Add(1)
	s.via.Store(via.String())
	return nil
}

func (s *writeSink) Delete(item *domain.Item, root string, via domain.Visited) error {
	s.dels.Add(1)
	s.via.Store(via.String())
	return nil
}

func donorPinnedAboveDelegatedChild(t *testing.T) (*Node, *writeSink, string) {
	t.Helper()

	settings, err := NewSettings(donorName, 21099, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	root := encoding.BASE64.Root()
	node.create(restartCol, root)
	collection, ok := node.collections.Get(restartCol)
	if !ok {
		t.Fatal("create did not open the collection")
	}
	collection.Own(donorParentZone, domain.Delegation{})
	collection.MarkDelegatedTo(root, donorParentZone, donorName)
	collection.MarkDelegatedTo(donorParentZone, delegatedZone, childOwnerName)

	self := peer.NewContact(donorName, map[string]any{"127.0.0.1": nil}, 21099)
	owner := &writeSink{dialPeer: newDialPeer(childOwnerName, "10.0.0.9", 21008)}
	node.register([]domain.Contact{self, owner})

	return node, owner, root
}

func TestAWriteUnderDelegatedChildForwardsFromTheAddress(t *testing.T) {
	node, owner, root := donorPinnedAboveDelegatedChild(t)

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "x", Metrics: []float64{1}}
	element := NewElement(item, root)

	if !node.feedOnce(element) {
		t.Fatal("placement from the address did not reach the child owner")
	}
	if got := owner.news.Load(); got != 1 {
		t.Fatalf("the child owner received %d writes, want 1", got)
	}
	if count, err := node.Count(); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v — the donor must not land an item it delegated away", count, err)
	}
}

func TestADeleteUnderDelegatedChildForwardsTheSameWay(t *testing.T) {
	node, owner, root := donorPinnedAboveDelegatedChild(t)

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "x"}
	element := NewDeleteElement(item, root)

	if !node.feedOnce(element) {
		t.Fatal("delete placement did not reach the child owner")
	}
	if got := owner.dels.Load(); got != 1 {
		t.Fatalf("the child owner received %d deletes, want 1", got)
	}
}

func TestAFailedAttemptUnderMarkParksWithEmptyVia(t *testing.T) {
	node, _, root := donorPinnedAboveDelegatedChild(t)
	node.reject([]domain.Contact{newDialPeer(childOwnerName, "10.0.0.9", 21008)})

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "x", Metrics: []float64{1}}
	element := NewElement(item, root)
	element.via = domain.Visited{"some-peer"}

	if node.feedOnce(element) {
		t.Fatal("a write landed although no node owns its zone")
	}
	if _, ok := node.queue.TryConsume(); ok {
		t.Fatal("blocked write stayed on the hot queue — it must park under the mark")
	}
	if got := node.parkedCount(); got != 1 {
		t.Fatalf("parked=%d, want 1", got)
	}
}

func TestAWriteUnderDelegatedChildParksWhenOwnerIsAbsent(t *testing.T) {
	node, _, root := donorPinnedAboveDelegatedChild(t)
	node.reject([]domain.Contact{newDialPeer(childOwnerName, "10.0.0.9", 21008)})

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "x", Metrics: []float64{1}}
	element := NewElement(item, root)

	if node.feedOnce(element) {
		t.Fatal("a write landed although no node owns its zone")
	}
	stats := node.IngressStats()
	if stats["parked"].(int) != 1 {
		t.Fatalf("parked=%v, want 1 — blocked writes garage under the mark", stats["parked"])
	}
	if stats["pending"].(int) != 0 {
		t.Fatalf("pending=%v, want 0 — parked items are off the hot queue", stats["pending"])
	}
}

func TestForwardCarriesViaNotAWalkPosition(t *testing.T) {
	node, owner, root := donorPinnedAboveDelegatedChild(t)

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "x", Metrics: []float64{1}}
	element := NewElement(item, root)

	if !node.feedOnce(element) {
		t.Fatal("expected forward to child owner")
	}
	got, _ := owner.via.Load().(string)
	if got == "" || got == pinnedAddress || got == donorParentZone {
		t.Fatalf("forward via=%q, want the donor name in the visited set", got)
	}
	if got != donorName {
		// via.String joins names; at least the donor must appear.
		if !domain.ParseVisited(got).Has(donorName) {
			t.Fatalf("forward via=%q does not include donor %q", got, donorName)
		}
	}
}

// When XOR still prefers the parent owner, a named mark must forward by name
// rather than park-and-wake thrash under verifyNamedMarks.
func TestWriteUnderMarkForwardsByNameWhenXorPointsHome(t *testing.T) {
	node, owner, root := donorPinnedAboveDelegatedChild(t)
	node.reject([]domain.Contact{owner})
	node.acknowledged.Insert(0, owner.ID(), owner)

	contact, err := node.find(restartCol, pinnedAddress)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if contact.Name() != donorName {
		t.Fatalf("precondition: XOR nearest=%q, want donor so the mark path is exercised", contact.Name())
	}

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "named-fwd", Metrics: []float64{1}}
	if !node.feedOnce(NewElement(item, root)) {
		t.Fatal("named mark holder is dialable — write must forward, not park")
	}
	if got := owner.news.Load(); got != 1 {
		t.Fatalf("named holder received %d writes, want 1", got)
	}
	if node.parkedCount() != 0 {
		t.Fatalf("parked=%d, want 0", node.parkedCount())
	}
}
