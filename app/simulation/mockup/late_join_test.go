package mockup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/storage"
)

// TestLateJoin_FourthNodeReceivesShare seeds three nodes, waits for a stable
// partition, then joins a fourth and checks that the newcomer receives a
// non-empty share while global completude and XOR ownership hold.
func TestLateJoin_FourthNodeReceivesShare(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 50
		tickEvery   = 50 * time.Millisecond
		steadyFor   = 1500 * time.Millisecond
		timeout     = 45 * time.Second
		collections = 40
		perColl     = 5
	)

	n1 := newConvergenceNode(t, delegation)
	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1}, 5, 10*time.Second)

	n2 := newConvergenceNode(t, delegation, n1)
	n3 := newConvergenceNode(t, delegation, n1)

	tk := &ticker{}
	defer tk.stopAll()
	for _, n := range []*core.Node{n1, n2, n3} {
		tk.start(t, n, tickEvery)
	}

	awaitStability(t, []*core.Node{n1, n2, n3}, expected, steadyFor, timeout)

	n4 := newConvergenceNode(t, delegation, n1)
	tk.start(t, n4, tickEvery)

	nodes := []*core.Node{n1, n2, n3, n4}
	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("late join total: got %d want %d", total, expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	c4, _ := n4.Count()
	if c4 == 0 {
		t.Fatalf("expected fourth node to own some items after join, Count=0")
	}
}

// newDiskBackedNode creates a node with real on-disk storage (for restart).
func newDiskBackedNode(t *testing.T, delegation int, rootDir, archiveDir string, bootstraps ...*core.Node) *core.Node {
	t.Helper()

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	contacts := make([]domain.Contact, 0, len(bootstraps))
	for _, b := range bootstraps {
		contacts = append(contacts, NewContact(b.Name(), b.IPs(), b.Port()))
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatalf("MkdirAll root: %v", err)
	}
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll archive: %v", err)
	}
	st := storage.NewStorage(archiveDir, rootDir)
	settings, err := core.NewSettings(name, 0, time.Second, time.Minute, delegation)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := core.NewNode(settings, NewContact, contacts, st, nil)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	network.Join(node)
	go func() { _ = node.Feed() }()
	return node
}

// restartDiskBackedNode replaces an existing disk-backed node with a new
// process instance using the same name and storage directories (simulates OS
// process restart).
func restartDiskBackedNode(t *testing.T, savedName string, delegation int, rootDir, archiveDir string, boot *core.Node) *core.Node {
	t.Helper()

	contacts := []domain.Contact{NewContact(boot.Name(), boot.IPs(), boot.Port())}
	settings, err := core.NewSettings(savedName, 0, time.Second, time.Minute, delegation)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	st := storage.NewStorage(archiveDir, rootDir)
	node, err := core.NewNode(settings, NewContact, contacts, st, nil)
	if err != nil {
		t.Fatalf("NewNode restart: %v", err)
	}
	network.Join(node)
	go func() { _ = node.Feed() }()
	return node
}

// TestRestart_DiskBackedNodeRejoinsStable stops a disk-backed middle node and
// brings it back with the same identity and storage paths; global item count
// and ownership invariants must hold afterward.
func TestRestart_DiskBackedNodeRejoinsStable(t *testing.T) {
	ResetNetwork()

	const (
		delegation  = 50
		tickEvery   = 50 * time.Millisecond
		steadyFor   = 1500 * time.Millisecond
		timeout     = 60 * time.Second
		collections = 35
		perColl     = 5
	)

	tmp := t.TempDir()
	n2Root := filepath.Join(tmp, "n2", "backup")
	n2Archive := filepath.Join(tmp, "n2", "archive")

	n1 := newConvergenceNode(t, delegation)
	n2 := newDiskBackedNode(t, delegation, n2Root, n2Archive, n1)
	n3 := newConvergenceNode(t, delegation, n1)

	expected, _ := seedAcrossCollections(t, n1, collections, perColl)
	drainQueues(t, []*core.Node{n1, n2, n3}, 5, 10*time.Second)

	tk := &ticker{}
	tk.start(t, n1, tickEvery)
	tk.start(t, n2, tickEvery)
	tk.start(t, n3, tickEvery)

	nodes := []*core.Node{n1, n2, n3}
	awaitStability(t, nodes, expected, steadyFor, timeout)

	savedName := n2.Name()

	tk.stopAll()
	network.Leave(savedName)

	n2b := restartDiskBackedNode(t, savedName, delegation, n2Root, n2Archive, n1)
	nodes = []*core.Node{n1, n2b, n3}

	tk.start(t, n1, tickEvery)
	tk.start(t, n2b, tickEvery)
	tk.start(t, n3, tickEvery)
	defer tk.stopAll()

	awaitStability(t, nodes, expected, steadyFor, timeout)

	if total := totalCount(t, nodes); total != expected {
		t.Fatalf("after restart total: got %d want %d", total, expected)
	}
	assertOwnershipDisjointAndComplete(t, nodes)

	if c, _ := n2b.Count(); c == 0 {
		t.Fatalf("restarted node should own some items, Count=0")
	}
}
