package storage

import (
	"path/filepath"
	"testing"
)

func TestSealRotateReplaysInStream(t *testing.T) {
	prev := sealMinBytes
	sealMinBytes = 64
	defer func() { sealMinBytes = prev }()

	dir := t.TempDir()
	base := filepath.Join(dir, "backup")
	s, err := NewStorage(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for i := 0; i < 20; i++ {
		if err := s.SyncAppend("ingress|demo|@|line"); err != nil {
			t.Fatal(err)
		}
	}

	archived, err := s.SealRotate()
	if err != nil {
		t.Fatal(err)
	}
	if archived == "" {
		t.Fatal("expected seal with lowered threshold")
	}
	if !s.Dirty() {
		t.Fatal("SealRotate must leave Dirty true")
	}

	if err := s.SyncAppend("ingress|demo|@|after-seal"); err != nil {
		t.Fatal(err)
	}

	sawBefore, sawAfter := false, false
	for log := range s.Stream(0) {
		switch log {
		case "ingress|demo|@|line":
			sawBefore = true
		case "ingress|demo|@|after-seal":
			sawAfter = true
		}
	}
	if !sawBefore {
		t.Fatal("stream missing sealed segment lines")
	}
	if !sawAfter {
		t.Fatal("stream missing post-seal line")
	}
}
