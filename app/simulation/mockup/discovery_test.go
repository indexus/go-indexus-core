package mockup

import (
	"fmt"
	"testing"

	"github.com/indexus/go-indexus-core/core"
)

// Repair paths conclude from silence: an unanswered pull means "nobody I know
// owns this zone", and a node acts on that. The conclusion is only sound if
// what it knows is the whole mesh, so full knowledge is not a nicety here, it
// is the premise those paths rest on.
//
// The chain is the worst case for reaching it. Every node is handed exactly one
// lead — its successor — and nothing else, so the fifteenth node is fifteen
// introductions away from the first. What has to happen is that each round
// roughly doubles what a node knows, which is what a log2 fanout buys.
func TestAChainOfNodesReachesFullKnowledgeOfTheMesh(t *testing.T) {
	ResetNetwork()

	const size = 16
	nodes := make([]*core.Node, size)
	for i := range nodes {
		nodes[i] = newConvergenceNode(t, 1024)
	}

	// Ping records its caller, so pinging the successor's contact from node i
	// is what plants the single edge i -> i+1.
	for i, n := range nodes {
		next := nodes[(i+1)%size]
		if _, err := n.Ping(NewContact(next.Name(), next.IPs(), next.Port())); err != nil {
			t.Fatalf("seeding %s -> %s: %v", n.Name(), next.Name(), err)
		}
	}

	// Observed range over repeated runs is 3–5 rounds; the budget is a stall
	// detector, not a performance bound.
	const maxRounds = 60
	gap := ""
	for round := 1; round <= maxRounds; round++ {
		for _, n := range nodes {
			if err := n.Observe(); err != nil {
				t.Fatalf("Observe %s: %v", n.Name(), err)
			}
		}
		if gap = knowledgeGap(t, nodes); gap == "" {
			t.Logf("every node knew all %d others after %d rounds", size-1, round)
			return
		}
	}

	t.Fatalf("the mesh never reached full knowledge in %d rounds: %s", maxRounds, gap)
}

// knowledgeGap returns the first node that is missing a peer, or "" when every
// node knows every other.
func knowledgeGap(t *testing.T, nodes []*core.Node) string {
	t.Helper()

	for _, n := range nodes {
		registered, err := n.Registered()
		if err != nil {
			t.Fatalf("Registered %s: %v", n.Name(), err)
		}
		// Registered reports the pool as stored, self included; only the other
		// nodes count towards knowing the mesh.
		known := make(map[string]bool, len(registered))
		for _, contact := range registered {
			if contact.Name() != n.Name() {
				known[contact.Name()] = true
			}
		}
		for _, other := range nodes {
			if other.Name() == n.Name() || known[other.Name()] {
				continue
			}
			return fmt.Sprintf("%s knows %d of %d peers, missing %s",
				n.Name(), len(known), len(nodes)-1, other.Name())
		}
	}
	return ""
}
