package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// ownedNode returns a node that owns one location, with the queue drained.
func ownedNode(t *testing.T, storage domain.Storage) *Node {
	t.Helper()

	node := newNodeOn(t, storage, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	if err := node.New(item, encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)
	return node
}

// Refresh has to persist the node's own state. Saving an empty list leaves the
// log as the only source of truth, so it can never be rotated and restore time
// grows with the lifetime of the node.
func TestRefreshSavesNodeState(t *testing.T) {
	storage := &memStorage{}
	node := ownedNode(t, storage)

	if err := node.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snapshot, err := storage.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snapshot) == 0 {
		t.Fatal("Refresh saved an empty snapshot: the log is the only source of truth")
	}
	if got := countLines(snapshot, "collection|"); got != 1 {
		t.Fatalf("collection lines: got %d want 1 (%v)", got, snapshot)
	}
	if got := countLines(snapshot, "ownership|"); got < 1 {
		t.Fatalf("ownership lines: got %d want at least 1 (%v)", got, snapshot)
	}
}

// The point of the snapshot is to stand on its own: a node must recover its
// collections and ownership from it even when the log has been rotated away.
func TestRestoreRebuildsOwnershipFromSnapshotAlone(t *testing.T) {
	storage := &memStorage{}
	node := ownedNode(t, storage)

	if err := node.Refresh(); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	snapshot, err := storage.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	rotated := &memStorage{}
	if err := rotated.Save(snapshot); err != nil {
		t.Fatalf("Save: %v", err)
	}

	restored := newNodeOn(t, rotated, 64)
	if _, ok := restored.collections.Get("demo"); !ok {
		t.Fatal("restore did not rebuild the collection from the snapshot")
	}
	ownership, err := restored.Ownership()
	if err != nil {
		t.Fatalf("Ownership: %v", err)
	}
	if len(ownership["demo"]) == 0 {
		t.Fatal("restore did not rebuild any ownership from the snapshot")
	}
}

// A snapshot can name a delegation the node cannot place: no collection line
// before it, or a location set that was never materialized because the snapshot
// was saved while ownership was being handed off. Restoring must skip it rather
// than delegate on a collection that is not there.
func TestRestoreSkipsUnplaceableDelegation(t *testing.T) {
	for name, snapshot := range map[string][]string{
		"no collection line": {"delegation|zz"},
		"set never created":  {"collection|demo", "ownership|aa", "delegation|zz"},
	} {
		t.Run(name, func(t *testing.T) {
			storage := &memStorage{}
			if err := storage.Save(snapshot); err != nil {
				t.Fatalf("Save: %v", err)
			}
			// NewNode restores, so reaching this point without a panic is the
			// assertion.
			newNodeOn(t, storage, 64)
		})
	}
}
