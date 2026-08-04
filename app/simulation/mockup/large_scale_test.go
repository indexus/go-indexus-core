package mockup

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

const seedChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"

// seedManyAcrossCollections inserts a large workload across `collections`
// distinct collections (each with a random ID) and `perColl` items each.
// Items use up-to-3-character locations derived from their index so that
// (a) no two items share `(collection,location,id)`, (b) the merged
// routing key spreads broadly across the keyspace, and (c) some
// sub-locations accumulate enough items to trigger Collection.Add
// delegation (which exercises the multi-area Transfer path).
func seedManyAcrossCollections(t *testing.T, n *core.Node, collections, perColl int) (totalItems int, collNames []string) {
	t.Helper()

	collNames = make([]string, collections)
	for i := range collNames {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		collNames[i] = name
	}

	for ci, c := range collNames {
		for j := 0; j < perColl; j++ {
			idx := ci*perColl + j
			loc := string(seedChars[idx%len(seedChars)]) +
				string(seedChars[(idx/len(seedChars))%len(seedChars)])
			_ = n.New(&domain.Item{
				Collection: c,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", idx),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, "@", loc)
			totalItems++
		}
	}
	return totalItems, collNames
}

// awaitDrained blocks until every node's queue is 0 for `steady`
// consecutive samples, or until `timeout` elapses (in which case it
// fatals the test). This is a coarser tool than awaitStability — used
// when we just want to be sure no Feed work is left, without checking
// snapshot equality.
func awaitDrained(t *testing.T, nodes []*core.Node, steady int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	calm := 0
	for time.Now().Before(deadline) {
		busy := false
		for _, n := range nodes {
			if n.Queue() > 0 {
				busy = true
				break
			}
		}
		if busy {
			calm = 0
		} else {
			calm++
			if calm >= steady {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	queues := make([]int, len(nodes))
	for i, n := range nodes {
		queues[i] = n.Queue()
	}
	t.Fatalf("queues did not drain within %s, lengths=%v", timeout, queues)
}

// -----------------------------------------------------------------------------
// 1) Large-scale burst: thousands of items, many collections, many
//    concurrent joiners. Verifies conservation and disjoint ownership at
//    a scale that actually exercises the trie-routing and per-collection
//    delegation paths.
// -----------------------------------------------------------------------------

func TestConvergence_LargeScale_Burst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping large-scale burst in short mode")
	}
	ResetNetwork()

	const (
		collections = 200
		perColl     = 20 // -> 4_000 items (enough to stress trie + delegation)
		burst       = 6
		delegation  = 80
		tickEvery   = 30 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 90 * time.Second
	)

	n1 := newConvergenceNode(t, delegation)

	t.Logf("seeding N1 with %d items across %d collections...", collections*perColl, collections)
	start := time.Now()
	expected, _ := seedManyAcrossCollections(t, n1, collections, perColl)
	awaitDrained(t, []*core.Node{n1}, 5, 60*time.Second)
	t.Logf("seeded %d items in %s", expected, time.Since(start))

	if got, _ := n1.Count(); got != expected {
		t.Fatalf("solo seed lost items: got %d want %d", got, expected)
	}

	tk := &ticker{}
	defer tk.stopAll()
	tk.start(t, n1, tickEvery)

	var spawned []*core.Node
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := newConvergenceNode(t, delegation, n1)
			mu.Lock()
			spawned = append(spawned, n)
			mu.Unlock()
		}()
	}
	wg.Wait()

	nodes := []*core.Node{n1}
	for _, n := range spawned {
		tk.start(t, n, tickEvery)
		nodes = append(nodes, n)
	}

	t.Logf("waiting for %d-node cluster to converge...", len(nodes))
	rebalanceStart := time.Now()
	awaitStability(t, nodes, expected, steadyFor, timeout)
	t.Logf("converged in %s", time.Since(rebalanceStart))

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("large burst lost items: got %d want %d, per-node=%v", total, expected, counts)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	counts := make([]int, len(nodes))
	for i, n := range nodes {
		counts[i], _ = n.Count()
	}
	loaded := 0
	min, max := counts[0], counts[0]
	for _, c := range counts {
		if c > 0 {
			loaded++
		}
		if c < min {
			min = c
		}
		if c > max {
			max = c
		}
	}
	t.Logf("large-scale final per-node counts (%d nodes, %d items): min=%d max=%d spread=%.2fx loaded=%d",
		len(nodes), expected, min, max, float64(max+1)/float64(min+1), loaded)

	if loaded < len(nodes)/2 {
		t.Errorf("expected most of %d nodes to carry data, only %d do", len(nodes), loaded)
	}
}

// -----------------------------------------------------------------------------
// 2) Mixed inserts during a join storm. A producer goroutine keeps
//    inserting new items into N1 while joins happen and rebalance is
//    underway. After the producer stops, we wait for stability and check
//    that the count of items stored equals the count of items inserted
//    (no inserts dropped, no transfers lost, no double-insertion).
// -----------------------------------------------------------------------------

func TestConvergence_MixedInsertsAndRebalance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping mixed insert+rebalance in short mode")
	}
	ResetNetwork()

	const (
		seedCollections = 100
		seedPerColl     = 10  // -> 1000 items pre-seed
		liveCollections = 50
		liveItemsTotal  = 1500
		burst           = 4
		delegation      = 60
		tickEvery       = 30 * time.Millisecond
		producerRate    = 400 * time.Microsecond
		steadyFor       = 2 * time.Second
		timeout         = 60 * time.Second
	)

	n1 := newConvergenceNode(t, delegation)

	t.Logf("pre-seeding N1 with %d items...", seedCollections*seedPerColl)
	preSeeded, _ := seedManyAcrossCollections(t, n1, seedCollections, seedPerColl)
	awaitDrained(t, []*core.Node{n1}, 5, 30*time.Second)
	if got, _ := n1.Count(); got != preSeeded {
		t.Fatalf("pre-seed lost items: got %d want %d", got, preSeeded)
	}

	// Build live-write collection IDs ahead of time so we don't fight
	// for the encoding RNG inside the hot loop.
	liveColls := make([]string, liveCollections)
	for i := range liveColls {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		liveColls[i] = name
	}

	tk := &ticker{}
	defer tk.stopAll()
	tk.start(t, n1, tickEvery)

	// Spawn the live-insert producer. It writes to whichever node it can
	// reach, picked randomly from the current cluster snapshot. Items go
	// in through the regular client-side path (n.New), so the system
	// must route them to the right owner just like a real client would.
	var attempts atomic.Int64 // bumped before bounds check (may overshoot)
	var inserted atomic.Int64 // bumped only after a real write went out
	ctx, cancel := context.WithCancel(context.Background())

	currentNodes := func() []*core.Node {
		out := []*core.Node{n1}
		network.mu.RLock()
		for _, n := range network.nodes {
			if n.Name() == n1.Name() {
				continue
			}
			out = append(out, n)
		}
		network.mu.RUnlock()
		return out
	}

	prodWg := sync.WaitGroup{}
	prodWg.Add(1)
	go func() {
		defer prodWg.Done()
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		ticker := time.NewTicker(producerRate)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cluster := currentNodes()
				if len(cluster) == 0 {
					continue
				}
				target := cluster[rng.Intn(len(cluster))]
				idx := attempts.Add(1) - 1
				if idx >= int64(liveItemsTotal) {
					return
				}
				coll := liveColls[idx%int64(liveCollections)]
				loc := string(seedChars[idx%int64(len(seedChars))]) +
					string(seedChars[(idx/int64(len(seedChars)))%int64(len(seedChars))])
				if err := target.New(&domain.Item{
					Collection: coll,
					Location:   loc,
					Id:         fmt.Sprintf("live-%d", idx),
					Metrics:    []float64{1, 2, 3, 4, 5},
				}, "@", loc); err == nil {
					inserted.Add(1)
				}
			}
		}
	}()

	// Now stage the join storm in parallel with the producer.
	var spawned []*core.Node
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n := newConvergenceNode(t, delegation, n1)
			mu.Lock()
			spawned = append(spawned, n)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, n := range spawned {
		tk.start(t, n, tickEvery)
	}

	// Wait for the producer to finish injecting all live items.
	for attempts.Load() < int64(liveItemsTotal) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	prodWg.Wait()

	nodes := append([]*core.Node{n1}, spawned...)
	expected := preSeeded + int(inserted.Load())
	t.Logf("producer finished, %d live items inserted (target %d), expected total=%d",
		inserted.Load(), liveItemsTotal, expected)

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("mixed insert+rebalance lost items: got %d want %d (preseed=%d live=%d), per-node=%v",
			total, expected, preSeeded, inserted.Load(), counts)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	counts := make([]int, len(nodes))
	for i, n := range nodes {
		counts[i], _ = n.Count()
	}
	t.Logf("mixed final per-node counts (%d nodes, %d items): %v", len(nodes), expected, counts)
}

// -----------------------------------------------------------------------------
// 3) Lossy transfer recovery. Drops a configurable percentage of
//    Transfer calls and verifies that the system still converges with
//    every item accounted for. The fix in core/jobs.go re-queues items
//    through the regular insert path on Transfer failure, so transient
//    transport drops cost only extra rounds — not data.
//
//    Two regimes are tested:
//      moderate (20%): should converge cleanly within a few cycles
//      severe (50%):   should still converge given enough time
// -----------------------------------------------------------------------------

func TestConvergence_LossyTransfer_RecoversWithoutLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping lossy transfer test in short mode")
	}

	cases := []struct {
		name      string
		dropPct   int
		timeout   time.Duration
		steadyFor time.Duration
	}{
		{name: "moderate_20pct", dropPct: 20, timeout: 45 * time.Second, steadyFor: 2 * time.Second},
		{name: "severe_50pct", dropPct: 50, timeout: 60 * time.Second, steadyFor: 3 * time.Second},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ResetNetwork()

			const (
				collections = 100
				perColl     = 8 // -> 800 items
				burst       = 3
				delegation  = 60
				tickEvery   = 30 * time.Millisecond
			)

			n1 := newConvergenceNode(t, delegation)
			expected, _ := seedManyAcrossCollections(t, n1, collections, perColl)
			awaitDrained(t, []*core.Node{n1}, 5, 30*time.Second)
			if got, _ := n1.Count(); got != expected {
				t.Fatalf("solo seed lost items: got %d want %d", got, expected)
			}

			network.SetTransferDropRate(tc.dropPct)
			defer network.SetTransferDropRate(0)

			tk := &ticker{}
			defer tk.stopAll()
			tk.start(t, n1, tickEvery)

			var spawned []*core.Node
			var mu sync.Mutex
			var wg sync.WaitGroup
			for i := 0; i < burst; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					n := newConvergenceNode(t, delegation, n1)
					mu.Lock()
					spawned = append(spawned, n)
					mu.Unlock()
				}()
			}
			wg.Wait()
			for _, n := range spawned {
				tk.start(t, n, tickEvery)
			}

			nodes := append([]*core.Node{n1}, spawned...)

			awaitStability(t, nodes, expected, tc.steadyFor, tc.timeout)

			total := totalCount(t, nodes)
			ok, dropped := network.TransferStats()
			counts := make([]int, len(nodes))
			for i, n := range nodes {
				counts[i], _ = n.Count()
			}

			t.Logf("drop rate=%d%%, transfers ok=%d dropped=%d", tc.dropPct, ok, dropped)
			t.Logf("per-node counts (%d nodes, %d items): %v", len(nodes), expected, counts)

			if dropped == 0 {
				t.Fatalf("test setup error: no transfers were dropped despite drop_pct=%d", tc.dropPct)
			}
			if total != expected {
				t.Fatalf("RECOVERY FAILED: drop rate %d%% leaked %d items (expected=%d observed=%d, transfers ok=%d dropped=%d)",
					tc.dropPct, expected-total, expected, total, ok, dropped)
			}
			assertOwnershipDisjointAndComplete(t, nodes)
			t.Logf("RECOVERED: every item present despite %d dropped transfers", dropped)
		})
	}
}
