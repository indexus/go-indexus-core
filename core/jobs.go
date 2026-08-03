package core

import (
	"log"
	"math/rand"
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

	toRegister := make([]domain.Contact, 0)

	for _, contact := range n.traverseRouting(false) {
		contacts, err := contact.Neighbors(n)
		if err != nil {
			continue
		}
		toRegister = append(toRegister, contacts...)
	}

	n.register(toRegister)

	for candidate, keys := range n.control() {
		for key, items := range keys {

			if len(items) == 0 {
				continue
			}

			// Collection may already be gone if control() handed off every location.
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
				// Items were already removed locally; re-queue so they are not lost.
				requeued := 0
				for _, item := range items {
					if n.New(item, key.Location, key.Location) != nil {
						break
					}
					requeued++
				}
				log.Printf("transfer to %s for %s/%s failed: %v; re-enqueued %d/%d items",
					candidate.Name(), key.Collection, key.Location, err, requeued, len(items))
			}
		}
	}

	// n.Snapshot()
	err := n.storage.Save([]string{})
	if err != nil {
		return err
	}

	err = n.clean()
	if err != nil {
		return err
	}

	return nil
}

func (n *Node) Update() error {

	if n.settings.cacheMax > 0 {
		n.cache.SetMax(n.settings.cacheMax)
	}

	refresh := n.cache.Refresh(n.settings.expiration, n.settings.cacheBeta)

	for _, collection := range n.collections.List() {
		for location := range collection.Refresh() {
			if _, exist := refresh[collection.Name()]; !exist {
				refresh[collection.Name()] = make(map[string]any)
			}
			refresh[collection.Name()][location] = nil
		}
	}

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

		// η: probabilistic skip to smooth owner refresh pressure.
		if n.settings.updateEta > 0 && rand.Float64() < n.settings.updateEta {
			continue
		}

		// Parent-refresh: ask the nearest registered peer (ring), depth 0 — no recursive fill.
		contact, err := n.find(collection, location)
		if err != nil {
			log.Println(err)
			continue
		}
		if contact == nil || contact.Name() == n.Name() {
			continue
		}

		_, set, err := contact.Get(collection, location, 0)
		if err != nil {
			log.Println(err)
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

func (n *Node) Feed() error {
	for {
		element, exist := n.queue.Consume()
		if !exist {
			continue
		}
		if err := n.process(element); err != nil {
			return err
		}
	}
}

func (n *Node) process(element *Element) error {
	if element.op == OpDelete {
		return n.deleteOp(element.item, element.root, element.current)
	}
	return n.insert(element.item, element.root, element.current)
}
