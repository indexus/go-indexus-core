package core

import (
	"fmt"
	"log"
	"math/rand"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

type Node struct {
	settings     *Settings
	newContact   func(string, map[string]any, int) domain.Contact
	bootstraps   []domain.Contact
	routing      *domain.BST[domain.Peer]
	registered   *domain.BST[domain.Contact]
	acknowledged *domain.BST[domain.Contact]
	collections  *domain.Collections
	owned        *domain.BST[map[domain.Key]any]
	cache        *domain.Cache
	queue        *domain.Queue[*Element]
	storage      domain.Storage
	ready        bool
}

func NewNode(settings *Settings, newContact func(string, map[string]any, int) domain.Contact, bootstraps []domain.Contact, storage domain.Storage) (*Node, error) {
	node := &Node{
		settings:     settings,
		newContact:   newContact,
		bootstraps:   bootstraps,
		routing:      domain.NewBST[domain.Peer](),
		registered:   domain.NewBST[domain.Contact](),
		acknowledged: domain.NewBST[domain.Contact](),
		collections:  domain.NewCollections(),
		owned:        domain.NewBST[map[domain.Key]any](),
		cache:        domain.NewCache(),
		queue:        domain.NewQueue[*Element](),
		storage:      storage,
	}

	node.register([]domain.Contact{node})
	node.acknowledge(bootstraps)

	if err := node.Restore(); err != nil {
		fmt.Printf("issue when restoring the node from backup: %v", err)

		if err := node.storage.Reset(); err != nil {
			return nil, fmt.Errorf("issue when resetting the storage: %v", err)
		}
	}

	node.ready = true

	return node, nil
}

func (n *Node) ID() []byte {
	return n.settings.id
}

func (n *Node) Name() string {
	return n.settings.name
}

func (n *Node) IPs() map[string]any {
	return n.settings.ips
}

func (n *Node) Port() int {
	return n.settings.port
}

func (n *Node) IP() string {
	return n.settings.ip
}

func (n *Node) Host() string {
	return fmt.Sprintf("%s@%s|%d", n.settings.name, n.settings.ip, n.settings.port)
}

func (n *Node) Delay() time.Duration {
	return n.settings.delay
}

func (n *Node) Ping(origin domain.Contact) (domain.Contact, error) {

	if len(origin.Name()) > 0 {
		n.acknowledge([]domain.Contact{origin})
	}

	return n, nil
}

func (n *Node) Neighbors(origin domain.Peer) ([]domain.Contact, error) {

	id, err := domain.DecodeName(origin.Name())
	if err != nil {
		return nil, err
	}

	neighbors := &[160]domain.Contact{}
	n.registered.Extract(0, id, neighbors)

	result := []domain.Contact{}
	for _, neighbor := range neighbors {
		if neighbor != nil {
			result = append(result, neighbor)
		}
	}
	return result, nil
}

func (n *Node) Random(origin domain.Peer) (domain.Contact, error) {

	contacts := n.traverseRegistered(false)

	if len(contacts) == 0 {
		return nil, nil
	}

	return contacts[rand.Intn(len(contacts))], nil
}

func (n *Node) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {

	for _, item := range items {
		n.New(item, key.Location, key.Location)
	}

	return nil
}

func (n *Node) Get(collection, location string) (domain.Contact, *domain.Set, error) {

	name := collection
	if location != domain.Root() {
		name = location + collection[len(location):]
	}

	id, err := domain.DecodeName(name)
	if err != nil {
		return nil, nil, err
	}

	nearest := n.registered.Nearest(0, id)

	if collection, exist := n.collections.Get(collection); exist {
		set, ok := collection.Get(location)
		if ok {
			return nearest, set, nil
		}
	}

	if collection, exist := n.collections.Get(collection); exist {
		set, ok := collection.Get(location)
		if ok {
			return nearest, set, nil
		}
	}

	if set, exist := n.cache.Get(collection, location); exist {
		return nearest, set, nil
	}

	n.cache.Set(collection, location, nil)

	return nearest, nil, nil
}

func (n *Node) New(item *domain.Item, root, current string) error {
	n.queue.Add(NewElement(item, root, current))
	return nil
}

func (n *Node) acknowledge(candidates []domain.Contact) {
	if len(candidates) == 0 {
		return
	}
	for _, candidate := range candidates {
		_, exist := n.registered.Get(0, candidate.ID())
		if !exist {
			n.acknowledged.Insert(0, candidate.ID(), candidate)
		}
	}
}

func (n *Node) ignore(candidates []domain.Contact) {
	if len(candidates) == 0 {
		return
	}
	for _, candidate := range candidates {
		n.acknowledged.Remove(0, candidate.ID())
	}
}

func (n *Node) register(contacts []domain.Contact) {
	if len(contacts) == 0 {
		return
	}
	for _, contact := range contacts {
		_ = n.acknowledged.Remove(0, contact.ID())
		_, exist := n.registered.Get(0, contact.ID())
		if !exist {
			n.registered.Insert(0, contact.ID(), contact)
		}
	}
}

func (n *Node) reject(contacts []domain.Contact) {
	if len(contacts) == 0 {
		return
	}
	for _, contact := range contacts {
		n.routing.Remove(0, contact.ID())
	}
	for _, contact := range contacts {
		n.registered.Remove(0, contact.ID())
	}
}

func (n *Node) subscribe(contacts []domain.Contact) {
	if len(contacts) == 0 {
		return
	}
	for _, contact := range contacts {
		n.routing.Insert(0, contact.ID(), contact)
	}
}

func (n *Node) find(collection, location string) (domain.Contact, error) {

	name := collection
	if location != domain.Root() {
		name = location + collection[len(location):]
	}

	id, err := domain.DecodeName(name)
	if err != nil {
		return nil, err
	}

	if nearest := n.registered.Nearest(0, id); nearest != nil {
		return nearest, nil
	}
	return n, nil
}

func (n *Node) traverseAcknowledged(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.acknowledged.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRegistered(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.registered.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRouting(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.routing.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, p domain.Peer) {
		if !self && n.Name() == p.Name() {
			return
		}
		c, exist := n.registered.Get(0, p.ID())
		if exist {
			contacts = append(contacts, c)
		}
	})
	return contacts
}

func (n *Node) clean() error {

	neighbors, err := n.Neighbors(n)
	if err != nil {
		return err
	}

	n.routing = domain.NewBST[domain.Peer]()

	n.subscribe(append(neighbors, n))
	return nil
}

func (n *Node) insert(item *domain.Item, root, current string) error {

	contact, err := n.find(item.Collection, current)
	if err != nil {
		return err
	}

	if n.Name() != contact.Name() {
		contact.New(item, root, current)
		return nil
	}

	if current == root {
		n.create(item.Collection, root)
	}

	if n.add(item) {
		return nil
	}

	current = domain.Parent(current)

	if len(current) == 0 {
		n.New(item, root, item.Location)
		return nil
	}

	return n.insert(item, root, current)
}

func (n *Node) create(col, root string) {

	collection, exist := n.collections.Get(col)
	if !exist {
		collection = domain.NewCollection(col, root)
	}

	_, exist = collection.Get(root)
	if !exist {
		collection.New(root)
	}

	n.own(collection, collection.Complete(root))

	n.collections.Set(collection)
}

func (n *Node) add(item *domain.Item) bool {

	collection, exist := n.collections.Get(item.Collection)
	if !exist || !collection.Allowing(item.Location) {
		return false
	}

	areas := collection.Add(item.Location, item.Id, n.settings.setLength, n.settings.delegation)
	if n.ready {
		n.storage.Append(item.Content())
	}

	if len(areas) > 0 {
		n.own(collection, areas)
	}

	return true
}

func (n *Node) own(collection *domain.Collection, owned domain.Ownership) {

	for location, delegation := range owned {

		id, err := domain.DecodeLocation(collection.Name(), location)
		if err != nil {
			log.Println("Error decoding: ", err)
		}

		n.owned.Upsert(0, id, map[domain.Key]any{}, func(i int, b []byte, m map[domain.Key]any) {
			m[domain.Key{Collection: collection.Name(), Location: location}] = nil

			collection.Own(location, delegation)
		})
	}
}

func (n *Node) control() map[domain.Contact]map[domain.Key][]*domain.Item {

	transferable := make(map[domain.Contact]map[domain.Key][]*domain.Item)
	for _, candidate := range n.traverseRouting(false) {

		n.owned.Range(0, n.ID(), candidate.ID(), make([]byte, domain.IdLength()), func(idx int, id []byte, sets map[domain.Key]any) {
			for key := range sets {

				_, exist := transferable[candidate]
				if !exist {
					transferable[candidate] = make(map[domain.Key][]*domain.Item, 0)
				}

				collection, _ := n.collections.Get(key.Collection)

				items, empty := collection.Delegate(key.Location)
				if empty {
					n.collections.Delete(key.Collection)
				}

				transferable[candidate][key] = items
			}
		})
		n.owned.Truncate(0, n.ID(), candidate.ID())
	}
	return transferable
}
