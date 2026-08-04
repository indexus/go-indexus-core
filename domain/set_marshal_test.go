package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSetMarshalJSONMatchesList(t *testing.T) {
	s := NewSet()
	s.Put("aa:x", NewAbelian(1, []float64{1, 2}))
	s.Put("ab:y", NewAbelian(1, []float64{3}))

	viaSet, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal set: %v", err)
	}
	viaList, err := json.Marshal(s.List())
	if err != nil {
		t.Fatalf("Marshal list: %v", err)
	}

	var fromSet, fromList map[string]*Abelian
	if err := json.Unmarshal(viaSet, &fromSet); err != nil {
		t.Fatalf("Unmarshal set: %v", err)
	}
	if err := json.Unmarshal(viaList, &fromList); err != nil {
		t.Fatalf("Unmarshal list: %v", err)
	}
	if len(fromSet) != len(fromList) {
		t.Fatalf("len set=%d list=%d", len(fromSet), len(fromList))
	}
	for k, a := range fromList {
		b, ok := fromSet[k]
		if !ok || !a.IsEqual(b) {
			t.Fatalf("key %s mismatch set=%v list=%v", k, b, a)
		}
	}
}

// MarshalJSON must release Set.mu before encoding so writers are not blocked
// for the duration of json.Marshal.
func TestSetMarshalJSONDoesNotHoldLockDuringEncode(t *testing.T) {
	s := NewSet()
	for i := 0; i < 200; i++ {
		s.Put(string(rune('a'+i%26))+string(rune('0'+i%10)), NewAbelian(1, []float64{float64(i)}))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = json.Marshal(s)
	}()

	// While marshal runs (or right after snapshot), Put must not deadlock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.Put("zz:concurrent", NewAbelian(1, []float64{1}))
		select {
		case <-done:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("Put blocked while MarshalJSON held the lock")
}

func TestSetMarshalJSONNil(t *testing.T) {
	var s *Set
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("got %s want null", raw)
	}
}
