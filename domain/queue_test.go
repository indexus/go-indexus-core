package domain

import "testing"

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
