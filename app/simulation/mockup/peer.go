package mockup

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

var network = NewNetwork()

// ResetNetwork drops all known nodes from the in-process simulation.
// Intended for tests that want a clean global before driving a new
// topology.
func ResetNetwork() {
	network = NewNetwork()
}

type Network struct {
	mu          sync.RWMutex
	nodes       map[string]*core.Node
	unreachable map[string]*core.Node
	blocked     map[string]map[string]bool

	// transferDropPct is a 0..100 percentage. When non-zero, every
	// Transfer call has that probability of returning a synthetic
	// transport error WITHOUT dispatching to the receiver. Used by the
	// fault-injection tests to exercise the lossy-handoff path.
	transferDropPct atomic.Int32

	// transferCounters track how many Transfer calls succeeded vs
	// were dropped by fault injection. Useful to assert in tests.
	transferOK      atomic.Uint64
	transferDropped atomic.Uint64
}

// SetTransferDropRate sets the probability (0..100) that any given
// Transfer call will fail with a synthetic transport error. Setting it
// to 0 disables fault injection.
func (n *Network) SetTransferDropRate(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	n.transferDropPct.Store(int32(pct))
}

// TransferStats returns (ok, dropped) Transfer counters since the
// network was created.
func (n *Network) TransferStats() (uint64, uint64) {
	return n.transferOK.Load(), n.transferDropped.Load()
}

func NewNetwork() *Network {
	return &Network{
		nodes:       map[string]*core.Node{},
		unreachable: map[string]*core.Node{},
		blocked:     map[string]map[string]bool{},
	}
}

func (n *Network) Join(node *core.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes[node.Name()] = node
}

func (n *Network) Unreachable(node *core.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.unreachable[node.Name()] = node
}

func (n *Network) Length() int {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return len(n.nodes)
}

func (n *Network) Get(name string) (*core.Node, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	node, ok := n.nodes[name]
	return node, ok
}

func (n *Network) GetFrom(origin, name string) (*core.Node, bool) {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if origin != "" && n.blocked[origin][name] {
		return nil, false
	}
	node, ok := n.nodes[name]
	return node, ok
}

func (n *Network) Partition(left, right []*core.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, a := range left {
		if n.blocked[a.Name()] == nil {
			n.blocked[a.Name()] = map[string]bool{}
		}
		for _, b := range right {
			n.blocked[a.Name()][b.Name()] = true
			if n.blocked[b.Name()] == nil {
				n.blocked[b.Name()] = map[string]bool{}
			}
			n.blocked[b.Name()][a.Name()] = true
		}
	}
}

func (n *Network) Heal() {
	n.mu.Lock()
	n.blocked = map[string]map[string]bool{}
	n.mu.Unlock()
}

func (n *Network) Nodes() []*core.Node {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]*core.Node, 0, len(n.nodes))
	for _, node := range n.nodes {
		out = append(out, node)
	}
	return out
}

func (n *Network) Random() *core.Node {
	n.mu.RLock()
	defer n.mu.RUnlock()
	if len(n.nodes) > 0 {
		stop := rand.Intn(len(n.nodes))
		for _, node := range n.nodes {
			if stop == 0 {
				return node
			}
			stop--
		}
	}
	return nil
}

type Peer struct {
	name string
	ips  map[string]any
	port int
	ip   string
	from string
	mesh *Network
}

func NewContact(name string, ips map[string]any, port int) domain.Contact {
	return newPeerContact(network, "", name, ips, port)
}

func newPeerContact(mesh *Network, from, name string, ips map[string]any, port int) *Peer {
	return &Peer{
		name: name,
		ips:  ips,
		port: port,
		from: from,
		mesh: mesh,
	}
}

func ContactsOf(origin string) func(string, map[string]any, int) domain.Contact {
	mesh := network
	return func(name string, ips map[string]any, port int) domain.Contact {
		return newPeerContact(mesh, origin, name, ips, port)
	}
}

func (p *Peer) ID() []byte {
	id, err := encoding.BASE64.Decode(p.name)
	if err != nil {
		panic(err)
	}
	return id
}

func (p *Peer) Name() string {
	return p.name
}

func (p *Peer) IPs() map[string]any {
	return p.ips
}

func (p *Peer) Port() int {
	return p.port
}

func (p *Peer) IP() string {
	return p.ip
}

func (p *Peer) Host() string {
	return fmt.Sprintf("%s@%s|%d", p.name, p.ip, p.port)
}

func (p *Peer) Ping(origin domain.Contact) (domain.Contact, error) {

	distant, ok := p.mesh.GetFrom(origin.Name(), p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	node, err := distant.Ping(NewContact(origin.Name(), origin.IPs(), origin.Port()))
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}

	return newPeerContact(p.mesh, origin.Name(), node.Name(), node.IPs(), node.Port()), nil
}

func (p *Peer) Neighbors(origin domain.Peer) ([]domain.Contact, error) {

	distant, ok := p.mesh.GetFrom(origin.Name(), p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	neighbors, err := distant.Neighbors(origin)
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}

	contacts := make([]domain.Contact, 0)
	for _, neighbor := range neighbors {
		contacts = append(contacts, newPeerContact(p.mesh, origin.Name(), neighbor.Name(), neighbor.IPs(), neighbor.Port()))
	}

	return contacts, nil
}

func (p *Peer) Random(origin domain.Peer) (domain.Contact, error) {

	distant, ok := p.mesh.GetFrom(origin.Name(), p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	random, err := distant.Random(origin)
	if err != nil {
		return nil, fmt.Errorf("error making request: %w", err)
	}

	if random == nil {
		return nil, fmt.Errorf("error no contact to propose")
	}

	return newPeerContact(p.mesh, origin.Name(), random.Name(), random.IPs(), random.Port()), nil
}

func (p *Peer) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {

	if pct := p.mesh.transferDropPct.Load(); pct > 0 {
		if rand.Intn(100) < int(pct) {
			p.mesh.transferDropped.Add(1)
			return "", fmt.Errorf("simulated transfer drop")
		}
	}

	distant, ok := p.mesh.GetFrom(origin.Name(), p.Name())
	if !ok {
		return "", fmt.Errorf("error code: 404")
	}
	p.mesh.transferOK.Add(1)

	ackedPeer, err := distant.Transfer(origin, key, items)
	if err != nil {
		return "", fmt.Errorf("error making request: %w", err)
	}

	return ackedPeer, nil
}

func (p *Peer) Claim(payload domain.ClaimPayload) error {
	distant, ok := p.mesh.GetFrom(payload.Peer, p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}
	origin := NewContact(payload.Peer, nil, 0)
	return distant.Claim(origin, payload)
}

func (p *Peer) Get(collection string, location string, deep bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {

	distant, ok := p.mesh.GetFrom(p.from, p.Name())
	if !ok {
		return nil, nil, fmt.Errorf("error code: 404")
	}

	contact, set, err := distant.Get(collection, location, deep, via, refresh)
	if err != nil {
		return nil, nil, fmt.Errorf("error making request: %w", err)
	}

	if contact != nil {
		contact = newPeerContact(p.mesh, p.from, contact.Name(), contact.IPs(), contact.Port())
	}
	return contact, set, nil
}

func (p *Peer) Children(collection, parent string) (map[string]*domain.ChildEntry, error) {
	distant, ok := p.mesh.GetFrom(p.from, p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}
	return distant.Children(collection, parent)
}

func (p *Peer) GetAggregates(collection string, locations []string) (map[string]*domain.Abelian, error) {
	distant, ok := p.mesh.GetFrom(p.from, p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}
	return distant.GetAggregates(collection, locations)
}

func (p *Peer) New(item *domain.Item, root string, via domain.Visited) error {

	distant, ok := p.mesh.GetFrom(p.from, p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.Handoff(item, root, via)
	if err != nil {
		return fmt.Errorf("error making request: %w", err)
	}

	return err
}

func (p *Peer) Delete(item *domain.Item, root string, via domain.Visited) error {

	distant, ok := p.mesh.GetFrom(p.from, p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.Delete(item, root, via)
	if err != nil {
		return fmt.Errorf("error making request: %w", err)
	}

	return err
}
