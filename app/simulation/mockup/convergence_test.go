package mockup

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// newConvergenceNode boots a Node in the in-process mockup network.
// All nodes share a small delegation threshold so subtree splits actually
// happen during the test instead of waiting for 1000+ items.
func newConvergenceNode(t *testing.T, delegation int, bootstraps ...*core.Node) *core.Node {
	t.Helper()

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	contacts := make([]domain.Contact, 0, len(bootstraps))
	for _, b := range bootstraps {
		contacts = append(contacts, NewContact(b.Name(), b.IPs(), b.Port()))
	}

	settings, err := core.NewSettings(name, 0, time.Second, time.Minute, delegation)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}

	node, err := core.NewNode(settings, NewContact, contacts, NewStorage(), nil)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}

	network.Join(node)

	go func() { _ = node.Feed() }() // leak: queue.Consume blocks forever; fine for the test process.

	return node
}

// xorDistance returns the byte-wise XOR of two equal-length byte slices,
// representing the Kademlia-style distance between them. We compare these
// lexicographically (most significant byte first), which is exactly the
// XOR magnitude order.
func xorDistance(a, b []byte) []byte {
	if len(a) != len(b) {
		panic(fmt.Sprintf("xor length mismatch %d vs %d", len(a), len(b)))
	}
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func cmpBytes(a, b []byte) int {
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}
	return 0
}

// closestNode returns the node whose ID is XOR-closest to id.
func closestNode(id []byte, nodes []*core.Node) *core.Node {
	var best *core.Node
	var bestDist []byte
	for i, n := range nodes {
		d := xorDistance(id, n.ID())
		if i == 0 || cmpBytes(d, bestDist) < 0 {
			best = n
			bestDist = d
		}
	}
	return best
}

// drainQueues spins until every node's queue is empty for `steady`
// consecutive samples or `timeout` elapses. The "steady" requirement
// avoids declaring success while a transfer is still mid-flight.
func drainQueues(t *testing.T, nodes []*core.Node, steady int, timeout time.Duration) {
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
		time.Sleep(20 * time.Millisecond)
	}
	for _, n := range nodes {
		if q := n.Queue(); q > 0 {
			t.Fatalf("queue did not drain on node %s within %s, length=%d", n.Name(), timeout, q)
		}
	}
}

// converge runs Observe+Refresh on every node repeatedly until the global
// state stops changing for `steady` consecutive rounds. Returns the round
// count it took to settle.
//
// A "warmup" of `warmup` rounds runs unconditionally before the
// stability check engages: rebalancing only kicks in once each node's
// routing table has actually learned about the others, which requires
// several Observe/Refresh interleavings (Observe -> ack -> register ->
// Refresh.clean -> subscribe -> next Refresh.control sees the peer).
// Without a warmup, "no change" can simply mean "no transfer has been
// attempted yet", and we'd report a false positive.
func converge(t *testing.T, nodes []*core.Node, warmup, steady, maxRounds int) int {
	t.Helper()

	type fingerprint struct {
		counts     []int
		ownerships []string
	}

	snapshot := func() fingerprint {
		fp := fingerprint{
			counts:     make([]int, len(nodes)),
			ownerships: make([]string, len(nodes)),
		}
		for i, n := range nodes {
			c, _ := n.Count()
			fp.counts[i] = c

			own, _ := n.Ownership()
			collKeys := make([]string, 0, len(own))
			for c := range own {
				collKeys = append(collKeys, c)
			}
			sort.Strings(collKeys)
			lines := make([]string, 0)
			for _, c := range collKeys {
				locs := make([]string, 0, len(own[c]))
				for l := range own[c] {
					locs = append(locs, l)
				}
				sort.Strings(locs)
				for _, l := range locs {
					lines = append(lines, c+"|"+l)
				}
			}
			fp.ownerships[i] = fmt.Sprintf("%v", lines)
		}
		return fp
	}

	equal := func(a, b fingerprint) bool {
		if len(a.counts) != len(b.counts) {
			return false
		}
		for i := range a.counts {
			if a.counts[i] != b.counts[i] || a.ownerships[i] != b.ownerships[i] {
				return false
			}
		}
		return true
	}

	runRound := func() {
		for _, n := range nodes {
			if err := n.Observe(); err != nil {
				t.Fatalf("Observe %s: %v", n.Name(), err)
			}
			if err := n.Refresh(); err != nil {
				t.Fatalf("Refresh %s: %v", n.Name(), err)
			}
		}
		drainQueues(t, nodes, 3, 10*time.Second)
	}

	for r := 0; r < warmup; r++ {
		runRound()
	}

	prev := snapshot()
	calm := 0
	for round := warmup + 1; round <= maxRounds; round++ {
		runRound()

		curr := snapshot()
		if equal(curr, prev) {
			calm++
			if calm >= steady {
				return round
			}
		} else {
			calm = 0
		}
		prev = curr
	}
	t.Fatalf("network did not converge within %d rounds; last counts=%v", maxRounds, prev.counts)
	return maxRounds
}

// totalCount sums Count() across all nodes.
func totalCount(t *testing.T, nodes []*core.Node) int {
	t.Helper()
	total := 0
	for _, n := range nodes {
		c, err := n.Count()
		if err != nil {
			t.Fatalf("Count: %v", err)
		}
		total += c
	}
	return total
}

// assertOwnershipDisjointAndComplete verifies that every (collection,
// location) appears as an ownership at exactly one node, and that the node
// is the XOR-closest one to the merged key. This is the post-convergence
// invariant we expect from a healthy growing network.
func assertOwnershipDisjointAndComplete(t *testing.T, nodes []*core.Node) {
	t.Helper()

	type owner struct {
		collection string
		location   string
		node       string
	}

	owned := make(map[string][]owner)
	for _, n := range nodes {
		own, err := n.Ownership()
		if err != nil {
			t.Fatalf("Ownership %s: %v", n.Name(), err)
		}
		for coll, locs := range own {
			for loc := range locs {
				k := coll + "|" + loc
				owned[k] = append(owned[k], owner{collection: coll, location: loc, node: n.Name()})
			}
		}
	}

	keys := make([]string, 0, len(owned))
	for k := range owned {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		holders := owned[k]
		if len(holders) != 1 {
			names := make([]string, 0, len(holders))
			for _, h := range holders {
				names = append(names, h.node)
			}
			t.Errorf("ownership %s held by %d nodes: %v (expected exactly 1)", k, len(holders), names)
			continue
		}
		h := holders[0]
		id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, h.location, h.collection)
		if err != nil {
			t.Fatalf("MergeEncodings: %v", err)
		}
		expected := closestNode(id, nodes)
		if expected.Name() != h.node {
			t.Errorf("ownership %s held by %s but XOR-closest is %s", k, h.node, expected.Name())
		}
	}
}

// -----------------------------------------------------------------------------
// Tests
// -----------------------------------------------------------------------------

// TestConvergence_TwoNodes_TotalPreserved is the smallest possible
// growth scenario: insert N items into a single-node network, then add a
// second node and run convergence rounds. After settling, the union of
// counts must equal N (no loss, no duplication) and the ownership table
// must be disjoint.
func TestConvergence_TwoNodes_TotalPreserved(t *testing.T) {
	ResetNetwork()

	const items = 200
	const delegation = 25

	n1 := newConvergenceNode(t, delegation)

	// Single collection, items spread across many sublocations to force
	// at least some delegation.
	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	for i := 0; i < items; i++ {
		loc := string([]byte{chars[i%len(chars)], chars[(i/len(chars))%len(chars)]})
		_ = n1.New(&domain.Item{
			Collection: collection,
			Location:   loc,
			Id:         fmt.Sprintf("id-%d", i),
			Metrics:    []float64{1, 2, 3, 4, 5},
		}, "@", loc)
	}

	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	got, err := n1.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != items {
		t.Fatalf("solo node lost items: got %d, want %d", got, items)
	}

	n2 := newConvergenceNode(t, delegation, n1)
	nodes := []*core.Node{n1, n2}

	rounds := converge(t, nodes, 4, 2, 30)
	t.Logf("converged in %d rounds", rounds)

	if total := totalCount(t, nodes); total != items {
		c1, _ := n1.Count()
		c2, _ := n2.Count()
		t.Fatalf("item total drifted after rebalance: got %d (n1=%d, n2=%d), want %d",
			total, c1, c2, items)
	}

	assertOwnershipDisjointAndComplete(t, nodes)
}

// TestConvergence_GrowingNetwork_Sequential adds nodes one at a time and
// checks invariants after each addition. This is the canonical "growing
// network" test for the user's question about convergence guarantees.
//
// We deliberately spread items across many collections (rather than one
// big one) because all locations within a single collection share a long
// common suffix in the merged routing ID, which makes the XOR-closest
// candidate trivially the same for every key. With many collection IDs
// the keyspace is well distributed and each new node is statistically
// guaranteed to receive a meaningful share of the load.
func TestConvergence_GrowingNetwork_Sequential(t *testing.T) {
	ResetNetwork()

	const (
		collections    = 40
		itemsPerColl   = 8
		delegation     = 50
		nodeCount      = 5
		minNodesLoaded = 2 // after final convergence
	)

	n1 := newConvergenceNode(t, delegation)

	collNames := make([]string, collections)
	for i := range collNames {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		collNames[i] = name
	}

	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	totalItems := 0
	for _, c := range collNames {
		for j := 0; j < itemsPerColl; j++ {
			loc := string(chars[j%len(chars)])
			_ = n1.New(&domain.Item{
				Collection: c,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", j),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, "@", loc)
			totalItems++
		}
	}

	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	if got, _ := n1.Count(); got != totalItems {
		t.Fatalf("solo node lost items: got %d, want %d", got, totalItems)
	}

	nodes := []*core.Node{n1}
	for k := 2; k <= nodeCount; k++ {
		newNode := newConvergenceNode(t, delegation, nodes[0])
		nodes = append(nodes, newNode)

		rounds := converge(t, nodes, 5, 2, 60)

		counts := make([]int, len(nodes))
		for i, n := range nodes {
			counts[i], _ = n.Count()
		}
		t.Logf("after adding node %d/%d: converged in %d rounds, per-node counts=%v",
			k, nodeCount, rounds, counts)

		total := totalCount(t, nodes)
		if total != totalItems {
			t.Fatalf("after adding node %d, total drifted: got %d, want %d, per-node=%v",
				k, total, totalItems, counts)
		}

		assertOwnershipDisjointAndComplete(t, nodes)
	}

	loaded := 0
	for _, n := range nodes {
		if c, _ := n.Count(); c > 0 {
			loaded++
		}
	}
	if loaded < minNodesLoaded {
		t.Errorf("expected at least %d nodes to hold data after rebalance, got %d", minNodesLoaded, loaded)
	}
}

// TestConvergence_NoDoubleOwnership stresses the "exactly one owner"
// invariant by inserting many items spread across distinct collections,
// growing the cluster, and asserting that no (collection, location) ever
// shows up at more than one node.
func TestConvergence_NoDoubleOwnership(t *testing.T) {
	ResetNetwork()

	const (
		collections = 30
		perColl     = 10
		delegation  = 50
		nodeCount   = 4
	)

	n1 := newConvergenceNode(t, delegation)

	collNames := make([]string, collections)
	for i := range collNames {
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			t.Fatalf("RandomName: %v", err)
		}
		collNames[i] = name
	}

	// NOTE: items inserted with Location == base.Root() ("@") are
	// silently dropped by Collection.Add — its main loop never finds a
	// set to write into. Use a 1-char sublocation so they actually land.
	chars := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	for _, c := range collNames {
		for j := 0; j < perColl; j++ {
			loc := string(chars[j%len(chars)])
			_ = n1.New(&domain.Item{
				Collection: c,
				Location:   loc,
				Id:         fmt.Sprintf("id-%d", j),
				Metrics:    []float64{1, 2, 3, 4, 5},
			}, "@", loc)
		}
	}

	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	expected := collections * perColl
	if got, _ := n1.Count(); got != expected {
		t.Fatalf("solo node lost items: got %d, want %d", got, expected)
	}

	nodes := []*core.Node{n1}
	for k := 2; k <= nodeCount; k++ {
		nodes = append(nodes, newConvergenceNode(t, delegation, nodes[0]))
		converge(t, nodes, 5, 2, 60)
		assertOwnershipDisjointAndComplete(t, nodes)
		if total := totalCount(t, nodes); total != expected {
			t.Fatalf("after adding node %d: total=%d, want %d", k, total, expected)
		}
	}

	// Also sanity-check that some redistribution actually occurred. With
	// 30 distinct collections and 4 nodes, a uniform XOR distribution
	// gives a vanishingly small probability that one node owns all 30
	// collection roots. We at least require >1 distinct owner.
	owned := make(map[string]struct{})
	for _, n := range nodes {
		o, _ := n.Ownership()
		if len(o) > 0 {
			owned[n.Name()] = struct{}{}
		}
	}
	if len(owned) < 2 {
		t.Errorf("expected ownership spread across multiple nodes, all stayed on %v", owned)
	}
}
