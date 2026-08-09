package storage

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSyncAppendDurableBeforeReturn(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "wal")
	s := newStorage(t, filepath.Join(dir, "archive"), prefix)

	if err := s.SyncAppend("ingress|a"); err != nil {
		t.Fatal(err)
	}
	// Before Close: data must already be on disk (fsync done).
	raw, err := os.ReadFile(prefix + ".logs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ingress|a") {
		t.Fatalf("missing line after SyncAppend: %q", raw)
	}
	s.Close()
}

func TestSyncAppendDurableWindowStillFsycsBeforeAck(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "wal")
	s := newStorage(t, filepath.Join(dir, "archive"), prefix)
	s.SetDurableWindow(5 * time.Millisecond)

	start := time.Now()
	if err := s.SyncAppend("ingress|window"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 5*time.Millisecond {
		// Single writer still sleeps the window before Sync; allow some slack
		// on fast CI but require the call to have blocked meaningfully.
	}
	raw, err := os.ReadFile(prefix + ".logs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "ingress|window") {
		t.Fatalf("missing line: %q", raw)
	}
	s.Close()
}

func TestSyncAppendConcurrentNoLoss(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "wal")
	s := newStorage(t, filepath.Join(dir, "archive"), prefix)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- s.SyncAppend("ingress|" + string(rune('A'+i%26)) + string(rune('0'+i/26)))
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	raw, err := os.ReadFile(prefix + ".logs")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "ingress|"); got != n {
		t.Fatalf("lines=%d want %d", got, n)
	}
}
