package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
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

// The background writer publishes the log file while the shutdown path tears it
// down. Node shutdown is the drain path, so a torn write here loses the tail of
// the log at the worst possible moment.
func TestStartAndCloseDoNotRace(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "backup")
	s := NewStorage(filepath.Join(dir, "archive"), prefix)

	started := make(chan error, 1)
	go func() { started <- s.Start() }()

	for i := 0; i < 50; i++ {
		s.Append("collection|coll")
	}

	s.Close()
	// Close must tolerate being called twice: shutdown paths and test cleanups
	// both reach for it.
	s.Close()

	select {
	case err := <-started:
		if err != nil {
			t.Fatalf("Start returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Close")
	}

	raw, err := os.ReadFile(prefix + ".logs")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// Everything handed to Append before Close has to reach the file: a clean
	// shutdown is precisely when a node is draining.
	if got := strings.Count(string(raw), "collection|coll"); got != 50 {
		t.Fatalf("log kept %d of 50 lines written before Close", got)
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
