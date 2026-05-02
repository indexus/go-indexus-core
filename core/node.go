package core

import (
	"fmt"
	"log"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// insertMetrics tracks the disposition of every queued item so we can
// detect silent ingestion losses (forwarded vs added vs requeued vs
// rejected at the c.Add level).
type insertMetrics struct {
	added     int64
	addFail   int64
	requeued  int64
	forwarded int64
}

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
	shards       *ShardManager
	ready        bool
	metrics      insertMetrics

	// lastClusterHash skips redundant cluster snapshot writes when the
	// in-memory state has not changed since the last successful save.
	lastClusterHash uint64
}

// InsertMetrics returns a snapshot of (added, addFail, requeued, forwarded).
func (n *Node) InsertMetrics() (int64, int64, int64, int64) {
	return atomic.LoadInt64(&n.metrics.added),
		atomic.LoadInt64(&n.metrics.addFail),
		atomic.LoadInt64(&n.metrics.requeued),
		atomic.LoadInt64(&n.metrics.forwarded)
}

func NewNode(settings *Settings, newContact func(string, map[string]any, int) domain.Contact, bootstraps []domain.Contact, storage domain.Storage, shards *ShardManager) (*Node, error) {
	if shards == nil {
		shards = NewShardManager(0, 0) // unlimited, no TTL eviction
	}
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
		shards:       shards,
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

	id, err := encoding.BASE64.Decode(origin.Name())
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

// localFetch returns whatever this node holds for (collection, location)
// without ever forwarding. Used by GetLocal and as a fallback in Get.
func (n *Node) localFetch(collection, location string) *domain.Set {
	if c, exist := n.collections.Get(collection); exist {
		if set, ok := c.Get(location); ok {
			if owner, ownerOk := c.OwnerOf(location); ownerOk {
				n.shards.Touch(domain.Key{Collection: collection, Location: owner})
			}
			return set
		}
		if owner, ownerOk := c.OwnerOf(location); ownerOk {
			sk := domain.Key{Collection: collection, Location: owner}
			if err := n.shards.EnsureLoaded(sk, func() error { return n.loadShard(sk) }); err == nil {
				if set, ok := c.Get(location); ok {
					return set
				}
			}
		}
	}
	if set, exist := n.cache.Get(collection, location); exist {
		return set
	}
	return nil
}

// GetLocal answers strictly from this node's own state — never forwards.
// Peers call this variant via /set?local=1 to avoid recursive cross-node
// forwarding when the cluster routing owner doesn't actually hold the data.
func (n *Node) GetLocal(collection, location string) (domain.Contact, *domain.Set, error) {
	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		log.Println("Error decoding: ", err)
	}
	nearest := n.registered.Nearest(0, id)
	return nearest, n.localFetch(collection, location), nil
}

func (n *Node) Get(collection, location string) (domain.Contact, *domain.Set, error) {

	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		log.Println("Error decoding: ", err)
	}

	nearest := n.registered.Nearest(0, id)

	if nearest == nil || nearest.Name() == n.Name() {
		set := n.localFetch(collection, location)
		if set == nil {
			n.cache.Set(collection, location, nil)
		}
		return nearest, set, nil
	}

	// XOR-routing's nearest peer may not actually hold the shard if
	// ownership was placed at a different node when the keyspace was
	// less populated. Walk every registered peer (closest first) using
	// the local-only RPC variant so this fan-out can't recurse, and
	// fall back to our own state if no one has anything.
	tried := map[string]bool{n.Name(): true}
	visit := func(peer domain.Contact) (domain.Contact, *domain.Set, bool) {
		if peer == nil || tried[peer.Name()] {
			return nil, nil, false
		}
		tried[peer.Name()] = true
		contact, set, err := peer.Get(collection, location)
		if err != nil {
			return nil, nil, false
		}
		if set != nil && len(set.List()) > 0 {
			n.cache.Set(collection, location, set)
			return contact, set, true
		}
		return nil, nil, false
	}

	if contact, set, ok := visit(nearest); ok {
		return contact, set, nil
	}
	for _, peer := range n.traverseRegistered(false) {
		if contact, set, ok := visit(peer); ok {
			return contact, set, nil
		}
	}

	if set := n.localFetch(collection, location); set != nil {
		return nearest, set, nil
	}

	n.cache.Set(collection, location, nil)
	return nearest, nil, nil
}

func (n *Node) GetMultiple(collection string, locations []string, precision int, properties []func(*domain.Abelian) int) ([]byte, error) {
	if len(locations) == 0 {
		return nil, nil
	}

	// Group locations by their nearest owner so a single /sets that spans
	// multiple owners still returns data for every requested branch.
	// Without this, the server only handled the first owner and silently
	// dropped the remaining locations from the binary stream.
	type bucket struct {
		owner     domain.Contact
		locations []string
	}
	buckets := make(map[string]*bucket)
	order := make([]string, 0, len(locations))
	selfBucket := ""

	for _, loc := range locations {
		id, err := encoding.MergeEncodings(
			encoding.BASE64,
			encoding.BASE64,
			loc,
			collection,
		)
		var ownerName string
		var owner domain.Contact
		if err == nil {
			owner = n.registered.Nearest(0, id)
			if owner != nil {
				ownerName = owner.Name()
			}
		}
		if owner == nil || ownerName == n.Name() {
			ownerName = ""
			owner = nil
			if selfBucket == "" {
				selfBucket = "self"
			}
			ownerName = selfBucket
		}
		b, exists := buckets[ownerName]
		if !exists {
			b = &bucket{owner: owner}
			buckets[ownerName] = b
			order = append(order, ownerName)
		}
		b.locations = append(b.locations, loc)
	}

	var combined []byte
	for _, key := range order {
		b := buckets[key]
		if b.owner == nil {
			payload, err := n.getMultipleLocal(collection, b.locations, precision, properties)
			if err != nil {
				return nil, err
			}
			combined = append(combined, payload...)
			continue
		}
		if remote, ok := b.owner.(interface {
			GetMultiple(string, []string, int, []func(*domain.Abelian) int) ([]byte, error)
		}); ok {
			payload, err := remote.GetMultiple(collection, b.locations, precision, properties)
			if err != nil {
				// One owner being unavailable shouldn't blank the whole
				// response — keep merging successful buckets.
				continue
			}
			combined = append(combined, payload...)
		}
	}
	return combined, nil
}

func (n *Node) getMultipleLocal(collection string, locations []string, precision int, properties []func(*domain.Abelian) int) ([]byte, error) {
	c, ok := n.collections.Get(collection)
	if !ok {
		return nil, nil
	}

	// Ensure each relevant shard is loaded before aggregating.
	seen := make(map[domain.Key]bool)
	for _, location := range locations {
		if owner, ownerOk := c.OwnerOf(location); ownerOk {
			sk := domain.Key{Collection: collection, Location: owner}
			if !seen[sk] {
				seen[sk] = true
				if !n.shards.IsLoaded(sk) {
					if err := n.shards.EnsureLoaded(sk, func() error { return n.loadShard(sk) }); err != nil {
						return nil, err
					}
				} else {
					n.shards.Touch(sk)
				}
			}
		}
	}

	return c.GetMultiple(locations, precision, properties)
}

func (n *Node) New(item *domain.Item, root, current string) error {
	n.queue.Add(NewElement(item, root, current))
	return nil
}

// CacheStats returns a snapshot of shard cache counters for the /cache endpoint.
func (n *Node) CacheStats() map[string]int64 {
	s := n.shards.Stats()
	return map[string]int64{
		"loaded":    s.Loaded,
		"evicted":   s.Evicted,
		"dirty":     s.Dirty,
		"hits":      s.Hits,
		"misses":    s.Misses,
		"loads":     s.Loads,
		"evictions": s.Evictions,
	}
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

	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		log.Println("Error decoding: ", err)
	}

	if nearest := n.registered.Nearest(0, id); nearest != nil {
		return nearest, nil
	}
	return n, nil
}

func (n *Node) traverseAcknowledged(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.acknowledged.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRegistered(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.registered.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRouting(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.routing.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, p domain.Peer) {
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

	n.routing.Reset()

	n.subscribe(append(neighbors, n))
	return nil
}

func (n *Node) insert(item *domain.Item, root, current string) error {

	contact, err := n.find(item.Collection, current)
	if err != nil {
		return err
	}

	if n.Name() != contact.Name() {
		// Forwarding errors used to be swallowed, silently dropping items
		// whenever the remote peer was momentarily unreachable. Re-queue
		// locally so the next Feed cycle retries against the freshest
		// routing table.
		if err := contact.New(item, root, current); err != nil {
			log.Printf("forward to %s for %s/%s failed: %v; re-enqueuing", contact.Name(), item.Collection, current, err)
			return n.New(item, root, item.Location)
		}
		atomic.AddInt64(&n.metrics.forwarded, 1)
		return nil
	}

	if current == root {
		n.create(item.Collection, root)
	}

	if n.add(item) {
		atomic.AddInt64(&n.metrics.added, 1)
		return nil
	}

	atomic.AddInt64(&n.metrics.addFail, 1)

	current = encoding.BASE64.Parent(current)

	if len(current) == 0 {
		atomic.AddInt64(&n.metrics.requeued, 1)
		n.New(item, root, item.Location)
		return nil
	}

	return n.insert(item, root, current)
}

func (n *Node) create(col, root string) {

	collection, exist := n.collections.Get(col)
	if !exist {
		collection = domain.NewCollection(col, root, encoding.BASE64)
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
	if !exist {
		return false
	}

	areas := collection.Add(item.Location, item.Id, item.Metrics, n.settings.delegation)
	if areas == nil {
		return false
	}

	if n.ready {
		if owner, ok := collection.OwnerOf(item.Location); ok {
			sk := domain.Key{Collection: item.Collection, Location: owner}
			n.shards.MarkDirty(sk)
			n.storage.AppendShard(sk, item.Content())
			// Propagate dirty to all ancestor ownership zones so that their
			// shard headers remain in sync after this insert.
			n.markAncestorsDirty(collection, owner)
		}
	}

	if len(areas) > 0 {
		n.own(collection, areas)
	}

	return true
}

func (n *Node) own(collection *domain.Collection, owned domain.Ownership) {

	for location, delegation := range owned {

		id, err := encoding.MergeEncodings(
			encoding.BASE64,
			encoding.BASE64,
			location,
			collection.Name(),
		)
		if err != nil {
			log.Println("Error decoding: ", err)
		}

		n.owned.Upsert(0, id, map[domain.Key]any{}, func(i int, b []byte, m map[domain.Key]any) {
			m[domain.Key{Collection: collection.Name(), Location: location}] = nil

			collection.Own(location, delegation)
		})

		// During normal operation (not restore), the new shard's data is
		// already in RAM — mark it loaded so PlanWork can snapshot it.
		if n.ready {
			n.shards.MarkLoaded(domain.Key{Collection: collection.Name(), Location: location})
		}
	}
}

// markAncestorsDirty walks up the ownership tree from owner and marks every
// ancestor shard dirty. This keeps parent shard headers consistent when items
// are inserted into a child ownership zone.
func (n *Node) markAncestorsDirty(collection *domain.Collection, owner string) {
	current := owner
	for current != "@" {
		parentLoc := encoding.BASE64.Parent(current)
		if len(parentLoc) == 0 {
			break
		}
		parentOwner, ok := collection.OwnerOf(parentLoc)
		if !ok || parentOwner == current {
			break
		}
		n.shards.MarkDirty(domain.Key{Collection: collection.Name(), Location: parentOwner})
		current = parentOwner
	}
}

// registerOwnedLocation registers (collection, location) in the routing-side
// `owned` BST without touching the collection's owned/delegation map. Used
// during Restore where the collection state is rebuilt by RestoreOwned and
// AppendDelegation.
func (n *Node) registerOwnedLocation(collection *domain.Collection, location string) {
	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection.Name(),
	)
	if err != nil {
		log.Println("Error decoding: ", err)
		return
	}

	n.owned.Upsert(0, id, map[domain.Key]any{}, func(i int, b []byte, m map[domain.Key]any) {
		m[domain.Key{Collection: collection.Name(), Location: location}] = nil
	})
}

func (n *Node) control() map[domain.Contact]map[domain.Key][]*domain.Item {

	transferable := make(map[domain.Contact]map[domain.Key][]*domain.Item)
	for _, candidate := range n.traverseRouting(false) {

		n.owned.Range(0, n.ID(), candidate.ID(), encoding.BASE64.NewID(), func(idx int, id []byte, sets map[domain.Key]any) {

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
