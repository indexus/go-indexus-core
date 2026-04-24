package storage

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "backup")
	s := NewStorage(filepath.Join(dir, "archive"), prefix)

	want := []string{
		"contact|abc|1.2.3.4|21000",
		"collection|coll",
		"ownership|@",
		"delegation|@a",
	}
	if err := s.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if !s.Exist() {
		t.Fatalf("Exist must be true after Save")
	}

	got, err := s.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Load mismatch:\n got=%v\nwant=%v", got, want)
	}
}

func TestLoadMissingSnapshotIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	s := NewStorage(filepath.Join(dir, "archive"), filepath.Join(dir, "backup"))

	got, err := s.Load()
	if err != nil {
		t.Fatalf("missing snapshot must not be an error, got %v", err)
	}
	if got != nil {
		t.Fatalf("missing snapshot must return nil commands, got %v", got)
	}
}
