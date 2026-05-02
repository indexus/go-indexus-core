package mockup

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
)

// TestRollingRestartThreeDiskNodes cycles each disk-backed node through
// Leave + process restart while the other two stay up as bootstrap anchors.
func TestRollingRestartThreeDiskNodes(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 50
		tickEvery   = 50 * time.Millisecond
		steadyFor   = 1500 * time.Millisecond
		timeout     = 90 * time.Second
		collections = 28
		perColl     = 5
	)

	tmp := t.TempDir()
	n1 := newDiskBackedNode(t, delegation, filepath.Join(tmp, "n1", "backup"), filepath.Join(tmp, "n1", "archive"))
	n2 := newDiskBackedNode(t, delegation, filepath.Join(tmp, "n2", "backup"), filepath.Join(tmp, "n2", "archive"), n1)
	n3 := newDiskBackedNode(t, delegation, filepath.Join(tmp, "n3", "backup"), filepath.Join(tmp, "n3", "archive"), n1)

	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1, n2, n3}, 5, 15*time.Second)

	tk := &ticker{}
	defer tk.stopAll()
	for _, n := range []*core.Node{n1, n2, n3} {
		tk.start(t, n, tickEvery)
	}
	nodes := []*core.Node{n1, n2, n3}
	awaitStability(t, nodes, expected, steadyFor, timeout)

	n1Name, n2Name, n3Name := n1.Name(), n2.Name(), n3.Name()
	n1Root := filepath.Join(tmp, "n1", "backup")
	n1Arch := filepath.Join(tmp, "n1", "archive")
	n2Root := filepath.Join(tmp, "n2", "backup")
	n2Arch := filepath.Join(tmp, "n2", "archive")
	n3Root := filepath.Join(tmp, "n3", "backup")
	n3Arch := filepath.Join(tmp, "n3", "archive")

	// Round 1: restart n3 (anchor n1, n2).
	tk.stopAll()
	network.Leave(n3Name)
	time.Sleep(200 * time.Millisecond)
	n3b := restartDiskBackedNode(t, n3Name, delegation, n3Root, n3Arch, n1)
	tk.start(t, n1, tickEvery)
	tk.start(t, n2, tickEvery)
	tk.start(t, n3b, tickEvery)
	nodes = []*core.Node{n1, n2, n3b}
	drainQueues(t, nodes, 5, 20*time.Second)
	awaitStability(t, nodes, expected, steadyFor, timeout)
	if totalCount(t, nodes) != expected {
		t.Fatalf("after n3 restart: total=%d want %d", totalCount(t, nodes), expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	// Round 2: restart n2.
	tk.stopAll()
	network.Leave(n2Name)
	time.Sleep(200 * time.Millisecond)
	n2b := restartDiskBackedNode(t, n2Name, delegation, n2Root, n2Arch, n1)
	tk.start(t, n1, tickEvery)
	tk.start(t, n2b, tickEvery)
	tk.start(t, n3b, tickEvery)
	nodes = []*core.Node{n1, n2b, n3b}
	drainQueues(t, nodes, 5, 20*time.Second)
	awaitStability(t, nodes, expected, steadyFor, timeout)
	if totalCount(t, nodes) != expected {
		t.Fatalf("after n2 restart: total=%d want %d", totalCount(t, nodes), expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	// Round 3: restart n1 (bootstrap a peer that already survived two rounds).
	tk.stopAll()
	network.Leave(n1Name)
	time.Sleep(200 * time.Millisecond)
	n1b := restartDiskBackedNode(t, n1Name, delegation, n1Root, n1Arch, n2b)
	tk.start(t, n1b, tickEvery)
	tk.start(t, n2b, tickEvery)
	tk.start(t, n3b, tickEvery)
	nodes = []*core.Node{n1b, n2b, n3b}
	drainQueues(t, nodes, 5, 20*time.Second)
	awaitStability(t, nodes, expected, steadyFor, timeout)
	if totalCount(t, nodes) != expected {
		t.Fatalf("after n1 restart: total=%d want %d", totalCount(t, nodes), expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)
}
