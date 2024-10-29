package domain

import "encoding/json"

type Abelian struct {
	count      int
	properties []float64
}

// Implementing json.Marshaler interface
func (a *Abelian) MarshalJSON() ([]byte, error) {
	type Obj struct {
		Count      int       `json:"count"`
		Properties []float64 `json:"properties"`
	}
	return json.Marshal(&Obj{
		Count:      a.count,
		Properties: a.properties,
	})
}

// Implementing json.Unmarshaler interface
func (a *Abelian) UnmarshalJSON(data []byte) error {
	type Obj struct {
		Count      int       `json:"count"`
		Properties []float64 `json:"properties"`
	}
	obj := &Obj{}
	if err := json.Unmarshal(data, obj); err != nil {
		return err
	}
	a.count = obj.Count
	a.properties = obj.Properties
	return nil
}

func NewAbelian(count int, properties []float64) *Abelian {
	return &Abelian{
		count:      count,
		properties: properties,
	}
}

func (a *Abelian) IsEmpty() bool {
	if a.count != 0 {
		return false
	}
	for idx := range a.properties {
		if a.properties[idx] != 0 {
			return false
		}
	}
	return true
}

func (a *Abelian) Count() int {
	return a.count
}

func (a *Abelian) Properties() []float64 {
	return a.properties
}

func (a *Abelian) Sum(delta *Abelian) {
	a.count += delta.count
	for idx := range a.properties {
		a.properties[idx] += delta.properties[idx]
	}
}

func (a *Abelian) Substract(delta *Abelian) {
	a.count -= delta.count
	for idx := range a.properties {
		a.properties[idx] -= delta.properties[idx]
	}
}
