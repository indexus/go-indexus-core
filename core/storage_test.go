package core

import (
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/storage"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func countLines(logs []string, prefix string) int {
	count := 0
	for _, line := range logs {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

func TestRestoreReplaysDeleteAfterAdd(t *testing.T) {
	storage := &memStorage{}
	root := encoding.BASE64.Root()

	node := newNodeOn(t, storage, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}

	if err := node.New(item, root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 1 {
		t.Fatalf("count after add: got %d (err=%v) want 1", count, err)
	}

	if err := node.Delete(item, root, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 0 {
		t.Fatalf("count after delete: got %d (err=%v) want 0", count, err)
	}

	logs := storage.Logs()
	if got := countLines(logs, "ingress|"); got != 1 {
		t.Fatalf("ingress lines: got %d want 1 (%v)", got, logs)
	}
	if got := countLines(logs, "delete|"); got != 1 {
		t.Fatalf("delete lines: got %d want 1 (%v)", got, logs)
	}
	if got := countLines(logs, "tombstone|"); got != 1 {
		t.Fatalf("tombstone lines: got %d want 1 (%v)", got, logs)
	}

	restored := newNodeOn(t, storage, 64)
	drain(t, restored)

	if count, err := restored.Count(); err != nil || count != 0 {
		t.Fatalf("count after restore: got %d (err=%v) want 0", count, err)
	}

	collection, ok := restored.collections.Get("demo")
	if !ok {
		t.Fatal("restore did not rebuild the collection")
	}
	if !collection.IsTombstoned("aa", "x") {
		t.Fatal("restore lost the tombstone, a stale replay could resurrect the item")
	}
}

func TestRestoreReplaysReaddAfterDelete(t *testing.T) {
	storage := &memStorage{}
	root := encoding.BASE64.Root()

	node := newNodeOn(t, storage, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}

	if err := node.New(item, root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)
	if err := node.Delete(item, root, nil); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	drain(t, node)
	if err := node.New(item, root, nil); err != nil {
		t.Fatalf("re-New: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 1 {
		t.Fatalf("count after re-add: got %d (err=%v) want 1", count, err)
	}

	restored := newNodeOn(t, storage, 64)
	drain(t, restored)

	if count, err := restored.Count(); err != nil || count != 1 {
		t.Fatalf("count after restore: got %d (err=%v) want 1", count, err)
	}

	collection, ok := restored.collections.Get("demo")
	if !ok {
		t.Fatal("restore did not rebuild the collection")
	}
	if collection.IsTombstoned("aa", "x") {
		t.Fatal("tombstone survived the re-add")
	}
}

func TestCheckpointRestoresItemsWithoutTheLog(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "backup")
	archive := filepath.Join(dir, "archive")

	store, err := storage.NewStorage(archive, prefix)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	go func() { _ = store.Start() }()
	t.Cleanup(store.Close)
	node := newNodeOn(t, store, 64)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1, 2, 3}}
	if err := node.New(item, encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)

	if err := node.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	store.Close()

	if _, err := os.Stat(prefix + ".logs"); !os.IsNotExist(err) {

		raw, _ := os.ReadFile(prefix + ".logs")
		if len(raw) > 0 {
			t.Fatalf("log still holds data after checkpoint: %q", raw)
		}
	}

	restoredStore, err := storage.NewStorage(archive, prefix)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	restored := newNodeOn(t, restoredStore, 64)

	count, err := restored.Count()
	if err != nil || count != 1 {
		t.Fatalf("Count: got %d err=%v want 1", count, err)
	}
	commands, err := restoredStore.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	collection, ok := restored.collections.Get("demo")
	if !ok {
		t.Fatalf("collection missing after restore; snapshot=%v", commands)
	}
	var metrics []float64
	var seenID string
	collection.Traverse(encoding.BASE64.Root(),
		func(string, *domain.Abelian) {},
		func(_, _, id string, a *domain.Abelian) {
			seenID = id
			metrics = a.Metrics()
		},
	)
	if seenID != "x" {
		t.Fatalf("item id=%q want x; snapshot=%v", seenID, commands)
	}
	if len(metrics) != 3 || metrics[0] != 1 || metrics[1] != 2 || metrics[2] != 3 {
		t.Fatalf("metrics after restore: %v; snapshot=%v", metrics, commands)
	}
}

func TestRestoreReplaysLogWithoutSnapshot(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "backup")
	archive := filepath.Join(dir, "archive")

	store, err := storage.NewStorage(archive, prefix)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	go func() { _ = store.Start() }()
	t.Cleanup(store.Close)
	node := newNodeOn(t, store, 64)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "y", Metrics: []float64{9}}
	if err := node.New(item, encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)
	store.Close()

	_ = os.Remove(prefix + ".snapshot")
	if _, err := os.Stat(prefix + ".logs"); err != nil {
		t.Fatalf("precondition: log missing: %v", err)
	}

	restoredStore, err := storage.NewStorage(archive, prefix)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	restored := newNodeOn(t, restoredStore, 64)
	drain(t, restored)

	count, err := restored.Count()
	if err != nil || count != 1 {
		t.Fatalf("Count after log-only restore: got %d err=%v want 1", count, err)
	}
}

func TestCheckpointKeepsPendingIngress(t *testing.T) {
	storage := &memStorage{}
	node := newNodeOn(t, storage, 64)

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "pending", Metrics: []float64{1}}
	if err := node.New(item, encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := node.queue.Length(); got != 1 {
		t.Fatalf("precondition: queue=%d want 1", got)
	}

	if err := node.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := countLines(storage.snapshot, "pending-ingress|"); got != 1 {
		t.Fatalf("pending-ingress lines: got %d want 1 (%v)", got, storage.snapshot)
	}

	restored := newNodeOn(t, storage, 64)
	if got := restored.queue.Length(); got != 1 {
		t.Fatalf("pending queue after restore: got %d want 1", got)
	}
	drain(t, restored)
	count, err := restored.Count()
	if err != nil || count != 1 {
		t.Fatalf("Count: got %d err=%v want 1", count, err)
	}
}

func TestCheckpointKeepsParkedIngressOutsideHotQueue(t *testing.T) {
	store := &memStorage{}
	node := newNodeOn(t, store, 64)
	root := encoding.BASE64.Root()

	node.create("demo", root)
	collection, ok := node.collections.Get("demo")
	if !ok {
		t.Fatal("missing collection")
	}
	collection.Own("a", domain.Delegation{})
	collection.MarkDelegatedTo("a", "aa", "missing-peer")

	item := &domain.Item{
		Collection: "demo",
		Location:   "aaa",
		Id:         "parked",
		Metrics:    []float64{1},
	}
	if err := node.New(item, root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	element, ok := node.queue.TryConsume()
	if !ok {
		t.Fatal("accepted write missing from queue")
	}
	if node.feedOnce(element) {
		t.Fatal("write under a named mark unexpectedly landed")
	}
	if got := node.parkedCount(); got != 1 {
		t.Fatalf("parked before checkpoint: got %d want 1", got)
	}
	if got := node.queue.Length(); got != 0 {
		t.Fatalf("hot queue before checkpoint: got %d want 0", got)
	}

	if err := node.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if got := countLines(store.snapshot, "parked-ingress|"); got != 1 {
		t.Fatalf("parked-ingress lines: got %d want 1 (%v)", got, store.snapshot)
	}

	restored := newNodeOn(t, store, 64)
	if got := restored.parkedCount(); got != 1 {
		t.Fatalf("parked after restore: got %d want 1", got)
	}
	if got := restored.queue.Length(); got != 0 {
		t.Fatalf("hot queue after restore: got %d want 0", got)
	}
	stats := restored.IngressStats()
	byCollection, ok := stats["parked_by_collection"].(map[string]int)
	if !ok {
		t.Fatalf("parked_by_collection type=%T", stats["parked_by_collection"])
	}
	if got := byCollection["demo"]; got != 1 {
		t.Fatalf("parked_by_collection[demo]=%d want 1", got)
	}
}

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

func TestCheckpointSkipsSaveWhenClean(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "backup")
	archive := filepath.Join(dir, "archive")

	store, err := storage.NewStorage(archive, prefix)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	go func() { _ = store.Start() }()
	t.Cleanup(store.Close)

	node := ownedNode(t, store)
	if err := node.Checkpoint(); err != nil {
		t.Fatalf("first Checkpoint: %v", err)
	}
	first, err := os.ReadFile(prefix + ".snapshot")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if store.Dirty() {
		t.Fatal("storage must be clean after Checkpoint")
	}

	if err := node.Checkpoint(); err != nil {
		t.Fatalf("idle Checkpoint: %v", err)
	}
	second, err := os.ReadFile(prefix + ".snapshot")
	if err != nil {
		t.Fatalf("ReadFile after idle: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("idle Checkpoint rewrote the snapshot")
	}
}

func TestRestoreRebuildsOwnershipFromSnapshotAlone(t *testing.T) {
	storage := &memStorage{}
	node := ownedNode(t, storage)

	if err := node.Checkpoint(); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	snapshot, err := storage.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := countLines(snapshot, "item|"); got < 1 {
		t.Fatalf("item lines: got %d want at least 1 (%v)", got, snapshot)
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
	count, err := restored.Count()
	if err != nil || count != 1 {
		t.Fatalf("Count: got %d err=%v want 1", count, err)
	}
}

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

			newNodeOn(t, storage, 64)
		})
	}
}
