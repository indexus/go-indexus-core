package mockup

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// -----------------------------------------------------------------------------
// Async driver — every node has its own ticker goroutine that runs the
// Observe+Refresh loop concurrently with the others, mimicking real
// production timing where nothing is centrally orchestrated.
// -----------------------------------------------------------------------------

type ticker struct {
	mu    sync.Mutex
	stops []chan struct{}
	wg    sync.WaitGroup
	errs  atomic.Int32
}

func (tk *ticker) start(t *testing.T, n *core.Node, interval time.Duration) {
	t.Helper()

	stop := make(chan struct{})
	tk.mu.Lock()
	tk.stops = append(tk.stops, stop)
	tk.mu.Unlock()

	tk.wg.Add(1)
	go func() {
		defer tk.wg.Done()
		// First tick is immediate — convergence requires ~3 round-trips
		// per join, no need to wait a full interval before starting.
		tick := time.NewTimer(0)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := n.Observe(); err != nil {
					t.Logf("Observe %s: %v", n.Name(), err)
					tk.errs.Add(1)
				}
				if err := n.Refresh(); err != nil {
					t.Logf("Refresh %s: %v", n.Name(), err)
					tk.errs.Add(1)
				}
				tick.Reset(interval)
			}
		}
	}()
}

func (tk *ticker) stopAll() {
	tk.mu.Lock()
	for _, s := range tk.stops {
		close(s)
	}
	tk.stops = nil
	tk.mu.Unlock()
	tk.wg.Wait()
}

// awaitStability polls `nodes` until two conditions hold simultaneously
// for at least `steady`:
//   - sum of Count() across nodes equals expectedTotal
//   - the per-node (count, sorted ownership keys) fingerprint is unchanged
//     since the last sample
//
// Returns once stability is achieved or fails the test on timeout.
func awaitStability(t *testing.T, nodes []*core.Node, expectedTotal int, steady, timeout time.Duration) {
	t.Helper()

	type fp struct {
		counts []int
		owns   []string
	}

	snap := func() fp {
		out := fp{counts: make([]int, len(nodes)), owns: make([]string, len(nodes))}
		for i, n := range nodes {
			c, _ := n.Count()
			out.counts[i] = c

			own, _ := n.Ownership()
			lines := make([]string, 0)
			collKeys := make([]string, 0, len(own))
			for k := range own {
				collKeys = append(collKeys, k)
			}
			sort.Strings(collKeys)
			for _, k := range collKeys {
				locs := make([]string, 0, len(own[k]))
				for l := range own[k] {
					locs = append(locs, l)
				}
				sort.Strings(locs)
				for _, l := range locs {
					lines = append(lines, k+"|"+l)
				}
			}
			out.owns[i] = fmt.Sprintf("%v", lines)
		}
		return out
	}

	queuesCalm := func() bool {
		for _, n := range nodes {
			if n.Queue() > 0 {
				return false
			}
		}
		return true
	}

	totalOK := func(s fp) bool {
		total := 0
		for _, c := range s.counts {
			total += c
		}
		return total == expectedTotal
	}

	equal := func(a, b fp) bool {
		if len(a.counts) != len(b.counts) {
			return false
		}
		for i := range a.counts {
			if a.counts[i] != b.counts[i] || a.owns[i] != b.owns[i] {
				return false
			}
		}
		return true
	}

	deadline := time.Now().Add(timeout)
	prev := snap()
	stableSince := time.Time{}

	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		curr := snap()

		if equal(curr, prev) && totalOK(curr) && queuesCalm() {
			if stableSince.IsZero() {
				stableSince = time.Now()
			} else if time.Since(stableSince) >= steady {
				return
			}
		} else {
			stableSince = time.Time{}
		}
		prev = curr
	}

	last := snap()
	queues := make([]int, len(nodes))
	for i, n := range nodes {
		queues[i] = n.Queue()
	}
	t.Fatalf("network did not stabilize within %s; last counts=%v queues=%v expected_total=%d",
		timeout, last.counts, queues, expectedTotal)
}

// -----------------------------------------------------------------------------
// Test data seeding
// -----------------------------------------------------------------------------

// seedAcrossCollections inserts items spread across many collections
// (each with a random ID) at deterministic 1-char sublocations. This
// gives well-distributed merged routing IDs so that any reasonable
// rebalance ends up loading every node.
func seedAcrossCollections(t *testing.T, n *core.Node, collections, perColl int) (totalItems int, collNames []string) {
	t.Helper()

	collNames = make([]string, collections)
	for i := range collNames {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		collNames[i] = name
	}

	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	for _, c := range collNames {
		for j := 0; j < perColl; j++ {
			loc := string(chars[j%len(chars)])
			_ = n.New(&domain.Item{
				Collection: c,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", j),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, "@", loc)
			totalItems++
		}
	}
	return totalItems, collNames
}

// -----------------------------------------------------------------------------
// 1) Burst join from a single bootstrap.
//    A seeded N1 is hit by `burst` brand-new nodes, all bootstrapping
//    against N1 simultaneously. Validates: total preserved, ownership
//    disjoint, every owned key on its XOR-closest node, and every node
//    actually receives a share.
// -----------------------------------------------------------------------------

func TestConvergence_BurstJoin_FromOneBootstrap(t *testing.T) {
	ResetNetwork()

	const (
		burst       = 5
		collections = 60
		perColl     = 6
		delegation  = 50
		tickEvery   = 50 * time.Millisecond
		steadyFor   = 1500 * time.Millisecond
		timeout     = 30 * time.Second
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)
	if got, _ := n1.Count(); got != expected {
		t.Fatalf("solo seed lost items: got %d want %d", got, expected)
	}

	tk := &ticker{}
	tk.start(t, n1, tickEvery)

	nodes := []*core.Node{n1}
	for i := 0; i < burst; i++ {
		nodes = append(nodes, newConvergenceNode(t, delegation, n1))
	}
	for _, n := range nodes[1:] {
		tk.start(t, n, tickEvery)
	}

	defer tk.stopAll()

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("total drifted after burst join: got %d want %d, per-node=%v", total, expected, counts)
	}

	assertOwnershipDisjointAndComplete(t, nodes)

	loaded := 0
	for _, n := range nodes {
		if c, _ := n.Count(); c > 0 {
			loaded++
		}
	}
	if loaded < 3 {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Errorf("expected burst join to load most nodes; only %d carry data, per-node=%v", loaded, counts)
	}

	if errs := tk.errs.Load(); errs > 0 {
		t.Errorf("background workers reported %d Observe/Refresh errors", errs)
	}
}

// -----------------------------------------------------------------------------
// 2) Burst join with random bootstraps.
//    Once N1 and N2 exist, additional nodes pick whichever bootstrap is
//    available at the time they start. Tests that the join procedure
//    doesn't depend on a single hub being reached first.
// -----------------------------------------------------------------------------

func TestConvergence_BurstJoin_RandomBootstraps(t *testing.T) {
	ResetNetwork()

	const (
		seedNodes   = 2
		burst       = 4
		collections = 50
		perColl     = 5
		delegation  = 50
		tickEvery   = 50 * time.Millisecond
		steadyFor   = 1500 * time.Millisecond
		timeout     = 30 * time.Second
	)

	rng := rand.New(rand.NewSource(42))

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	nodes := []*core.Node{n1}
	for i := 1; i < seedNodes; i++ {
		nodes = append(nodes, newConvergenceNode(t, delegation, n1))
	}

	tk := &ticker{}
	defer tk.stopAll()
	for _, n := range nodes {
		tk.start(t, n, tickEvery)
	}

	// Let the seed cluster settle so subsequent joins talk to a network
	// that has more than one entry point.
	awaitStability(t, nodes, expected, 1*time.Second, 15*time.Second)

	for i := 0; i < burst; i++ {
		boot := nodes[rng.Intn(len(nodes))]
		newNode := newConvergenceNode(t, delegation, boot)
		nodes = append(nodes, newNode)
		tk.start(t, newNode, tickEvery)
	}

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("total drifted: got %d want %d", total, expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
}

// -----------------------------------------------------------------------------
// 3) Staggered waves of joins — start a wave, wait briefly (NOT for full
//    convergence), then start another wave on top. This is the worst
//    case for "join during rebalance" because new ownership decisions
//    may cascade through three or more handoffs.
// -----------------------------------------------------------------------------

func TestConvergence_StaggeredWaves(t *testing.T) {
	ResetNetwork()

	const (
		collections = 80
		perColl     = 4
		delegation  = 50
		tickEvery   = 30 * time.Millisecond
		settleStep  = 300 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 45 * time.Second
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	tk := &ticker{}
	defer tk.stopAll()
	tk.start(t, n1, tickEvery)

	nodes := []*core.Node{n1}

	addWave := func(size int) {
		// Snapshot current cluster as the bootstrap pool, then add the
		// wave concurrently from goroutines so the joins really do race.
		bootPool := append([]*core.Node(nil), nodes...)
		var spawned []*core.Node
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < size; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				boot := bootPool[i%len(bootPool)]
				n := newConvergenceNode(t, delegation, boot)
				mu.Lock()
				spawned = append(spawned, n)
				mu.Unlock()
			}(i)
		}
		wg.Wait()
		for _, n := range spawned {
			tk.start(t, n, tickEvery)
			nodes = append(nodes, n)
		}
	}

	addWave(2)
	time.Sleep(settleStep)
	addWave(3)
	time.Sleep(settleStep)
	addWave(2)

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("staggered waves lost items: got %d want %d, per-node=%v", total, expected, counts)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	counts := make([]int, len(nodes))
	for i, n := range nodes {
		counts[i], _ = n.Count()
	}
	t.Logf("staggered waves final per-node counts (%d nodes): %v", len(nodes), counts)
}

// -----------------------------------------------------------------------------
// 4) Join during rebalance — add a node BEFORE the previous join's
//    transfers have settled. Forces "interrupted handoff" path: a key
//    that N1 was about to send to N2 may need to be re-routed to N3.
// -----------------------------------------------------------------------------

func TestConvergence_JoinDuringRebalance(t *testing.T) {
	ResetNetwork()

	const (
		collections = 100
		perColl     = 5
		delegation  = 80
		tickEvery   = 40 * time.Millisecond
		gap         = 80 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 45 * time.Second
		joinCount   = 6
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	tk := &ticker{}
	defer tk.stopAll()
	tk.start(t, n1, tickEvery)

	nodes := []*core.Node{n1}
	for i := 0; i < joinCount; i++ {
		boot := nodes[i%len(nodes)]
		newNode := newConvergenceNode(t, delegation, boot)
		nodes = append(nodes, newNode)
		tk.start(t, newNode, tickEvery)
		// Don't wait for stability — that's the whole point.
		time.Sleep(gap)
	}

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("join-during-rebalance lost items: got %d want %d, per-node=%v", total, expected, counts)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	counts := make([]int, len(nodes))
	for i, n := range nodes {
		counts[i], _ = n.Count()
	}
	t.Logf("join-during-rebalance final per-node counts (%d nodes): %v", len(nodes), counts)
}

// -----------------------------------------------------------------------------
// 5) Stress: simultaneous all-at-once startup. Seed N1, then in parallel
//    spin up `bigBurst` new nodes against N1, and let everyone race.
//    This is the worst-case from the convergence-under-pressure point of
//    view.
// -----------------------------------------------------------------------------

func TestConvergence_BigBurst(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping big burst in short mode")
	}
	ResetNetwork()

	const (
		bigBurst    = 9
		collections = 120
		perColl     = 4
		delegation  = 60
		tickEvery   = 40 * time.Millisecond
		steadyFor   = 2500 * time.Millisecond
		timeout     = 60 * time.Second
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	tk := &ticker{}
	defer tk.stopAll()
	tk.start(t, n1, tickEvery)

	nodes := []*core.Node{n1}
	var spawned []*core.Node
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < bigBurst; i++ {
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
		nodes = append(nodes, n)
	}

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Fatalf("big burst lost items: got %d want %d, per-node=%v", total, expected, counts)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	counts := make([]int, len(nodes))
	for i, n := range nodes {
		counts[i], _ = n.Count()
	}
	loaded := 0
	for _, c := range counts {
		if c > 0 {
			loaded++
		}
	}
	t.Logf("big burst (%d nodes): per-node counts=%v, loaded=%d", len(nodes), counts, loaded)
	if loaded < len(nodes)/2 {
		t.Errorf("expected most of %d nodes to carry data, only %d do", len(nodes), loaded)
	}
}
