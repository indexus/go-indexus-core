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
func (noopStorage) Dirty() bool             { return false }
func (noopStorage) Stream(int) <-chan string {
	c := make(chan string)
	close(c)
	return c
}

// The owned BST stores a map per key, and writers mutate it in place. Any
// reader that keeps what Get returned races with them — and a concurrent map
// read/write is a runtime throw, not a warning. Ingress marks dirty zones on
// every item while Transfer and Refresh claim new ones, so the two run
// together constantly. Run under -race.
func TestOwnedTreeSurvivesConcurrentClaimsAndDirtyMarks(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 100)
	const coll = "demo"
	n.create(coll, encoding.BASE64.Root())
	c, _ := n.collections.Get(coll)

	locations := []string{"a", "ab", "abc", "b", "bc", "bcd"}
	for _, loc := range locations {
		c.EnsureSet(loc)
	}

	var wg sync.WaitGroup
	for _, loc := range locations {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n.own(c, domain.Ownership{loc: domain.Delegation{}})
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n.markOwnedDirtyForItem(coll, loc)
			}
		}()
	}
	wg.Wait()

	for _, loc := range locations {
		if !c.Owns(loc) {
			t.Fatalf("lost ownership of %s", loc)
		}
	}
}

func TestNodeCleanRacesMonitoring(t *testing.T) {
	const ops = 500

	n := newNodeOn(t, noopStorage{}, domain.DelegationSize())

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
			}, encoding.BASE64.Root(), nil, true)
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
			}, encoding.BASE64.Root(), nil, true)
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
