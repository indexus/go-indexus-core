package core

import (
	"fmt"
	"log/slog"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// claimSender is implemented by peer contacts that speak /claim.
type claimSender interface {
	Claim(domain.ClaimPayload) error
}

// repair walks every locally owned zone and pushes a Claim to the XOR-nearest
// holder of its parent. It also verifies named marks: membership declaring the
// named peer dead/quarantined triggers Own+wake of the parked garage. Silence
// alone never lifts a mark.
func (n *Node) repair() {
	for _, key := range n.listOwnedKeys() {
		n.pushClaim(key)
	}
	n.verifyNamedMarks()
}

func (n *Node) pushClaim(key domain.Key) {
	if !n.allowParentSync(key.Collection, key.Location) {
		return
	}
	parent := encoding.BASE64.Parent(key.Location)
	if parent == "" {
		return
	}
	contact, err := n.find(key.Collection, parent)
	if err != nil || contact == nil {
		return
	}
	if contact.Name() == n.Name() {
		if c, ok := n.collections.Get(key.Collection); ok {
			c.MarkDelegatedTo(parent, key.Location, n.Name())
			n.wakeParked(key.Collection, key.Location)
			n.markParentSync(key.Collection, key.Location)
		}
		return
	}
	sender, ok := contact.(claimSender)
	if !ok {
		return
	}
	payload := domain.ClaimPayload{
		Collection: key.Collection,
		Parent:     parent,
		Child:      key.Location,
		Peer:       n.Name(),
	}
	if err := sender.Claim(payload); err != nil {
		slog.Debug("claim push failed",
			"collection", key.Collection, "child", key.Location,
			"parent_peer", contact.Name(), "err", err)
		return
	}
	n.markParentSync(key.Collection, key.Location)
}

// Claim is the inbound handler: if we own parent, record the named mark and
// pull the child's aggregate. Parking under that child is woken so writes retry.
func (n *Node) Claim(origin domain.Peer, payload domain.ClaimPayload) error {
	if payload.Collection == "" || payload.Child == "" || payload.Parent == "" {
		return fmt.Errorf("incomplete claim")
	}
	if origin == nil {
		return fmt.Errorf("claim without origin")
	}
	if payload.Peer != "" && payload.Peer != origin.Name() {
		return fmt.Errorf("claim peer %q does not match origin %q", payload.Peer, origin.Name())
	}
	c, ok := n.collections.Get(payload.Collection)
	if !ok || !c.Owns(payload.Parent) {
		return nil
	}
	if !domain.IsDirectChild(c.Base(), payload.Parent, payload.Child) {
		return nil
	}
	peer := payload.Peer
	if peer == "" {
		peer = origin.Name()
	}
	c.MarkDelegatedTo(payload.Parent, payload.Child, peer)
	n.wakeParked(payload.Collection, payload.Child)

	if peer != "" && peer != n.Name() {
		if contact := n.lookupPeer(peer); contact != nil {
			n.pullDelegated(payload.Collection, payload.Child, contact)
		}
	}
	return nil
}

func (n *Node) lookupPeer(name string) domain.Contact {
	for _, c := range n.traverseRegistered(false) {
		if c != nil && c.Name() == name {
			return c
		}
	}
	for _, c := range n.traverseAcknowledged(false) {
		if c != nil && c.Name() == name {
			return c
		}
	}
	return nil
}

func (n *Node) verifyNamedMarks() {
	for _, collection := range n.collections.List() {
		type mark struct{ parent, child, peer string }
		var marks []mark
		collection.Browse(
			func(string) {},
			func(parent, child string, h domain.Handoff) {
				if h.Peer == "" {
					return
				}
				marks = append(marks, mark{parent, child, h.Peer})
			},
		)
		for _, m := range marks {
			if n.suspended(m.peer) {
				slog.Warn("named mark holder quarantined, reclaiming child",
					"collection", collection.Name(), "child", m.child, "peer", m.peer)
				n.reclaimParkedChild(collection, m.child)
				continue
			}
			contact := n.lookupPeer(m.peer)
			if contact == nil {
				// Absence alone is not a verdict — the named peer may simply
				// not have been discovered yet. Absence after quarantine is:
				// the peer was known, failed, and has since been evicted.
				if n.ghostedPeer(m.peer) {
					slog.Warn("named mark holder evicted after quarantine, reclaiming child",
						"collection", collection.Name(), "child", m.child, "peer", m.peer)
					n.reclaimParkedChild(collection, m.child)
				}
				continue
			}
			if n.pullDelegated(collection.Name(), m.child, contact) == pullOK {
				n.wakeParked(collection.Name(), m.child)
			}
		}
	}
}
