package core

import (
	"log"

	"gitlab.com/indexus/node/domain"
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
		if err != nil || node.Name() != contact.Name() {
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
			candidate.Transfer(n, key, items)
		}
	}

	err := n.storage.Save(n.Snapshot())
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

			_, set, err := contact.Get(collection, location)
			if err != nil {
				log.Println(err)
			}

			if set == nil {
				continue
			}

			n.cache.Set(collection, location, set)

			if c, exist := n.collections.Get(collection); exist {
				c.Update(domain.Parent(location), location, set.Count())
			}
		}
	}
	return nil
}

func (n *Node) Feed() error {
	for {
		element, exist := n.queue.Consume()
		if !exist {
			continue
		}
		err := n.insert(element.item, element.root, element.current)
		if err != nil {
			return err
		}
	}
}
