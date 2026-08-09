package domain

import "encoding/json"

type Abelian struct {
	count   int
	metrics []float64
}

// Implementing json.Marshaler interface without a nested named-type alloc dance.
func (a *Abelian) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Count   int       `json:"count"`
		Metrics []float64 `json:"metrics"`
	}{
		Count:   a.count,
		Metrics: a.metrics,
	})
}

// Implementing json.Unmarshaler interface
func (a *Abelian) UnmarshalJSON(data []byte) error {
	type Obj struct {
		Count   int       `json:"count"`
		Metrics []float64 `json:"metrics"`
	}
	obj := &Obj{}
	if err := json.Unmarshal(data, obj); err != nil {
		return err
	}
	a.count = obj.Count
	a.metrics = obj.Metrics
	return nil
}

func NewAbelian(count int, metrics []float64) *Abelian {
	return &Abelian{
		count:   count,
		metrics: metrics,
	}
}

func (a *Abelian) IsEmpty() bool {
	if a.count != 0 {
		return false
	}
	for idx := range a.metrics {
		if a.metrics[idx] != 0 {
			return false
		}
	}
	return true
}

func (a *Abelian) Count() int {
	return a.count
}

func (a *Abelian) Metrics() []float64 {
	return a.metrics
}

// Metric reads one position, or 0 when the value does not carry it. Nothing
// forces a collection to hold items of one metric width — Sum says so — and an
// aggregate can reach a read with no metrics at all. Indexing Metrics() from a
// request handler therefore panics on data the mesh considers valid.
func (a *Abelian) Metric(idx int) float64 {
	if a == nil || idx < 0 || idx >= len(a.metrics) {
		return 0
	}
	return a.metrics[idx]
}

func (a *Abelian) Clone() *Abelian {
	metrics := make([]float64, len(a.metrics))
	copy(metrics, a.metrics)
	return &Abelian{
		count:   a.count,
		metrics: metrics,
	}
}

func (a *Abelian) IsEqual(b *Abelian) bool {
	if b == nil {
		return false
	}
	if a.count != b.count {
		return false
	}
	if len(a.metrics) != len(b.metrics) {
		return false
	}
	for idx := range a.metrics {
		if a.metrics[idx] != b.metrics[idx] {
			return false
		}
	}
	return true
}

// Sum folds a delta in, position by position. Deltas come from items and from
// peer aggregates, and nothing forces a collection to hold items of one metric
// width: a narrower delta only touches the positions it carries, and a wider
// one grows the total to fit.
//
// Growing matters because a counter raised by Set.incr starts with no metrics
// at all. Only widening on nil left such a total pinned at length zero, so it
// absorbed the count of every later item and none of their metrics — a cell
// reporting real transactions with a price, a latitude and a longitude of 0.
func (a *Abelian) Sum(delta *Abelian) {
	if delta == nil {
		return
	}
	a.count += delta.count
	a.widen(len(delta.metrics))
	for idx := range delta.metrics {
		a.metrics[idx] += delta.metrics[idx]
	}
}

// Substract is Sum's inverse, and widens on the same terms: an inverse that
// dropped the positions Sum keeps would not cancel it.
func (a *Abelian) Substract(delta *Abelian) {
	if delta == nil {
		return
	}
	a.count -= delta.count
	a.widen(len(delta.metrics))
	for idx := range delta.metrics {
		a.metrics[idx] -= delta.metrics[idx]
	}
}

// widen grows metrics to at least n positions, preserving what is already
// there. Positions a value never carried read as 0, which is their identity.
func (a *Abelian) widen(n int) {
	if len(a.metrics) >= n {
		return
	}
	grown := make([]float64, n)
	copy(grown, a.metrics)
	a.metrics = grown
}
