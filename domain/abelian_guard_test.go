package domain

import "testing"

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
