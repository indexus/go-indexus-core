package mockup

import (
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
)

// TestDelegation_LowThresholdSpreadWorkload uses a low delegation threshold
// with many small collections (same pattern as convergence tests) so
// subtree splits fire frequently, then asserts completude, disjoint XOR
// ownership, empty Check(), and Owned() shard counts summing to Count().
func TestDelegation_LowThresholdSpreadWorkload(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 32
		tickEvery   = 45 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 60 * time.Second
		collections = 45
		perColl     = 30 // 1350 items — enough to stress delegation without a single mega-bucket stall
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 15*time.Second)

	n2 := newConvergenceNode(t, delegation, n1)
	n3 := newConvergenceNode(t, delegation, n1)

	tk := &ticker{}
	defer tk.stopAll()
	for _, n := range []*core.Node{n1, n2, n3} {
		tk.start(t, n, tickEvery)
	}

	nodes := []*core.Node{n1, n2, n3}
	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("delegation completude total: got %d want %d", total, expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	for _, n := range nodes {
		for _, line := range n.Check() {
			t.Errorf("node %s Check: %s", n.Name(), line)
		}
		owned, err := n.Owned()
		if err != nil {
			t.Fatalf("Owned %s: %v", n.Name(), err)
		}
		sum := 0
		for _, e := range owned {
			sum += e.Count
		}
		c, _ := n.Count()
		if sum != c {
			t.Errorf("node %s Owned sum %d != Count %d", n.Name(), sum, c)
		}
	}
}
