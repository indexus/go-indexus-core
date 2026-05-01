package core

import (
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

// noopStorage is a tiny in-memory domain.Storage so the test can boot a
// Node without importing the simulation mockup (which would create an
// import cycle since mockup depends on core).
type noopStorage struct{}

func (noopStorage) Exist() bool                  { return false }
func (noopStorage) Reset() error                 { return nil }
func (noopStorage) SaveCluster([]string) error   { return nil }
func (noopStorage) LoadCluster() ([]string, error) { return nil, nil }
func (noopStorage) Shards() ([]domain.Key, error) { return nil, nil }
func (noopStorage) LoadShard(domain.Key) ([]string, []string, error) {
	return nil, nil, nil
}
func (noopStorage) SnapshotShard(domain.Key, []string) error { return nil }
func (noopStorage) AppendShard(domain.Key, string)          {}
func (noopStorage) DropShard(domain.Key) error               { return nil }

func newTestNode(t *testing.T) *Node {
	t.Helper()

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	settings, err := NewSettings(name, 0, time.Second, time.Minute, domain.DelegationTreshold())
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := NewNode(settings, peer.NewContact, nil, noopStorage{})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// TestNodeRoutingReassignmentRace forces clean() (which used to swap the
// n.routing pointer) to run concurrently with monitoring readers
// (Routing/Acknowledged/Registered) and write paths (subscribe via
// register). With the old `n.routing = NewBST(...)` reassignment this is a
// data race; after the fix it must run cleanly under -race.
func TestNodeRoutingReassignmentRace(t *testing.T) {
	n := newTestNode(t)

	const ops = 500
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < ops; i++ {
			_ = n.clean()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = n.Routing()
				_, _ = n.Acknowledged()
				_, _ = n.Registered()
			}
		}()
	}

	wg.Wait()
}

// TestNodeOwnershipBrowseRace exercises monitoring's Ownership() while the
// Feed loop ingests items. Browse used to iterate Collection internals
// without taking c.mu, so it raced with insert -> add.
func TestNodeOwnershipBrowseRace(t *testing.T) {
	n := newTestNode(t)

	const items = 500
	stop := make(chan struct{})
	var wg sync.WaitGroup

	collName, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(stop)
		for i := 0; i < items; i++ {
			item := &domain.Item{
				Collection: collName,
				Location:   "@",
				Id:         "id",
				Metrics:    []float64{1, 2, 3, 4, 5},
			}
			n.insert(item, "@", "@")
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = n.Ownership()
				_, _ = n.Count()
			}
		}()
	}

	wg.Wait()
}
