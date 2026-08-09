package domain

import (
	"testing"
)

func TestAbelianCloneIsDeepCopy(t *testing.T) {
	original := NewAbelian(3, []float64{1, 2, 3})
	clone := original.Clone()

	clone.Substract(NewAbelian(1, []float64{1, 1, 1}))

	if got := original.Count(); got != 3 {
		t.Fatalf("Clone shared count with original: got %d, want 3", got)
	}
	for i, want := range []float64{1, 2, 3} {
		if got := original.Metrics()[i]; got != want {
			t.Fatalf("Clone shared metrics[%d] with original: got %v, want %v", i, got, want)
		}
	}
}

func TestAbelianIsEqualHandlesNilAndShape(t *testing.T) {
	a := NewAbelian(1, []float64{1, 2})

	if a.IsEqual(nil) {
		t.Fatalf("IsEqual(nil) must be false, not panic")
	}
	if a.IsEqual(NewAbelian(1, []float64{1, 2, 3})) {
		t.Fatalf("IsEqual must reject mismatched metrics shape")
	}
	if !a.IsEqual(NewAbelian(1, []float64{1, 2})) {
		t.Fatalf("IsEqual must accept structural equals")
	}
}

func TestAbelianSumToleratesNilAndShorterDelta(t *testing.T) {
	a := NewAbelian(1, []float64{1, 2, 3})
	a.Sum(nil) // must not panic
	a.Sum(NewAbelian(1, []float64{10}))
	if a.Count() != 2 {
		t.Fatalf("count=%d want 2", a.Count())
	}
	m := a.Metrics()
	if m[0] != 11 || m[1] != 2 || m[2] != 3 {
		t.Fatalf("metrics=%v want [11 2 3]", m)
	}
}

// Set.incr opens a counter with no metrics, then folds items into it. A total
// that only widened on nil stayed at length zero for good: it kept counting the
// items and discarded every metric they carried, so the cell reported real
// transactions at a price, a latitude and a longitude of exactly 0.
func TestAbelianSumGrowsToFitAWiderDelta(t *testing.T) {
	counter := NewAbelian(0, nil)
	counter.Sum(NewAbelian(0, nil)) // the incr that pins the width

	counter.Sum(NewAbelian(1, []float64{250000, 80, 3125, 138.85, 182.35}))
	counter.Sum(NewAbelian(1, []float64{150000, 50, 3000, 138.86, 182.36}))

	if counter.Count() != 2 {
		t.Fatalf("count=%d want 2", counter.Count())
	}
	m := counter.Metrics()
	if len(m) != 5 {
		t.Fatalf("metrics=%v, want 5 positions", m)
	}
	if m[0] != 400000 || m[1] != 130 || m[2] != 6125 {
		t.Fatalf("metrics=%v, want the items' metrics summed", m)
	}
}

// Sum and Substract have to agree on width, or a delta that was added cannot be
// taken back out — which is how a handoff undoes a transfer.
func TestAbelianSubstractUndoesAWiderSum(t *testing.T) {
	a := NewAbelian(0, nil)
	delta := NewAbelian(2, []float64{7, 8, 9})

	a.Sum(delta)
	a.Substract(delta)

	if a.Count() != 0 {
		t.Fatalf("count=%d want 0", a.Count())
	}
	for idx, value := range a.Metrics() {
		if value != 0 {
			t.Fatalf("metrics[%d]=%v, want the sum undone", idx, value)
		}
	}
}

func TestAbelianSubstractToleratesNilAndShorterDelta(t *testing.T) {
	a := NewAbelian(1, []float64{10, 20})
	a.Substract(nil)
	a.Substract(NewAbelian(1, []float64{3}))
	if a.Count() != 0 {
		t.Fatalf("count=%d want 0", a.Count())
	}
	m := a.Metrics()
	if m[0] != 7 || m[1] != 20 {
		t.Fatalf("metrics=%v want [7 20]", m)
	}
}
