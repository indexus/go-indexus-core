package core

import (
	"log"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

func (n *Node) Observe() error {

	toIgnore := make([]domain.Contact, 0)
	toRegister := make([]domain.Contact, 0)
	toReject := make([]domain.Contact, 0)

	empty := true

	for _, contact := range n.traverseRegistered(false) {
		empty = false

		_, err := contact.Ping(n)
		if err != nil {
			toReject = append(toReject, contact)
		}
	}
	for _, contact := range n.traverseAcknowledged(false) {
		empty = false

		node, err := contact.Ping(n)
		if node != nil {
			toRegister = append(toRegister, node)
		}
		if err != nil || node == nil || node.Name() != contact.Name() {
			toIgnore = append(toIgnore, contact)
		}
	}

	if empty {
		n.acknowledge(n.bootstraps)
	}

	n.ignore(toIgnore)
	n.reject(toReject)
	n.register(toRegister)

	return nil
}

func (n *Node) Refresh() error {
	n.refreshRoutingContacts()
	n.handleOwnershipTransfers()
	if _, err := n.SaveClusterIfDirty(); err != nil {
		return err
	}
	n.maintainShards(time.Now())
	return n.clean()
}

// refreshRoutingContacts pulls neighbor lists from routing peers and registers them.
func (n *Node) refreshRoutingContacts() {
	toRegister := make([]domain.Contact, 0)
	for _, contact := range n.traverseRouting(false) {
		contacts, err := contact.Neighbors(n)
		if err != nil {
			continue
		}
		toRegister = append(toRegister, contacts...)
	}
	n.register(toRegister)
}

// handleOwnershipTransfers delegates shards to XOR-closer peers via control().
func (n *Node) handleOwnershipTransfers() {
	for candidate, keys := range n.control() {
		for key, items := range keys {
			if len(items) == 0 {
				continue
			}
			// control() may have deleted the collection if every owned
			// location was handed off; in that case there is no Base()
			// to walk so we just transfer with the original location.
			if collection, ok := n.collections.Get(key.Collection); ok {
				parent := key.Location
				for {
					k := domain.Key{
						Collection: key.Collection,
						Location:   parent,
					}
					if _, ok := keys[k]; !ok {
						break
					}
					if parent == collection.Base().Root() {
						break
					}
					key.Location, parent = parent, collection.Base().Parent(parent)
				}
			}
			if err := candidate.Transfer(n, key, items); err != nil {
				// Hand-off failed in transit. control() already removed
				// these items from our local collection AND truncated
				// our owned-key entry, so without a fallback they would
				// be silently lost. Re-queue them through the normal
				// insert path with the delegated subtree as their root
				// — exactly as a successful receiver's Transfer would
				// have done — so Feed will recreate the collection at
				// the right scope and route each item to whoever is
				// XOR-closest now (the same candidate, ourselves, or a
				// third node that joined since). Transient transport
				// failures therefore cost only an extra round; they no
				// longer leak data.
				log.Printf("transfer to %s for %s/%s failed: %v; re-enqueuing %d items",
					candidate.Name(), key.Collection, key.Location, err, len(items))
				for _, item := range items {
					_ = n.New(item, key.Location, key.Location)
				}
			} else {
				if err := n.storage.DropShard(key); err != nil {
					log.Printf("DropShard after transfer %v: %v", key, err)
				}
			}
		}
	}
}

// snapshotShard persists one shard header + WAL items when dirty.
func (n *Node) snapshotShard(key domain.Key, collection *domain.Collection) {
	header, items := extractShard(collection, key.Location)
	if header == nil && len(items) == 0 {
		return
	}
	if err := n.storage.SnapshotShard(key, header, items); err == nil {
		n.shards.MarkSnapshotted(key)
	}
}

// maintainShards runs PlanWork: snapshot hot shards, snapshot-then-evict cold ones.
func (n *Node) maintainShards(now time.Time) {
	// PlanWork returns disjoint sets: toSnapshot stays in RAM, toEvict gets
	// snapshotted (if dirty) then removed from RAM.
	toSnapshot, toEvict := n.shards.PlanWork(now)
	for _, key := range toSnapshot {
		if collection, ok := n.collections.Get(key.Collection); ok {
			n.snapshotShard(key, collection)
		}
	}
	for _, key := range toEvict {
		collection, ok := n.collections.Get(key.Collection)
		if !ok {
			n.shards.MarkEvicted(key)
			continue
		}
		// Race-window cover: PlanWork may have skipped the dirty bit
		// flip between its snapshot and our iteration here.
		if n.shards.IsDirty(key) {
			n.snapshotShard(key, collection)
		}
		collection.EvictShard(key.Location)
		n.shards.MarkEvicted(key)
	}
}

func (n *Node) Update() error {

	refresh := n.cache.Refresh(n.settings.expiration)

	for _, collection := range n.collections.List() {
		for location := range collection.Refresh() {
			if _, exist := refresh[collection.Name()]; !exist {
				refresh[collection.Name()] = make(map[string]any)
			}
			refresh[collection.Name()][location] = nil
		}
	}

	for collection, sets := range refresh {

		for location := range sets {

			contact, err := n.find(collection, location)
			if err != nil {
				log.Println(err)
			}

			if contact != nil && contact.Name() == n.Name() {
				continue
			}

			// n.Get fans out across all registered peers when the
			// XOR-nearest one doesn't actually hold the shard, so the
			// aggregate refresh works even when ownership ended up on
			// a peer that isn't routing-nearest. contact.Get on its own
			// would return nil here and the parent aggregate would
			// freeze (or worse, get rolled back to zero).
			_, set, err := n.Get(collection, location)
			if err != nil {
				log.Println(err)
			}

			// Only propagate when we actually got something — empty
			// or nil sets must not overwrite a healthy aggregate.
			if set == nil || set.Abelian().Count() == 0 {
				continue
			}

			n.cache.Set(collection, location, set)

			if c, exist := n.collections.Get(collection); exist {
				c.Update(location, set.Abelian())
			}
		}
	}
	return nil
}

func (n *Node) Feed() error {
	// queue.Consume blocks via sync.Cond until an item is available, so the
	// loop never busy-spins.
	for {
		element, _ := n.queue.Consume()
		if err := n.insert(element.item, element.root, element.current); err != nil {
			return err
		}
	}
}
