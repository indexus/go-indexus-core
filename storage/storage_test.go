package storage

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

func TestSaveLoadClusterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	s := NewStorage(filepath.Join(dir, "archive"), root)

	want := []string{
		"contact|abc|1.2.3.4|21000",
		"collection|coll",
		"ownership|@",
		"delegation|@a",
	}
	if err := s.SaveCluster(want); err != nil {
		t.Fatalf("SaveCluster: %v", err)
	}
	if !s.Exist() {
		t.Fatalf("Exist must be true after SaveCluster")
	}

	got, err := s.LoadCluster()
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoadCluster mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestLoadClusterMissingIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(filepath.Join(dir, "archive"), filepath.Join(dir, "data"))

	got, err := s.LoadCluster()
	if err != nil {
		t.Fatalf("missing cluster snapshot must not be an error, got %v", err)
	}
	if got != nil {
		t.Fatalf("missing cluster snapshot must return nil commands, got %v", got)
	}
}

func TestShardSnapshotAppendLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	s := NewStorage(filepath.Join(dir, "archive"), root)

	key := domain.Key{Collection: "myColl", Location: "@"}
	header := map[string]*domain.Abelian{
		"myColl|@|id1|1:2": domain.NewAbelian(1, []float64{1, 2}),
	}
	items := []string{
		"myColl|@|id1|1:2",
		"myColl|ab|id2|3",
	}
	if err := s.SnapshotShard(key, header, items); err != nil {
		t.Fatalf("SnapshotShard: %v", err)
	}
	s.AppendShard(key, "myColl|ab|id3|0")

	gotHeader, gotItems, gotLogs, err := s.LoadShard(key)
	if err != nil {
		t.Fatalf("LoadShard: %v", err)
	}
	if !reflect.DeepEqual(gotItems, items) {
		t.Fatalf("items mismatch got=%v want=%v", gotItems, items)
	}
	wantLogs := []string{"myColl|ab|id3|0"}
	if !reflect.DeepEqual(gotLogs, wantLogs) {
		t.Fatalf("logs mismatch got=%v want=%v", gotLogs, wantLogs)
	}
	if len(gotHeader) != len(header) {
		t.Fatalf("header length mismatch got=%d want=%d", len(gotHeader), len(header))
	}

	// Verify LoadShardHeader returns only header without items.
	hdrOnly, err := s.LoadShardHeader(key)
	if err != nil {
		t.Fatalf("LoadShardHeader: %v", err)
	}
	if len(hdrOnly) != len(header) {
		t.Fatalf("LoadShardHeader length mismatch got=%d want=%d", len(hdrOnly), len(header))
	}

	keys, err := s.Shards()
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(keys) != 1 || keys[0] != key {
		t.Fatalf("Shards got=%v want one key %v", keys, key)
	}
}

func TestSnapshotShardTruncatesLog(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	s := NewStorage(filepath.Join(dir, "archive"), root)
	key := domain.Key{Collection: "c", Location: "@"}

	s.AppendShard(key, "c|@|a|1")
	if err := s.SnapshotShard(key, nil, []string{"c|@|a|1"}); err != nil {
		t.Fatal(err)
	}
	_, items, logs, err := s.LoadShard(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("log should be empty after snapshot, got %v", logs)
	}
	if len(items) != 1 {
		t.Fatalf("snapshot want 1 item got %v", items)
	}
}

// TestShardHeaderRoundTrip verifies that a header with Abelian values
// survives a gob encode / decode cycle with full precision.
func TestShardHeaderRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(filepath.Join(dir, "archive"), filepath.Join(dir, "data"))

	key := domain.Key{Collection: "col", Location: "1uA"}
	header := map[string]*domain.Abelian{
		"1uAx": domain.NewAbelian(42, []float64{1.23456789, 9.87654321}),
		"1uAy": domain.NewAbelian(7, nil),
	}
	if err := s.SnapshotShard(key, header, nil); err != nil {
		t.Fatalf("SnapshotShard: %v", err)
	}

	got, err := s.LoadShardHeader(key)
	if err != nil {
		t.Fatalf("LoadShardHeader: %v", err)
	}
	if len(got) != len(header) {
		t.Fatalf("header length mismatch got=%d want=%d", len(got), len(header))
	}
	for k, wantA := range header {
		gotA, ok := got[k]
		if !ok {
			t.Fatalf("missing key %q in header", k)
		}
		if !wantA.IsEqual(gotA) {
			t.Fatalf("abelian mismatch for %q: got=%v want=%v", k, gotA, wantA)
		}
	}
}

// TestShardCaseInsensitiveFSNoCollision guards against the bug where two
// owned locations differing only in letter case (e.g. `1uA` and `1ua`)
// landed on the same on-disk file on case-insensitive filesystems (APFS
// default, NTFS), silently overwriting each other and losing data on
// restore.
func TestShardCaseInsensitiveFSNoCollision(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(filepath.Join(dir, "archive"), filepath.Join(dir, "data"))

	upper := domain.Key{Collection: "DENjYsMTAyLDE2ME", Location: "1uA"}
	lower := domain.Key{Collection: "DENjYsMTAyLDE2ME", Location: "1ua"}

	upperItems := []string{"DENjYsMTAyLDE2ME|1uAxxx|idU|1"}
	lowerItems := []string{"DENjYsMTAyLDE2ME|1uaxxx|idL|2"}
	if err := s.SnapshotShard(upper, nil, upperItems); err != nil {
		t.Fatalf("SnapshotShard upper: %v", err)
	}
	if err := s.SnapshotShard(lower, nil, lowerItems); err != nil {
		t.Fatalf("SnapshotShard lower: %v", err)
	}

	_, gotUpperItems, _, err := s.LoadShard(upper)
	if err != nil {
		t.Fatalf("LoadShard upper: %v", err)
	}
	if !reflect.DeepEqual(gotUpperItems, upperItems) {
		t.Fatalf("upper shard corrupted by case collision: got=%v want=%v", gotUpperItems, upperItems)
	}
	_, gotLowerItems, _, err := s.LoadShard(lower)
	if err != nil {
		t.Fatalf("LoadShard lower: %v", err)
	}
	if !reflect.DeepEqual(gotLowerItems, lowerItems) {
		t.Fatalf("lower shard corrupted by case collision: got=%v want=%v", gotLowerItems, lowerItems)
	}

	keys, err := s.Shards()
	if err != nil {
		t.Fatalf("Shards: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("Shards must enumerate both case-distinct keys, got %v", keys)
	}
	seen := map[string]bool{}
	for _, k := range keys {
		seen[k.Location] = true
	}
	if !seen["1uA"] || !seen["1ua"] {
		t.Fatalf("Shards lost case-distinct key: %v", keys)
	}
}

func TestDropShardArchivesFiles(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "archive")
	root := filepath.Join(dir, "data")
	s := NewStorage(archive, root)
	key := domain.Key{Collection: "c", Location: "@"}

	if err := s.SnapshotShard(key, nil, []string{"c|@|x|0"}); err != nil {
		t.Fatal(err)
	}
	s.AppendShard(key, "c|@|y|1")

	if err := s.DropShard(key); err != nil {
		t.Fatalf("DropShard: %v", err)
	}
	keys, _ := s.Shards()
	if len(keys) != 0 {
		t.Fatalf("Shards should be empty after drop, got %v", keys)
	}

	entries, err := filepath.Glob(filepath.Join(archive, "*_drop_c_@*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one archive drop dir, got %v", entries)
	}
}
