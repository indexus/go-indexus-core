package core

import (
	"context"
	"os"
	"testing"

	"github.com/indexus/go-indexus-core/encoding"
)

func TestZoneSnapshotRoundTrip(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	n.SetStore(store)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")

	root := encoding.BASE64.Root()
	if err := n.New(item("z1"), root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)

	keys := n.listOwnedKeys()
	if len(keys) == 0 {
		t.Fatal("expected owned keys")
	}
	if err := n.forceZones(context.Background(), keys); err != nil {
		t.Fatalf("forceZones: %v", err)
	}

	entry, ok := n.zoneRef(keys[0])
	if !ok {
		t.Fatal("expected manifest entry after checkpoint")
	}
	body, err := store.Get(context.Background(), entry.Key)
	if err != nil {
		t.Fatalf("Get zone: %v", err)
	}
	lines, err := gobDecodeStrings(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(lines) < 2 {
		t.Fatalf("zone snap too short: %v", lines)
	}

	man, err := pullManifest(context.Background(), store, n.Name())
	if err != nil {
		t.Fatalf("pullManifest: %v", err)
	}
	if len(man.Zones) == 0 {
		t.Fatal("manifest missing zones")
	}

	recv := newNodeOn(t, &memStorage{}, 64)
	if err := recv.applyZoneLines(lines); err != nil {
		t.Fatalf("applyZoneLines: %v", err)
	}
	count, err := recv.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d want 1", count)
	}
}

func TestUploadWALRecordsManifest(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	n.SetStore(store)

	dir := t.TempDir()
	path := dir + "/20260101_000000_backup.logs"
	if err := os.WriteFile(path, []byte("demo|aa|x|1.000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := n.UploadWAL(context.Background(), path); err != nil {
		t.Fatalf("UploadWAL: %v", err)
	}
	man, err := pullManifest(context.Background(), store, n.Name())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(man.WALSegs) != 1 {
		t.Fatalf("wal segs=%d want 1", len(man.WALSegs))
	}
}

func TestDirtyTrackingMarksOwnedZone(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	if err := n.New(item("d1"), root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)

	n.zoneSnap.mu.Lock()
	dirty := len(n.zoneSnap.dirty)
	n.zoneSnap.mu.Unlock()
	if dirty == 0 {
		t.Fatal("expected dirty zone after insert")
	}
}
