package domain

import "encoding/json"

type Abelian struct {
	count   int
	metrics []float64
}

// Implementing json.Marshaler interface
func (a *Abelian) MarshalJSON() ([]byte, error) {
	type Obj struct {
		Count   int       `json:"count"`
		Metrics []float64 `json:"metrics"`
	}
	return json.Marshal(&Obj{
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

func (a *Abelian) Sum(delta *Abelian) {
	a.count += delta.count
	for idx := range a.metrics {
		a.metrics[idx] += delta.metrics[idx]
	}
}

func (a *Abelian) Substract(delta *Abelian) {
	a.count -= delta.count
	for idx := range a.metrics {
		a.metrics[idx] -= delta.metrics[idx]
	}
}
