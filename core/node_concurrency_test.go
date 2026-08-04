package core

import (
	"fmt"
	"sync"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

type noopStorage struct{}

func (noopStorage) Exist() bool             { return false }
func (noopStorage) Reset() error            { return nil }
func (noopStorage) Save([]string) error     { return nil }
func (noopStorage) Load() ([]string, error) { return nil, nil }
func (noopStorage) Append(string)           {}
func (noopStorage) Stream(int) <-chan string {
	c := make(chan string)
	close(c)
	return c
}

func TestNodeCleanRacesMonitoring(t *testing.T) {
	const ops = 500

	n := newNodeOn(t, noopStorage{}, domain.DelegationTreshold())

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			_ = n.clean()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_, _ = n.Routing()
				_, _ = n.Acknowledged()
				_, _ = n.Registered()
			}
		}()
	}

	wg.Wait()
}

func TestNodeInsertRacesOwnership(t *testing.T) {
	const (
		items      = 500
		delegation = 10
	)

	n := newNodeOn(t, noopStorage{}, delegation)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < items; i++ {
			location := encoding.BASE64.CharAt(i % encoding.BASE64.Length())
			_ = n.insert(&domain.Item{
				Collection: collection,
				Location:   location,
				Id:         fmt.Sprintf("id-%d", i),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, encoding.BASE64.Root(), location, true)
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < items; i++ {
				_, _ = n.Ownership()
				_, _ = n.Count()
			}
		}()
	}

	wg.Wait()

	count, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count == 0 {
		t.Fatal("no item stored, the race window was never opened")
	}
}

func TestNodeCheckRacesInsert(t *testing.T) {
	const (
		items      = 500
		delegation = 10
	)

	n := newNodeOn(t, noopStorage{}, delegation)

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < items; i++ {
			location := encoding.BASE64.CharAt(i % encoding.BASE64.Length())
			_ = n.insert(&domain.Item{
				Collection: collection,
				Location:   location,
				Id:         fmt.Sprintf("id-%d", i),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, encoding.BASE64.Root(), location, true)
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < items; i++ {
				_ = n.Check()
			}
		}()
	}

	wg.Wait()
}
