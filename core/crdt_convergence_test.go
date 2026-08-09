package core

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func TestTransferDeliveredTwiceIsIdempotent(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	batch := func() []*domain.Item {
		return []*domain.Item{
			{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}},
			{Collection: "demo", Location: "ab", Id: "y", Metrics: []float64{2}},
		}
	}
	key := domain.Key{Collection: "demo", Location: "a"}

	if _, err := node.Transfer(nil, key, batch()); err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if _, err := node.Transfer(nil, key, batch()); err != nil {
		t.Fatalf("second delivery: %v", err)
	}
	drain(t, node)

	count, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Fatalf("duplicate delivery changed state: count=%d want 2", count)
	}
}

func TestStaleSnapshotDuplicateOwnerHeals(t *testing.T) {
	storage := &memStorage{}
	donor := newNodeOn(t, storage, 64)
	root := encoding.BASE64.Root()

	const items = 3
	for i := 0; i < items; i++ {
		it := &domain.Item{Collection: "demo", Location: "aa", Id: fmt.Sprintf("x%d", i), Metrics: []float64{1}}
		if err := donor.New(it, root, nil); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
	drain(t, donor)
	if err := donor.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	receiver := newNodeOn(t, &memStorage{}, 64)
	if handed := donor.transferToPeer(receiver, donor.listOwnedKeys()); handed != items {
		t.Fatalf("first handoff moved %d items want %d", handed, items)
	}
	drain(t, receiver)
	if got, _ := receiver.Count(); got != items {
		t.Fatalf("receiver after handoff: count=%d want %d", got, items)
	}
	if owned := len(donor.listOwnedKeys()); owned != 0 {
		t.Fatalf("donor kept ownership after ACK: %d", owned)
	}

	revenant := newNodeOn(t, storage, 64)
	if got, _ := revenant.Count(); got != items {
		t.Fatalf("revenant did not re-own the stale items: count=%d want %d", got, items)
	}

	if handed := revenant.transferToPeer(receiver, revenant.listOwnedKeys()); handed != items {
		t.Fatalf("healing handoff moved %d items want %d", handed, items)
	}
	drain(t, receiver)
	if got, _ := receiver.Count(); got != items {
		t.Fatalf("duplicates not absorbed: count=%d want %d", got, items)
	}
	if owned := len(revenant.listOwnedKeys()); owned != 0 {
		t.Fatalf("revenant kept ownership: %d", owned)
	}
}

func TestTransferredTombstoneThenLateAddResurrects(t *testing.T) {
	root := encoding.BASE64.Root()

	donor := newNodeOn(t, &memStorage{}, 64)
	if err := donor.New(item("x"), root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, donor)
	if err := donor.Delete(item("x"), root, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	drain(t, donor)

	col, ok := donor.collections.Get("demo")
	if !ok {
		t.Fatal("donor lost its collection")
	}
	if !col.IsTombstoned("aa", "x") {
		t.Fatal("precondition: delete left no tombstone")
	}

	batch, _ := col.Delegate(root)
	receiver := newNodeOn(t, &memStorage{}, 64)
	if _, err := receiver.Transfer(donor, domain.Key{Collection: "demo", Location: root}, batch); err != nil {
		t.Fatalf("Transfer: %v", err)
	}

	rcol, ok := receiver.collections.Get("demo")
	if !ok {
		t.Fatal("receiver did not create the collection for the tombstone")
	}
	if !rcol.IsTombstoned("aa", "x") {
		t.Fatal("tombstone did not survive the transfer")
	}

	if err := receiver.Handoff(item("x"), root, nil); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	drain(t, receiver)

	if got, _ := receiver.Count(); got != 1 {
		t.Fatalf("add-wins semantics changed: count=%d want 1 (resurrected)", got)
	}
	if rcol.IsTombstoned("aa", "x") {
		t.Fatal("id resurrected but tombstone still active")
	}
}

func TestRefusedWriteStillReplaysFromWAL(t *testing.T) {
	storage := &memStorage{}
	node := newNodeOn(t, storage, 64)
	node.settings.SetQueueMax(1)

	if err := node.New(item("accepted"), "@", nil); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := node.New(item("refused"), "@", nil); err != ErrQueueFull {
		t.Fatalf("second write: err=%v want ErrQueueFull", err)
	}

	restarted := newNodeOn(t, storage, 64)
	drain(t, restarted)

	count, err := restarted.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Fatalf("replay applied %d items; the refused write is expected to replay too (want 2)", count)
	}
}
