package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

// transferItems mirrors a real handoff: the zone root is "a" and the items sit
// below it.
func transferItems() []*domain.Item {
	return []*domain.Item{
		{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}},
		{Collection: "demo", Location: "ab", Id: "y", Metrics: []float64{1}},
	}
}

var transferKey = domain.Key{Collection: "demo", Location: "a"}

// The sender removes its own copy as soon as a transfer is acknowledged, so a
// receiver that cannot enqueue has to say so. Reporting success here is how
// items disappear during a rebalance or a drain.
func TestTransferFailsWhenQueueIsFull(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)
	node.settings.SetQueueMax(1)

	if err := node.Transfer(nil, transferKey, transferItems()); err == nil {
		t.Fatal("Transfer acknowledged items it could not enqueue")
	}
}

func TestTransferAcceptsItemsWhenQueueHasRoom(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	if err := node.Transfer(nil, transferKey, transferItems()); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	drain(t, node)

	count, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count after transfer: got %d want 2", count)
	}
}
