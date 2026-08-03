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

func ResetNetwork() {
	network.Reset()
}

type Network struct {
	mu          sync.RWMutex
	nodes       map[string]*core.Node
	unreachable map[string]*core.Node

	transferDropPct atomic.Int32
	transferOK      atomic.Uint64
	transferDropped atomic.Uint64
}

func (n *Network) SetTransferDropRate(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	n.transferDropPct.Store(int32(pct))
}

func (n *Network) TransferStats() (uint64, uint64) {
	return n.transferOK.Load(), n.transferDropped.Load()
}

func NewNetwork() *Network {
	return &Network{
		nodes:       map[string]*core.Node{},
		unreachable: map[string]*core.Node{},
	}
}

// Reset empties the network in place; reassigning the package global would
// race with the peers still reading it.
func (n *Network) Reset() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes = map[string]*core.Node{}
	n.unreachable = map[string]*core.Node{}
	n.transferDropPct.Store(0)
	n.transferOK.Store(0)
	n.transferDropped.Store(0)
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
}

func NewContact(name string, ips map[string]any, port int) domain.Contact {
	return &Peer{
		name: name,
		ips:  ips,
		port: port,
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

	distant, ok := network.Get(p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	node, err := distant.Ping(NewContact(origin.Name(), origin.IPs(), origin.Port()))
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}

	return NewContact(node.Name(), node.IPs(), node.Port()), nil
}

func (p *Peer) Neighbors(origin domain.Peer) ([]domain.Contact, error) {

	distant, ok := network.Get(p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	neighbors, err := distant.Neighbors(origin)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}

	contacts := make([]domain.Contact, 0)
	for _, neighbor := range neighbors {
		contacts = append(contacts, NewContact(neighbor.Name(), neighbor.IPs(), neighbor.Port()))
	}

	return contacts, nil
}

func (p *Peer) Random(origin domain.Peer) (domain.Contact, error) {

	distant, ok := network.Get(p.Name())
	if !ok {
		return nil, fmt.Errorf("error code: 404")
	}

	random, err := distant.Random(origin)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}

	if random == nil {
		return nil, fmt.Errorf("error no contact to propose")
	}

	return NewContact(random.Name(), random.IPs(), random.Port()), nil
}

func (p *Peer) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {

	if pct := network.transferDropPct.Load(); pct > 0 {
		if rand.Intn(100) < int(pct) {
			network.transferDropped.Add(1)
			return fmt.Errorf("simulated transfer drop")
		}
	}

	distant, ok := network.Get(p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.Transfer(origin, key, items)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}
	network.transferOK.Add(1)

	return nil
}

func (p *Peer) Get(collection string, location string, depth int) (domain.Contact, *domain.Set, error) {

	distant, ok := network.Get(p.Name())
	if !ok {
		return nil, nil, fmt.Errorf("error code: 404")
	}

	contact, set, err := distant.Get(collection, location, depth)
	if err != nil {
		return nil, nil, fmt.Errorf("error making request: %s", err.Error())
	}

	return contact, set, nil
}

func (p *Peer) New(item *domain.Item, root string, current string) error {

	distant, ok := network.Get(p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.New(item, root, current)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}

	return err
}

func (p *Peer) Delete(item *domain.Item, root string, current string) error {
	distant, ok := network.Get(p.Name())
	if !ok {
		return fmt.Errorf("error code: 404")
	}
	err := distant.Delete(item, root, current)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}
	return err
}
