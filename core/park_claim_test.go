package core

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

// Park + Claim: a write under a named mark is garaged, not absorbed; a Claim
// from the child owner wakes the garage so placement retries from the address.
func TestParkedWriteWakesOnClaim(t *testing.T) {
	settings, err := NewSettings(donorName, 21101, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	donor, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	root := encoding.BASE64.Root()
	donor.create(restartCol, root)
	collection, ok := donor.collections.Get(restartCol)
	if !ok {
		t.Fatal("missing collection")
	}
	collection.Own(donorParentZone, domain.Delegation{})
	collection.MarkDelegatedTo(donorParentZone, delegatedZone, childOwnerName)

	self := peer.NewContact(donorName, map[string]any{"127.0.0.1": nil}, 21101)
	donor.register([]domain.Contact{self})

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "park-1", Metrics: []float64{1}}
	element := NewElement(item, root)
	if donor.feedOnce(element) {
		t.Fatal("write under named mark must not land on the donor")
	}
	if donor.parkedCount() != 1 {
		t.Fatalf("parked=%d, want 1", donor.parkedCount())
	}

	origin := newDialPeer(childOwnerName, "10.0.0.9", 21008)
	if err := donor.Claim(origin, domain.ClaimPayload{
		Collection: restartCol,
		Parent:     donorParentZone,
		Child:      delegatedZone,
		Peer:       childOwnerName,
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	if donor.parkedCount() != 0 {
		t.Fatalf("parked=%d after Claim, want 0", donor.parkedCount())
	}
	pending := donor.queue.Snapshot()
	if len(pending) != 1 {
		t.Fatalf("queue pending=%d after wake, want 1", len(pending))
	}
	if len(pending[0].via) != 0 {
		t.Fatalf("woken element via=%v, want empty", pending[0].via)
	}

	handoff, marked := collection.DelegationOf(donorParentZone, delegatedZone)
	if !marked || handoff.Peer != childOwnerName {
		t.Fatalf("mark after Claim = %+v marked=%v, want peer %s", handoff, marked, childOwnerName)
	}
}

func TestClaimCannotNameAnotherPeer(t *testing.T) {
	settings, err := NewSettings(donorName, 21105, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	root := encoding.BASE64.Root()
	node.create(restartCol, root)
	collection, _ := node.collections.Get(restartCol)
	collection.Own(donorParentZone, domain.Delegation{})

	origin := newDialPeer(childOwnerName, "10.0.0.9", 21008)
	err = node.Claim(origin, domain.ClaimPayload{
		Collection: restartCol,
		Parent:     donorParentZone,
		Child:      delegatedZone,
		Peer:       "_uSrV2020idx0001",
	})
	if err == nil {
		t.Fatal("spoofed Claim was accepted")
	}
	if _, marked := collection.DelegationOf(donorParentZone, delegatedZone); marked {
		t.Fatal("spoofed Claim planted an ownership mark")
	}
}

func TestAnonymousMarkNeverBlocksProgress(t *testing.T) {
	settings, err := NewSettings(donorName, 21102, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	donor, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	root := encoding.BASE64.Root()
	donor.create(restartCol, root)
	collection, ok := donor.collections.Get(restartCol)
	if !ok {
		t.Fatal("missing collection")
	}
	collection.Own(donorParentZone, domain.Delegation{})
	collection.MarkDelegated(donorParentZone, delegatedZone) // Peer ""
	collection.NoteDelegatedHandoff(delegatedZone, domain.Handoff{})

	self := peer.NewContact(donorName, map[string]any{"127.0.0.1": nil}, 21102)
	donor.register([]domain.Contact{self})

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "anon-1", Metrics: []float64{1}}
	if !donor.feedOnce(NewElement(item, root)) {
		t.Fatal("anonymous legacy mark blocked a write")
	}
	if donor.parkedCount() != 0 {
		t.Fatalf("parked=%d, want 0 under anonymous mark", donor.parkedCount())
	}

	// It remains an unresolved topology hint until Claim names it.
	donor.repair()
	if collection.IsDelegated(donorParentZone, delegatedZone) {
		t.Fatal("anonymous mark must not be treated as authoritative delegation")
	}
}

func TestReclaimParkedWhenNamedPeerEvicted(t *testing.T) {
	settings, err := NewSettings(donorName, 21106, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	donor, err := NewNode(settings, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	root := encoding.BASE64.Root()
	donor.create(restartCol, root)
	collection, ok := donor.collections.Get(restartCol)
	if !ok {
		t.Fatal("missing collection")
	}
	collection.Own(donorParentZone, domain.Delegation{})
	collection.MarkDelegatedTo(donorParentZone, delegatedZone, childOwnerName)

	self := peer.NewContact(donorName, map[string]any{"127.0.0.1": nil}, 21106)
	donor.register([]domain.Contact{self})

	item := &domain.Item{Collection: restartCol, Location: pinnedAddress, Id: "evict-1", Metrics: []float64{1}}
	if donor.feedOnce(NewElement(item, root)) {
		t.Fatal("write under named mark must park")
	}
	if donor.parkedCount() != 1 {
		t.Fatalf("parked=%d, want 1", donor.parkedCount())
	}

	// Quarantine then expire it and drop the peer from every membership table.
	// That is the post-eviction hole: suspended is false, lookup is nil.
	donor.suspend(childOwnerName, time.Millisecond)
	time.Sleep(2 * time.Millisecond)
	if donor.suspended(childOwnerName) {
		t.Fatal("quarantine should have expired")
	}
	if !donor.ghostedPeer(childOwnerName) {
		t.Fatal("expired quarantine must leave a ghost for reclaim")
	}

	donor.verifyNamedMarks()

	if donor.parkedCount() != 0 {
		t.Fatalf("parked=%d after eviction reclaim, want 0", donor.parkedCount())
	}
	if !collection.Owns(delegatedZone) {
		t.Fatal("child must be owned locally after eviction reclaim")
	}
}

func TestPushClaimNamesMarkOnLocalParent(t *testing.T) {
	settingsA, err := NewSettings(donorName, 21103, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings A: %v", err)
	}
	donor, err := NewNode(settingsA, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode A: %v", err)
	}
	settingsB, err := NewSettings(childOwnerName, 21104, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings B: %v", err)
	}
	child, err := NewNode(settingsB, peer.NewContact, nil, &memStorage{})
	if err != nil {
		t.Fatalf("NewNode B: %v", err)
	}

	root := encoding.BASE64.Root()
	donor.create(restartCol, root)
	child.create(restartCol, root)
	donorColl, _ := donor.collections.Get(restartCol)
	childColl, _ := child.collections.Get(restartCol)
	donorColl.Own(donorParentZone, domain.Delegation{})
	donor.own(donorColl, domain.Ownership{donorParentZone: domain.Delegation{}})
	child.own(childColl, domain.Ownership{delegatedZone: domain.Delegation{}})

	link(t, donor, child)
	child.repair()

	handoff, marked := donorColl.DelegationOf(donorParentZone, delegatedZone)
	if !marked {
		t.Fatal("Claim from child did not plant a mark on the parent holder")
	}
	if handoff.Peer != childOwnerName {
		t.Fatalf("named mark peer=%q, want %s", handoff.Peer, childOwnerName)
	}
}
