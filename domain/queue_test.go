package domain

import (
	"sync"
	"testing"
)

// The reset in Consume only fires on an empty queue, so a queue that stays busy
// — the only kind that matters under load — kept every element it had ever held
// alive in the backing array.
func TestQueueReclaimsConsumedPrefix(t *testing.T) {
	q := NewQueue[int]()

	q.Add(0)
	for i := 1; i <= 20*compactFloor; i++ {
		q.Add(i)
		if _, exist := q.TryConsume(); !exist {
			t.Fatal("queue handed back nothing while an element was pending")
		}
	}

	if pending := q.Length(); pending != 1 {
		t.Fatalf("pending=%d want 1", pending)
	}

	q.mu.Lock()
	held := len(q.data)
	q.mu.Unlock()

	if held > 2*compactFloor {
		t.Fatalf("consumed prefix never reclaimed: backing array holds %d for 1 pending", held)
	}
}

func TestQueueKeepsOrderAcrossCompaction(t *testing.T) {
	q := NewQueue[int]()

	for i := 0; i < 4*compactFloor; i++ {
		q.Add(i)
	}
	for i := 0; i < 4*compactFloor; i++ {
		got, exist := q.TryConsume()
		if !exist {
			t.Fatalf("queue ran dry at %d", i)
		}
		if got != i {
			t.Fatalf("compaction reordered the queue: got %d want %d", got, i)
		}
	}
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

func TestQueueTryAddBackpressure(t *testing.T) {
	q := NewQueue[int]()
	if !q.TryAdd(1, 2) || !q.TryAdd(2, 2) {
		t.Fatal("expected first two adds to succeed")
	}
	if q.TryAdd(3, 2) {
		t.Fatal("expected backpressure on third add")
	}
	if q.Length() != 2 {
		t.Fatalf("length=%d want 2", q.Length())
	}
}
