package core

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/peer"
)

type memStorage struct {
	mu       sync.Mutex
	lines    []string
	snapshot []string
	dirty    bool
}

func (m *memStorage) Exist() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.snapshot) > 0 || len(m.lines) > 0
}

func (m *memStorage) Reset() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lines, m.snapshot = nil, nil
	m.dirty = false
	return nil
}

func (m *memStorage) Save(commands []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshot = append([]string(nil), commands...)
	m.lines = nil
	m.dirty = false
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
	m.dirty = true
}

func (m *memStorage) SyncAppend(line string) error {
	m.Append(line)
	return nil
}

func (m *memStorage) Dirty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dirty || len(m.lines) > 0
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

type dialPeer struct {
	name string
	id   []byte
	ip   string
	ips  map[string]any
	port int
}

func newDialPeer(name, ip string, port int) *dialPeer {
	id, err := encoding.BASE64.Decode(name)
	if err != nil {
		id = make([]byte, 12)
		copy(id, []byte(name))
	}
	ips := map[string]any{}
	if ip != "" {
		ips[ip] = nil
	}
	return &dialPeer{name: name, id: id, ip: ip, ips: ips, port: port}
}

func (p *dialPeer) Name() string                                { return p.name }
func (p *dialPeer) ID() []byte                                  { return p.id }
func (p *dialPeer) Hash() string                                { return p.name }
func (p *dialPeer) IPs() map[string]any                         { return p.ips }
func (p *dialPeer) Port() int                                   { return p.port }
func (p *dialPeer) IP() string                                  { return p.ip }
func (p *dialPeer) Host() string                                { return fmt.Sprintf("%s@%s|%d", p.name, p.ip, p.port) }
func (p *dialPeer) Ping(domain.Contact) (domain.Contact, error) { return p, nil }
func (p *dialPeer) Neighbors(domain.Peer) ([]domain.Contact, error) {
	return nil, nil
}
func (p *dialPeer) Random(domain.Peer) (domain.Contact, error) { return nil, nil }
func (p *dialPeer) Get(string, string, bool, int) (domain.Contact, *domain.Set, error) {
	return p, nil, nil
}
func (p *dialPeer) New(*domain.Item, string, string) error                 { return nil }
func (p *dialPeer) Delete(*domain.Item, string, string) error              { return nil }
func (p *dialPeer) Transfer(domain.Peer, domain.Key, []*domain.Item) error { return nil }

type transferSink struct {
	*dialPeer
	mu       sync.Mutex
	batches  [][]*domain.Item
	failOnce atomic.Bool
	calls    atomic.Int64
}

func newTransferSink(name, ip string, port int) *transferSink {
	return &transferSink{dialPeer: newDialPeer(name, ip, port)}
}

func (s *transferSink) Transfer(_ domain.Peer, _ domain.Key, items []*domain.Item) error {
	s.calls.Add(1)
	if s.failOnce.Swap(false) {
		return fmt.Errorf("simulated transfer failure")
	}
	copied := append([]*domain.Item(nil), items...)
	s.mu.Lock()
	s.batches = append(s.batches, copied)
	s.mu.Unlock()
	return nil
}

func (s *transferSink) receivedItems() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.batches {
		n += len(b)
	}
	return n
}

func item(id string) *domain.Item {
	return &domain.Item{Collection: "demo", Location: "aa", Id: id, Metrics: []float64{1}}
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

func ownedNode(t *testing.T, storage domain.Storage) *Node {
	t.Helper()
	node := newNodeOn(t, storage, 64)
	if err := node.New(item("x"), encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)
	return node
}

func leaveEnv(t *testing.T) {
	t.Helper()
	t.Setenv("INDEXUS_LEAVE_DIR", t.TempDir())
	t.Setenv("INDEXUS_STORAGE", t.TempDir()+"/backup")
	t.Setenv("SNAPSHOT_BUCKET", "")
}

func nodeWithBootstrap(t *testing.T, boot domain.Contact, storage domain.Storage) *Node {
	t.Helper()
	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	settings, err := NewSettings(name, 21000, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatalf("NewSettings: %v", err)
	}
	settings.SetAdvertise("127.0.0.1")
	node, err := NewNode(settings, func(n string, ips map[string]any, port int) domain.Contact {
		ip := "127.0.0.1"
		for k := range ips {
			ip = k
			break
		}
		return newDialPeer(n, ip, port)
	}, []domain.Contact{boot}, storage)
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	return node
}

func seedOwned(t *testing.T, n *Node, id string) {
	t.Helper()
	if err := n.New(item(id), encoding.BASE64.Root(), "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)
}

func ownedCount(t *testing.T, n *Node) int {
	t.Helper()
	return len(n.listOwnedKeys())
}
