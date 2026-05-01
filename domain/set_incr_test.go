package domain

import "testing"

func TestSetIncrInitializesMissingEntry(t *testing.T) {
	s := NewSet()
	delta := NewAbelian(2, []float64{3, 4})

	got := s.Incr("missing", delta)
	if got == nil {
		t.Fatal("expected non-nil abelian")
	}
	if got.Count() != 2 {
		t.Fatalf("count=%d want 2", got.Count())
	}
	if len(got.Metrics()) != 2 || got.Metrics()[0] != 3 || got.Metrics()[1] != 4 {
		t.Fatalf("metrics=%v want [3 4]", got.Metrics())
	}
}
