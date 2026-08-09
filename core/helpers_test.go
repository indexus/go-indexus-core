package core

import (
	"bytes"
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

// Ensure memStorage satisfies domain.Storage even when the interface grows.
var _ domain.Storage = (*memStorage)(nil)

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
func (p *dialPeer) Get(string, string, bool, domain.Visited, bool) (domain.Contact, *domain.Set, error) {
	return p, nil, nil
}
func (p *dialPeer) Children(string, string) (map[string]*domain.ChildEntry, error) {
	return map[string]*domain.ChildEntry{}, nil
}
func (p *dialPeer) New(*domain.Item, string, domain.Visited) error    { return nil }
func (p *dialPeer) Delete(*domain.Item, string, domain.Visited) error { return nil }
func (p *dialPeer) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	return p.Name(), nil
}

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

func (s *transferSink) Transfer(_ domain.Peer, _ domain.Key, items []*domain.Item) (string, error) {
	s.calls.Add(1)
	if s.failOnce.Swap(false) {
		return "", fmt.Errorf("simulated transfer failure")
	}
	copied := append([]*domain.Item(nil), items...)
	s.mu.Lock()
	s.batches = append(s.batches, copied)
	s.mu.Unlock()
	return s.Name(), nil
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
	if err := node.New(item("x"), encoding.BASE64.Root(), nil); err != nil {
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
	if err := n.New(item(id), encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)
}

func ownedCount(t *testing.T, n *Node) int {
	t.Helper()
	return len(n.listOwnedKeys())
}

type readPeer struct {
	*dialPeer
	set      *domain.Set
	err      error
	gets     atomic.Int64
	via      atomic.Value
	refreshs atomic.Int64
}

func (p *readPeer) Get(_, _ string, _ bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {
	p.gets.Add(1)
	p.via.Store(via)
	if refresh {
		p.refreshs.Add(1)
	}
	if p.err != nil {
		return p, nil, p.err
	}
	return p, p.set, nil
}

func setOf(key string, count int) *domain.Set {
	set := domain.NewSet()
	set.Put(key, domain.NewAbelian(count, nil))
	return set
}

func atZone(t *testing.T, collection, location string, set *domain.Set) *readPeer {
	t.Helper()
	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	peer := &readPeer{
		dialPeer: newDialPeer(name, "10.0.0.1", 21000),
		set:      set,
	}
	peer.id = zoneID(t, collection, location)
	return peer
}

func zonePeer(t *testing.T, n *Node, collection, location string, set *domain.Set) *readPeer {
	t.Helper()
	peer := atZone(t, collection, location, set)
	n.register([]domain.Contact{peer})
	return peer
}

type ownerPeer struct {
	*readPeer
	aggregates atomic.Int64
}

func (p *ownerPeer) GetAggregates(_ string, locations []string) (map[string]*domain.Abelian, error) {
	p.aggregates.Add(1)
	out := make(map[string]*domain.Abelian)
	if p.set == nil {
		return out, nil
	}
	for _, location := range locations {
		out[location] = p.set.Abelian()
	}
	return out, nil
}

func (p *readPeer) lastVia() domain.Visited {
	v, _ := p.via.Load().(domain.Visited)
	return v
}

// nodeProxy exposes a Node over a dialable Contact, so two real Nodes can run
// the protocol against each other in-process.
type nodeProxy struct {
	*dialPeer
	node *Node
	gets atomic.Int64
}

func (p *nodeProxy) Get(collection, location string, deep bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {
	p.gets.Add(1)
	return p.node.Get(collection, location, deep, via, refresh)
}

func (p *nodeProxy) Children(collection, parent string) (map[string]*domain.ChildEntry, error) {
	return p.node.Children(collection, parent)
}

func (p *nodeProxy) GetAggregates(collection string, locations []string) (map[string]*domain.Abelian, error) {
	return p.node.GetAggregates(collection, locations)
}

func (p *nodeProxy) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	return p.node.Transfer(origin, key, items)
}

func (p *nodeProxy) Neighbors(origin domain.Peer) ([]domain.Contact, error) {
	return p.node.Neighbors(origin)
}

func (p *nodeProxy) New(item *domain.Item, root string, via domain.Visited) error {
	return p.node.New(item, root, via)
}

func (p *nodeProxy) Delete(item *domain.Item, root string, via domain.Visited) error {
	return p.node.Delete(item, root, via)
}

func (p *nodeProxy) Claim(payload domain.ClaimPayload) error {
	origin := domain.Peer(newDialPeer(payload.Peer, "", 0))
	if payload.Peer == "" {
		origin = p
	}
	return p.node.Claim(origin, payload)
}

// link registers each node's proxy on the other.
func link(t *testing.T, a, b *Node) (toB, toA *nodeProxy) {
	t.Helper()
	toB = &nodeProxy{dialPeer: newDialPeer(b.Name(), "127.0.0.1", 21098), node: b}
	toA = &nodeProxy{dialPeer: newDialPeer(a.Name(), "127.0.0.1", 21099), node: a}
	a.register([]domain.Contact{toB})
	b.register([]domain.Contact{toA})
	return toB, toA
}

// seedOwnedZone owns loc and inserts leaves at child locations under loc so they
// materialise inside sets[loc] (Add stores entry on Parent(item.Location)).
func seedOwnedZone(t *testing.T, n *Node, coll, loc string, leafLocs map[string]string) *domain.Collection {
	t.Helper()
	n.create(coll, encoding.BASE64.Root())
	c, ok := n.collections.Get(coll)
	if !ok {
		t.Fatal("missing collection")
	}
	c.EnsureSet(loc)
	n.own(c, domain.Ownership{loc: domain.Delegation{}})
	if !c.Owns(loc) {
		t.Fatalf("failed to own %s", loc)
	}
	for id, itemLoc := range leafLocs {
		it := &domain.Item{Collection: coll, Location: itemLoc, Id: id, Metrics: []float64{1}}
		if !n.add(it, false) {
			t.Fatalf("seed %s: add refused", id)
		}
	}
	if got := c.ItemCount(loc); got != len(leafLocs) {
		t.Fatalf("seed ItemCount=%d want %d", got, len(leafLocs))
	}
	return c
}

// nearerToZone sorts two nodes by XOR distance to the zone key: the nearest is
// the one placement must converge on. Node names are random, so a test derives
// the expected outcome rather than assuming it.
func nearerToZone(t *testing.T, coll, loc string, a, b *Node) (near, far *Node) {
	t.Helper()
	id := zoneID(t, coll, loc)
	if bytes.Compare(xorDistance(a.ID(), id), xorDistance(b.ID(), id)) <= 0 {
		return a, b
	}
	return b, a
}

func zoneID(t *testing.T, collection, location string) []byte {
	t.Helper()
	id, err := zoneKeyID(collection, location)
	if err != nil {
		t.Fatalf("zoneKeyID: %v", err)
	}
	return id
}
