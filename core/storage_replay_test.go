package core

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

// memStorage replays what it recorded, so a second node can restore from the
// same log a first node wrote. Exist() starts true to mimic a node that has
// already saved at least one snapshot, which is what gates the log replay.
type memStorage struct {
	mu       sync.Mutex
	lines    []string
	snapshot []string
}

func (m *memStorage) Exist() bool { return true }

func (m *memStorage) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lines, m.snapshot = nil, nil
	return nil
}

func (m *memStorage) Save(commands []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.snapshot = append([]string(nil), commands...)
	return nil
}

func (m *memStorage) Load() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.snapshot...), nil
}

func (m *memStorage) Append(line string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.lines = append(m.lines, line)
}

func (m *memStorage) SyncAppend(line string) error {
	m.Append(line)
	return nil
}

func (m *memStorage) Stream(start int) <-chan string {
	m.mu.Lock()
	lines := append([]string(nil), m.lines...)
	m.mu.Unlock()

	stream := make(chan string)
	go func() {
		defer close(stream)
		for idx, line := range lines {
			if idx >= start {
				stream <- line
			}
		}
	}()
	return stream
}

func (m *memStorage) Logs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]string(nil), m.lines...)
}

func newNodeOn(t *testing.T, storage domain.Storage, delegation int) *Node {
	t.Helper()

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	settings, err := NewSettings(name, 0, time.Second, time.Minute, delegation)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	node, err := NewNode(settings, peer.NewContact, nil, storage)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

// drain applies the queue synchronously, standing in for the Feed goroutine.
func drain(t *testing.T, n *Node) {
	t.Helper()

	for steps := 0; n.queue.Length() > 0; steps++ {
		if steps > 1000 {
			t.Fatalf("queue does not drain, length=%d", n.queue.Length())
		}
		element, ok := n.queue.Consume()
		if !ok {
			return
		}
		if err := n.process(element); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
}

func countLines(logs []string, prefix string) int {
	count := 0
	for _, line := range logs {
		if strings.HasPrefix(line, prefix) {
			count++
		}
	}
	return count
}

// A delete must survive a restart: replaying the log has to apply the delete
// after the add that precedes it, not resurrect the item.
func TestRestoreReplaysDeleteAfterAdd(t *testing.T) {
	storage := &memStorage{}
	root := encoding.BASE64.Root()

	node := newNodeOn(t, storage, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}

	if err := node.New(item, root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 1 {
		t.Fatalf("count after add: got %d (err=%v) want 1", count, err)
	}

	if err := node.Delete(item, root, "aa"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 0 {
		t.Fatalf("count after delete: got %d (err=%v) want 0", count, err)
	}

	logs := storage.Logs()
	if got := countLines(logs, "ingress|"); got != 1 {
		t.Fatalf("ingress lines: got %d want 1 (%v)", got, logs)
	}
	if got := countLines(logs, "delete|"); got != 1 {
		t.Fatalf("delete lines: got %d want 1 (%v)", got, logs)
	}
	if got := countLines(logs, "tombstone|"); got != 1 {
		t.Fatalf("tombstone lines: got %d want 1 (%v)", got, logs)
	}

	// A fresh node restoring from the same log must not resurrect the item.
	restored := newNodeOn(t, storage, 64)
	drain(t, restored)

	if count, err := restored.Count(); err != nil || count != 0 {
		t.Fatalf("count after restore: got %d (err=%v) want 0", count, err)
	}

	collection, ok := restored.collections.Get("demo")
	if !ok {
		t.Fatal("restore did not rebuild the collection")
	}
	if !collection.IsTombstoned("aa", "x") {
		t.Fatal("restore lost the tombstone, a stale replay could resurrect the item")
	}
}

// Re-adding after a delete is allowed and must land exactly once.
func TestRestoreReplaysReaddAfterDelete(t *testing.T) {
	storage := &memStorage{}
	root := encoding.BASE64.Root()

	node := newNodeOn(t, storage, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}

	if err := node.New(item, root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)
	if err := node.Delete(item, root, "aa"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	drain(t, node)
	if err := node.New(item, root, "aa"); err != nil {
		t.Fatalf("re-New: %v", err)
	}
	drain(t, node)

	if count, err := node.Count(); err != nil || count != 1 {
		t.Fatalf("count after re-add: got %d (err=%v) want 1", count, err)
	}

	restored := newNodeOn(t, storage, 64)
	drain(t, restored)

	if count, err := restored.Count(); err != nil || count != 1 {
		t.Fatalf("count after restore: got %d (err=%v) want 1", count, err)
	}

	collection, ok := restored.collections.Get("demo")
	if !ok {
		t.Fatal("restore did not rebuild the collection")
	}
	if collection.IsTombstoned("aa", "x") {
		t.Fatal("tombstone survived the re-add")
	}
}
