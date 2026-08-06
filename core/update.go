package core

import (
	"log/slog"
	"math/rand"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

func (n *Node) Update() error {

	if n.settings.cacheMax > 0 {
		n.cache.SetMax(n.settings.cacheMax)
	}

	// Delegated children live on peers; parent abelian entries only converge
	// via pulls. Always reconcile those first (no updateEta skip).
	n.reconcileDelegatedParents()

	refresh := n.cache.Refresh(n.settings.expiration, n.settings.cacheBeta)

	type key struct {
		collection string
		location   string
	}
	keys := make([]key, 0)
	for collection, sets := range refresh {
		for location := range sets {
			keys = append(keys, key{collection, location})
		}
	}
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for _, k := range keys {
		collection, location := k.collection, k.location

		if n.settings.updateEta > 0 && rand.Float64() < n.settings.updateEta {
			continue
		}

		contact, err := n.find(collection, location)
		if err != nil {
			slog.Debug("cache refresh has no route", "collection", collection, "location", location, "err", err)
			continue
		}
		if contact == nil || contact.Name() == n.Name() {
			continue
		}

		_, set, err := contact.Get(collection, location, false, 0)
		if err != nil {
			slog.Debug("cache refresh unanswered", "peer", contact.Name(), "collection", collection, "location", location, "err", err)
			continue
		}
		if set == nil {
			continue
		}

		hops := n.cache.Hops(collection, location)
		if hops < 1 {
			hops = 1
		}
		n.cache.SetHops(collection, location, set, hops)

		if c, exist := n.collections.Get(collection); exist {
			c.Update(location, set.Abelian())
		}

		time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
	}
	return nil
}

// reconcileDelegatedParents pulls remote abelian summaries for every child
// location still marked delegated on a local parent. This is what keeps
// parent blocks converging toward leaf truth after rebalance — exactness is
// eventual, but Update must not leave parents frozen when find()==self.
func (n *Node) reconcileDelegatedParents() {
	for _, collection := range n.collections.List() {
		for location := range collection.Refresh() {
			n.pullDelegatedAggregate(collection.Name(), location, nil)
		}
	}
}

// pullDelegatedAggregate fetches a remote set for a delegated child location
// and applies it into local parent entries via Collection.Update.
// prefer, when set, is tried first (the handoff receiver).
func (n *Node) pullDelegatedAggregate(collection, location string, prefer domain.Contact) bool {
	peers := make([]domain.Contact, 0, 8)
	seen := make(map[string]struct{})

	add := func(c domain.Contact) {
		if c == nil || c.Name() == n.Name() || !contactDialable(c) {
			return
		}
		if _, ok := seen[c.Name()]; ok {
			return
		}
		seen[c.Name()] = struct{}{}
		peers = append(peers, c)
	}

	add(prefer)
	if contact, err := n.find(collection, location); err == nil {
		add(contact)
	}
	for _, c := range n.traverseRegistered(false) {
		add(c)
	}
	// Peers we have pinged but not yet promoted into registered still carry
	// leaf truth after a fresh handoff — try them when XOR still points at self.
	for _, c := range n.traverseAcknowledged(false) {
		add(c)
	}

	for _, peer := range peers {
		_, set, err := peer.Get(collection, location, false, 0)
		if err != nil || set == nil {
			continue
		}
		if c, ok := n.collections.Get(collection); ok {
			c.Update(location, set.Abelian())
			n.cache.SetHops(collection, location, set, 1)
			return true
		}
	}
	return false
}

// syncParentsAfterHandoff forces an immediate parent-summary pull for each
// transferred key from the receiving peer — closes the post-Delegate window
// before the next Update tick.
func (n *Node) syncParentsAfterHandoff(keys []domain.Key, receiver domain.Peer) {
	var prefer domain.Contact
	if c, ok := receiver.(domain.Contact); ok {
		prefer = c
	} else if receiver != nil {
		prefer = n.findRegisteredByName(receiver.Name())
	}
	for _, key := range keys {
		if n.pullDelegatedAggregate(key.Collection, key.Location, prefer) {
			continue
		}
		slog.Debug("parent sync after handoff missed",
			"collection", key.Collection,
			"location", key.Location,
			"receiver", receiverName(receiver),
		)
	}
}

func receiverName(c domain.Peer) string {
	if c == nil {
		return ""
	}
	return c.Name()
}
