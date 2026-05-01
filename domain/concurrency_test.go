package domain

import (
	"sync"
	"testing"
)

// TestBSTConcurrentInsertGetTraverseRemove hammers a BST with concurrent
// writers, readers and traversers. With the per-BST sync.Mutex this is
// expected to be race-free; the test exists to lock that property in.
func TestBSTConcurrentInsertGetTraverseRemove(t *testing.T) {
	const (
		writers = 8
		readers = 8
		ops     = 200
	)

	bst := NewBST[int]()

	keys := make([][]byte, writers*ops)
	for i := range keys {
		keys[i] = []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}
	}

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				bst.Insert(0, keys[w*ops+i], w*ops+i)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_, _ = bst.Get(0, keys[i])
				_ = bst.Nearest(0, keys[i])
				bst.Traverse(0, make([]byte, 4), func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}

// TestQueueConcurrentProducersConsumers exercises Queue's sync.Cond
// signaling under multiple producers and consumers.
func TestQueueConcurrentProducersConsumers(t *testing.T) {
	const (
		producers = 4
		consumers = 4
		perProd   = 500
	)

	q := NewQueue[int]()

	var prodWG sync.WaitGroup
	for p := 0; p < producers; p++ {
		p := p
		prodWG.Add(1)
		go func() {
			defer prodWG.Done()
			for i := 0; i < perProd; i++ {
				q.Add(p*perProd + i)
			}
		}()
	}

	got := make(chan int, producers*perProd)
	var consWG sync.WaitGroup
	stop := make(chan struct{})
	for c := 0; c < consumers; c++ {
		consWG.Add(1)
		go func() {
			defer consWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v, ok := q.Consume()
				if !ok {
					return
				}
				got <- v
			}
		}()
	}

	prodWG.Wait()

	want := producers * perProd
	seen := make(map[int]struct{}, want)
	for len(seen) < want {
		seen[<-got] = struct{}{}
	}
	close(stop)

	for c := 0; c < consumers; c++ {
		q.Add(-1)
	}
	consWG.Wait()
}

// TestCacheConcurrentGetSetRefresh checks Cache against parallel readers,
// writers and the periodic Refresh sweep.
func TestCacheConcurrentGetSetRefresh(t *testing.T) {
	const (
		writers = 4
		readers = 4
		ops     = 500
	)

	cache := NewCache()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				cache.Set("coll", string(rune('A'+w)), NewSet())
			}
		}()
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_, _ = cache.Get("coll", "A")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			_ = cache.Refresh(0)
		}
	}()
	wg.Wait()
}
