package domain

import "testing"

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
