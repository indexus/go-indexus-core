package core

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMemStoreListAndDeletePrefix(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()
	_ = m.Put(ctx, "zones/a/1.snap", []byte("a"))
	_ = m.Put(ctx, "zones/b/2.snap", []byte("bb"))
	_ = m.Put(ctx, "nodes/x/manifest.json", []byte("{}"))
	_ = m.Put(ctx, "other/keep", []byte("k"))

	listed, err := m.List(ctx, "zones/")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("list zones: got %d want 2", len(listed))
	}

	if err := m.DeletePrefix(ctx, "zones/"); err != nil {
		t.Fatal(err)
	}
	listed, err = m.List(ctx, "zones/")
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("after delete zones: got %d", len(listed))
	}
	if _, err := m.Get(ctx, "nodes/x/manifest.json"); err != nil {
		t.Fatalf("nodes key should remain: %v", err)
	}
	if _, err := m.Get(ctx, "other/keep"); err != nil {
		t.Fatalf("other key should remain: %v", err)
	}
}

func TestNodeListAndClearSnapshots(t *testing.T) {
	n := &Node{}
	store := NewMemStore()
	n.SetStore(store)
	ctx := context.Background()
	_ = store.Put(ctx, "zones/c/1.snap", []byte("z"))
	_ = store.Put(ctx, "nodes/n/manifest.json", []byte("{}"))
	_ = store.Put(ctx, "snapshots/n/latest/x", []byte("s"))
	_ = store.Put(ctx, "keep/me", []byte("k"))

	listed, err := n.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if listed["available"] != true {
		t.Fatalf("available=%v", listed["available"])
	}
	if listed["count"] != 3 {
		t.Fatalf("count=%v want 3", listed["count"])
	}

	cleared, err := n.ClearSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cleared["cleared"] != 3 {
		t.Fatalf("cleared=%v want 3", cleared["cleared"])
	}
	if _, err := store.Get(ctx, "keep/me"); err != nil {
		t.Fatalf("unrelated key deleted: %v", err)
	}
	listed, err = n.ListSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if listed["count"] != 0 {
		t.Fatalf("after clear count=%v", listed["count"])
	}
}

func TestDirStoreRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SNAPSHOT_DIR", root)
	d, err := NewDirStore()
	if err != nil || d == nil {
		t.Fatalf("NewDirStore: %v store=%v", err, d)
	}
	ctx := context.Background()
	if err := d.Put(ctx, "zones/c/1.snap", []byte("zone-a")); err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "nodes/n/manifest.json", []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := d.Get(ctx, "zones/c/1.snap")
	if err != nil || string(got) != "zone-a" {
		t.Fatalf("get: %v %q", err, got)
	}
	listed, err := d.List(ctx, "zones/")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v len=%d", err, len(listed))
	}
	if err := d.DeletePrefix(ctx, "zones/"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Get(ctx, "zones/c/1.snap"); err == nil {
		t.Fatal("expected missing after delete")
	}
	if _, err := d.Get(ctx, "nodes/n/manifest.json"); err != nil {
		t.Fatalf("nodes key should remain: %v", err)
	}
	// ".." segments must not write outside root.
	if err := d.Put(ctx, "../outside", []byte("x")); err != nil {
		t.Fatalf("normalized put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "outside")); err == nil {
		t.Fatal("escaped write outside root")
	}
	if _, err := os.Stat(filepath.Join(root, "outside")); err != nil {
		t.Fatalf("expected write under root: %v", err)
	}
}
