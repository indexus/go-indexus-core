package mockup

import (
	"fmt"
	"math/rand"

	"gitlab.com/indexus/node/core"
	"gitlab.com/indexus/node/domain"
)

var network = NewNetwork()

type Network struct {
	nodes       map[string]*core.Node
	unreachable map[string]*core.Node
}

func NewNetwork() *Network {
	return &Network{
		nodes:       map[string]*core.Node{},
		unreachable: map[string]*core.Node{},
	}
}

func (n *Network) Join(node *core.Node) {
	n.nodes[node.Name()] = node
}

func (n *Network) Unreachable(node *core.Node) {
	n.unreachable[node.Name()] = node
}

func (n *Network) Length() int {
	return len(n.nodes)
}

func (n *Network) Random() *core.Node {
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
	id, err := domain.DecodeName(p.name)
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

	distant, ok := network.nodes[p.Name()]
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

	distant, ok := network.nodes[p.Name()]
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

	distant, ok := network.nodes[p.Name()]
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

	distant, ok := network.nodes[p.Name()]
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.Transfer(origin, key, items)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}

	return nil
}

func (p *Peer) Get(collection string, location string) (domain.Contact, *domain.Set, error) {

	distant, ok := network.nodes[p.Name()]
	if !ok {
		return nil, nil, fmt.Errorf("error code: 404")
	}

	contact, set, err := distant.Get(collection, location)
	if err != nil {
		return nil, nil, fmt.Errorf("error making request: %s", err.Error())
	}

	return contact, set, nil
}

func (p *Peer) New(item *domain.Item, root string, current string) error {

	distant, ok := network.nodes[p.Name()]
	if !ok {
		return fmt.Errorf("error code: 404")
	}

	err := distant.New(item, root, current)
	if err != nil {
		return fmt.Errorf("error making request: %s", err.Error())
	}

	return err
}
