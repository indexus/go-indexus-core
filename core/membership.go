package core

import (
	"log/slog"
	"math/rand"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Observe() error {

	toIgnore := make([]domain.Contact, 0)
	toRegister := make([]domain.Contact, 0)
	toReject := make([]domain.Contact, 0)

	empty := true

	for _, contact := range n.traverseRegistered(false) {
		empty = false

		if !contactDialable(contact) {
			toReject = append(toReject, contact)
			continue
		}
		if _, err := contact.Ping(n); err != nil {

			n.suspend(contact.Name(), quarantineWindow)
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

	registeredNew := n.register(toRegister)

	if registeredNew {
		n.subscribe(toRegister)
	}

	n.MeasureItems()

	n.tryPublishClientReady()

	if registeredNew && !n.leaving.Load() && !n.rebalancing.Load() {
		go func() {
			if err := n.Refresh(); err != nil {
				slog.Warn("refresh-on-new-peer failed", "err", err)
			}
		}()
	}

	return nil
}

func (n *Node) Ping(origin domain.Contact) (domain.Contact, error) {

	if n.leaving.Load() {
		return nil, domain.ErrLeaving
	}

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

func (n *Node) register(contacts []domain.Contact) bool {
	if len(contacts) == 0 {
		return false
	}
	inserted := false
	for _, contact := range contacts {
		if !contactDialable(contact) || n.suspended(contact.Name()) {
			continue
		}
		_ = n.acknowledged.Remove(0, contact.ID())
		existing, exist := n.registered.Get(0, contact.ID())
		if !exist {
			n.registered.Insert(0, contact.ID(), contact)
			inserted = true
			continue
		}

		if !contactDialable(existing) || (existing.IP() == "" && contact.IP() != "") {
			n.registered.Remove(0, contact.ID())
			n.registered.Insert(0, contact.ID(), contact)
		}
	}
	return inserted
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

const quarantineWindow = 90 * time.Second

func (n *Node) suspend(name string, window time.Duration) {
	if name == "" || name == n.Name() {
		return
	}

	n.suspectMu.Lock()
	defer n.suspectMu.Unlock()

	until := time.Now().Add(window)
	if current, exist := n.suspect[name]; exist && current.After(until) {
		return
	}
	n.suspect[name] = until
}

func (n *Node) suspended(name string) bool {
	n.suspectMu.Lock()
	defer n.suspectMu.Unlock()

	until, exist := n.suspect[name]
	if !exist {
		return false
	}
	if time.Now().After(until) {
		delete(n.suspect, name)
		return false
	}
	return true
}

func contactDialable(c domain.Contact) bool {
	if c == nil || c.Port() <= 0 {
		return false
	}
	if c.IP() != "" {
		return true
	}
	for ip := range c.IPs() {
		if ip != "" {
			return true
		}
	}
	return false
}
