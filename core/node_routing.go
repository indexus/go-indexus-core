package core

import (
	"log"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// mergeRouteKey is the XOR-routing key for (collection, location).
func (n *Node) mergeRouteKey(collection, location string) []byte {
	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		log.Println("Error decoding: ", err)
	}
	return id
}

// nearestRegistered returns the closest registered peer for this key.
func (n *Node) nearestRegistered(collection, location string) domain.Contact {
	return n.registered.Nearest(0, n.mergeRouteKey(collection, location))
}

// fanOutGet asks every peer (nearest first) until one returns a non-empty set.
func (n *Node) fanOutGet(collection, location string, nearest domain.Contact) (domain.Contact, *domain.Set, bool) {
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
		return contact, set, true
	}
	for _, peer := range n.traverseRegistered(false) {
		if contact, set, ok := visit(peer); ok {
			return contact, set, true
		}
	}
	return nil, nil, false
}

// getMultipleBucket groups locations that share the same remote owner for /sets.
type getMultipleBucket struct {
	owner     domain.Contact
	locations []string
}

func (n *Node) groupGetMultipleBuckets(collection string, locations []string) []*getMultipleBucket {
	buckets := make(map[string]*getMultipleBucket)
	order := make([]string, 0, len(locations))
	selfKey := ""

	for _, loc := range locations {
		owner := n.nearestRegistered(collection, loc)
		ownerName := ""
		if owner != nil {
			ownerName = owner.Name()
		}
		if owner == nil || ownerName == n.Name() {
			owner = nil
			if selfKey == "" {
				selfKey = "self"
			}
			ownerName = selfKey
		}
		b, exists := buckets[ownerName]
		if !exists {
			b = &getMultipleBucket{owner: owner}
			buckets[ownerName] = b
			order = append(order, ownerName)
		}
		b.locations = append(b.locations, loc)
	}

	out := make([]*getMultipleBucket, 0, len(order))
	for _, key := range order {
		out = append(out, buckets[key])
	}
	return out
}

func (n *Node) fetchGetMultipleBucket(
	collection string,
	b *getMultipleBucket,
	precision int,
	properties []func(*domain.Abelian) int,
) ([]byte, error) {
	if b.owner == nil {
		return n.getMultipleLocal(collection, b.locations, precision, properties)
	}
	remote, ok := b.owner.(interface {
		GetMultiple(string, []string, int, []func(*domain.Abelian) int) ([]byte, error)
	})
	if !ok {
		return nil, nil
	}
	return remote.GetMultiple(collection, b.locations, precision, properties)
}
