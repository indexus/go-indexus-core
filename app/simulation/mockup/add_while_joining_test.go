package mockup

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// TestConcurrentIngestWhileJoining starts three nodes, floods inserts from
// many goroutines, joins two more nodes mid-flight, then asserts global
// completude and XOR ownership.
func TestConcurrentIngestWhileJoining(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 55
		tickEvery   = 40 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 90 * time.Second
		workers     = 24
		perWorker   = 40 // 960 items
	)

	n1 := newConvergenceNode(t, delegation)
	n2 := newConvergenceNode(t, delegation, n1)
	n3 := newConvergenceNode(t, delegation, n1)

	coll, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	tk := &ticker{}
	defer tk.stopAll()
	for _, n := range []*core.Node{n1, n2, n3} {
		tk.start(t, n, tickEvery)
	}

	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	peers := []*core.Node{n1, n2, n3}

	var wg sync.WaitGroup
	var inserts atomic.Int64
	var insertMu sync.Mutex
	var insertErr error

	startInserts := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			<-startInserts
			for j := 0; j < perWorker; j++ {
				idx := worker*perWorker + j
				loc := string(chars[idx%len(chars)]) + string(chars[(idx/len(chars))%len(chars)])
				item := &domain.Item{
					Collection: coll,
					Location:   loc,
					Id:         fmt.Sprintf("cij-%d", idx),
					Metrics:    []float64{1, 2, 3, 4, 5},
				}
				// Round-robin across peers (math/rand.Rand is not goroutine-safe).
				target := peers[(worker+j)%len(peers)]
				if err := target.New(item, "@", loc); err != nil {
					insertMu.Lock()
					if insertErr == nil {
						insertErr = err
					}
					insertMu.Unlock()
					return
				}
				inserts.Add(1)
			}
		}(w)
	}

	close(startInserts)
	time.Sleep(45 * time.Millisecond)

	n4 := newConvergenceNode(t, delegation, n1)
	tk.start(t, n4, tickEvery)
	time.Sleep(35 * time.Millisecond)

	n5 := newConvergenceNode(t, delegation, n2)
	tk.start(t, n5, tickEvery)

	wg.Wait()
	if insertErr != nil {
		t.Fatalf("concurrent insert: %v", insertErr)
	}

	expected := int(inserts.Load())
	if expected != workers*perWorker {
		t.Fatalf("insert count mismatch: got %d want %d", expected, workers*perWorker)
	}

	drainQueues(t, []*core.Node{n1, n2, n3, n4, n5}, 8, 30*time.Second)

	nodes := []*core.Node{n1, n2, n3, n4, n5}
	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("total after concurrent join ingest: got %d want %d", total, expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	for _, n := range nodes {
		for _, line := range n.Check() {
			t.Errorf("node %s Check: %s", n.Name(), line)
		}
	}
}
