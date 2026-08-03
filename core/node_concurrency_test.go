package core

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

// noopStorage keeps the test inside package core; importing the simulation
// mockup would create a cycle.
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

func newTestNode(t *testing.T, delegation int) *Node {
	t.Helper()

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	settings, err := NewSettings(name, 0, time.Second, time.Minute, delegation)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := NewNode(settings, peer.NewContact, nil, noopStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// clean() empties the routing tree while monitoring reads it.
func TestNodeCleanRacesMonitoring(t *testing.T) {
	const ops = 500

	n := newTestNode(t, domain.DelegationTreshold())

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

// Ownership() browses collection internals while items are being ingested.
// Locations and ids are unique so the writer really grows the collection
// (shrink splits sets, delegation adds ownerships) instead of overwriting one
// entry.
func TestNodeInsertRacesOwnership(t *testing.T) {
	const (
		items      = 500
		delegation = 10
	)

	n := newTestNode(t, delegation)

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
			}, encoding.BASE64.Root(), location)
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

	// Guard against a vacuous test: the writer must have stored something.
	count, err := n.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count == 0 {
		t.Fatal("no item stored, the race window was never opened")
	}
}

// Check() browses a collection and looks up the owned tree, while the ingest
// path locks the owned tree and then the collection. Taking both locks in
// opposite orders deadlocks, so this test hangs instead of failing.
func TestNodeCheckRacesInsert(t *testing.T) {
	const (
		items      = 500
		delegation = 10
	)

	n := newTestNode(t, delegation)

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
			}, encoding.BASE64.Root(), location)
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
