package domain

import (
	"sync"
	"testing"
)

func TestBSTConcurrentAccess(t *testing.T) {
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
			id := make([]byte, 4)
			for i := 0; i < ops; i++ {
				_, _ = bst.Get(0, keys[i])
				_ = bst.Nearest(0, keys[i])
				bst.Traverse(0, id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}

// Reset must be observed atomically: readers may see a full or an empty tree,
// never a half-replaced one.
func TestBSTConcurrentReset(t *testing.T) {
	const ops = 500

	bst := NewBST[int]()
	id := make([]byte, 4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			bst.Insert(0, []byte{byte(i), 0, 0, 0}, i)
			bst.Reset()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_ = bst.Nearest(0, id)
				bst.Traverse(0, id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}

func TestQueueConcurrentProducersConsumers(t *testing.T) {
	const (
		producers = 4
		consumers = 4
		perProd   = 500
		total     = producers * perProd
		done      = -1
	)

	q := NewQueue[int]()
	got := make(chan int, total)

	var wg sync.WaitGroup

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProd; i++ {
				q.Add(p*perProd + i)
			}
		}()
	}

	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				v, ok := q.Consume()
				if !ok || v == done {
					return
				}
				got <- v
			}
		}()
	}

	seen := make(map[int]struct{}, total)
	for len(seen) < total {
		seen[<-got] = struct{}{}
	}

	// Every item has been observed, so the queue now only hands out sentinels
	// and each consumer takes exactly one before returning.
	for c := 0; c < consumers; c++ {
		q.Add(done)
	}
	wg.Wait()
}

func TestCacheConcurrentGetSetRefresh(t *testing.T) {
	const (
		writers = 4
		readers = 4
		ops     = 500
	)

	cache := NewCache()

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
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
			_ = cache.Refresh(0, 1.0)
		}
	}()

	wg.Wait()
}
