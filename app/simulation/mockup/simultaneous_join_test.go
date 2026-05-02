package mockup

import (
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// spawnConvergenceNode is like newConvergenceNode but safe to call from any
// goroutine (returns errors instead of using *testing.T).
func spawnConvergenceNode(delegation int, boot *core.Node) (*core.Node, error) {
	name, err := encoding.BASE64.RandomName()
	if err != nil {
		return nil, err
	}
	contacts := []domain.Contact{NewContact(boot.Name(), boot.IPs(), boot.Port())}
	settings, err := core.NewSettings(name, 0, time.Second, time.Minute, delegation)
	if err != nil {
		return nil, err
	}
	node, err := core.NewNode(settings, NewContact, contacts, NewStorage(), nil)
	if err != nil {
		return nil, err
	}
	network.Join(node)
	go func() { _ = node.Feed() }()
	return node, nil
}

// TestSimultaneousJoinFourNodesWhileSeeding starts four joins in parallel with
// seedAcrossCollections on the bootstrap node (same wall-clock overlap).
func TestSimultaneousJoinFourNodesWhileSeeding(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 50
		tickEvery   = 45 * time.Millisecond
		steadyFor   = 2 * time.Second
		timeout     = 90 * time.Second
		collections = 32
		perColl     = 8
	)

	n1 := newConvergenceNode(t, delegation)

	startJoin := make(chan struct{})
	var wg sync.WaitGroup
	extra := make([]*core.Node, 4)
	var mu sync.Mutex
	var spawnErr error
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-startJoin
			n, err := spawnConvergenceNode(delegation, n1)
			mu.Lock()
			if err != nil && spawnErr == nil {
				spawnErr = err
			}
			extra[idx] = n
			mu.Unlock()
		}(i)
	}
	close(startJoin)

	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	wg.Wait()
	mu.Lock()
	err := spawnErr
	mu.Unlock()
	if err != nil {
		t.Fatalf("parallel join: %v", err)
	}

	tk := &ticker{}
	defer tk.stopAll()
	nodes := []*core.Node{n1}
	for _, n := range extra {
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	for _, n := range nodes {
		tk.start(t, n, tickEvery)
	}

	drainQueues(t, nodes, 8, 30*time.Second)
	awaitStability(t, nodes, expected, steadyFor, timeout)

	if totalCount(t, nodes) != expected {
		t.Fatalf("simultaneous join total: got %d want %d", totalCount(t, nodes), expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
}
