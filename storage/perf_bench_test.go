package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// legacySyncAppend reproduces the pre-optimization path: open + write + fsync + close
// on every call. Kept here for before/after measurement only.
func legacySyncAppend(path, log string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(log + "\n"); err != nil {
		return err
	}
	return f.Sync()
}

func BenchmarkSyncAppendLegacy(b *testing.B) {
	dir := b.TempDir()
	path := filepath.Join(dir, "wal.logs")
	line := "ingress|@|@|coll|loc|id|1.0"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := legacySyncAppend(path, line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSyncAppendOptimized(b *testing.B) {
	dir := b.TempDir()
	prefix := filepath.Join(dir, "wal")
	s, err := NewStorage(filepath.Join(dir, "archive"), prefix)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	line := "ingress|@|@|coll|loc|id|1.0"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.SyncAppend(line); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSyncAppendOptimizedParallel(b *testing.B) {
	dir := b.TempDir()
	prefix := filepath.Join(dir, "wal")
	s, err := NewStorage(filepath.Join(dir, "archive"), prefix)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			line := fmt.Sprintf("ingress|@|@|coll|loc|id-%d|1.0", i)
			i++
			if err := s.SyncAppend(line); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func TestSyncAppendDurable(t *testing.T) {
	dir := t.TempDir()
	prefix := filepath.Join(dir, "wal")
	s, err := NewStorage(filepath.Join(dir, "archive"), prefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncAppend("ingress|a"); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncAppend("ingress|b"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	raw, err := os.ReadFile(prefix + ".logs")
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if got != "ingress|a\ningress|b\n" {
		t.Fatalf("log content=%q", got)
	}
}
