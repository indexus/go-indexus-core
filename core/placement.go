package core

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) find(collection, location string) (domain.Contact, error) {

	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		return nil, fmt.Errorf("key %s/%s: %w", collection, location, err)
	}

	nearest := n.registered.Nearest(0, id)
	if nearest == nil {
		return n, nil
	}

	if n.suspended(nearest.Name()) {
		if live := n.nearestLive(id); live != nil {
			return live, nil
		}
		return nil, domain.ErrOwnerUnavailable
	}

	return nearest, nil
}

func (n *Node) nearestLive(id []byte) domain.Contact {
	var best domain.Contact
	var bestDistance []byte

	for _, contact := range n.traverseRegistered(false) {
		if !contactDialable(contact) || n.suspended(contact.Name()) {
			continue
		}
		distance := xorDistance(contact.ID(), id)
		if best == nil || bytes.Compare(distance, bestDistance) < 0 {
			best, bestDistance = contact, distance
		}
	}
	return best
}

func xorDistance(left, right []byte) []byte {
	size := min(len(left), len(right))
	out := make([]byte, size)
	for i := 0; i < size; i++ {
		out[i] = left[i] ^ right[i]
	}
	return out
}

// viaCap bounds how many peers a single write attempt may cross. Symmetric to
// zoneFanout on reads: large enough that one stale route heals on the same
// attempt, small enough that a mesh mid-convergence cannot grow the request
// without bound.
const viaCap = 16

func placementParent(current, root string) string {
	parent := encoding.BASE64.Parent(current)
	if parent == "" && current != root {
		return root
	}
	return parent
}

// insert walks a write to the zone that owns it. Every attempt is a pure
// function of the item's address and the local membership view: the climb
// always starts at item.Location. via names peers that already declined on
// this attempt so a pair with divergent tables cannot trade the write forever
// (symmetric to K8 on reads). via is discarded on requeue.
func (n *Node) insert(item *domain.Item, root string, via domain.Visited, meter bool) error {
	if via.Has(n.Name()) {
		return domain.ErrOwnerUnavailable
	}
	if len(via) >= viaCap {
		return domain.ErrOwnerUnavailable
	}
	nextVia := via.With(n.Name())

	current := item.Location
	for len(current) > 0 {
		// A root owner remains authoritative while membership converges. Serve
		// it before XOR forwarding so a routing cycle cannot manufacture a
		// second root merely because the existing owner is not nearest yet.
		if current == root && n.add(item, meter) {
			return nil
		}
		contact, err := n.find(item.Collection, current)
		if err != nil {
			return err
		}

		if n.Name() != contact.Name() {
			if nextVia.Has(contact.Name()) {
				if current == root {
					goto insertLocal
				}
				current = placementParent(current, root)
				continue
			}
			err := n.forwardNew(contact, item, root, nextVia)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrQueueFull) && !errors.Is(err, domain.ErrPeerBusy) {
				return err
			}
			// A remote refusal is a local fact for this attempt. Bound it in
			// via and keep climbing instead of hot-looping on that same peer.
			nextVia = nextVia.With(contact.Name())
			if len(nextVia) >= viaCap {
				return domain.ErrOwnerUnavailable
			}
			if current == root {
				goto insertLocal
			}
			current = placementParent(current, root)
			continue
		}

	insertLocal:
		if current == root {
			n.create(item.Collection, root)
		}

		if n.add(item, meter) {
			return nil
		}
		// Local nearest but Add refused: a delegated child covers the item.
		// Prefer the named mark holder when dialable — XOR can still point here
		// (parent owner) while the child lives elsewhere. Park only when that
		// peer is unreachable; climbing into aggregates would corrupt Shrink.
		if child, handoff, ok := n.delegatedCover(item.Collection, item.Location); ok {
			return n.forwardOrParkDelegated(item, root, nextVia, child, handoff.Peer, n.forwardNew)
		}

		current = placementParent(current, root)
	}

	return domain.ErrOwnerUnavailable
}

// forwardOrParkDelegated places a write at the named mark holder when local
// Add/Remove refused under a covering mark. Returning ErrDelegatedTo parks the
// element; a successful forward clears it from this node.
func (n *Node) forwardOrParkDelegated(
	item *domain.Item,
	root string,
	via domain.Visited,
	child, peer string,
	fwd func(domain.Contact, *domain.Item, string, domain.Visited) error,
) error {
	blocked := domain.ErrDelegatedTo{Child: child, Peer: peer}
	if peer == "" || peer == n.Name() || via.Has(peer) || n.suspended(peer) {
		return blocked
	}
	contact := n.contactByName(peer)
	if contact == nil {
		return blocked
	}
	if err := fwd(contact, item, root, via); err != nil {
		return blocked
	}
	return nil
}

func (n *Node) delegatedCover(collection, location string) (string, domain.Handoff, bool) {
	c, ok := n.collections.Get(collection)
	if !ok {
		return "", domain.Handoff{}, false
	}
	return c.DelegatedCover(location)
}

func (n *Node) deleteOp(item *domain.Item, root string, via domain.Visited) error {
	if via.Has(n.Name()) {
		return domain.ErrOwnerUnavailable
	}
	if len(via) >= viaCap {
		return domain.ErrOwnerUnavailable
	}
	nextVia := via.With(n.Name())

	current := item.Location
	for len(current) > 0 {
		// Ownership outranks XOR for deletes: a covering owner must apply the
		// tombstone locally before any forward. Otherwise find(item.Location)
		// can point at a nearer non-owner that plants a local tombstone while
		// the item remains on the true owner of an ancestor zone.
		if n.ownsLocation(item.Collection, current) && n.remove(item) {
			return nil
		}
		if current == root && n.remove(item) {
			return nil
		}
		contact, err := n.find(item.Collection, current)
		if err != nil {
			return err
		}

		if n.Name() != contact.Name() {
			if nextVia.Has(contact.Name()) {
				if current == root {
					goto deleteLocal
				}
				current = placementParent(current, root)
				continue
			}
			err := n.forwardDelete(contact, item, root, nextVia)
			if err == nil {
				return nil
			}
			if !errors.Is(err, ErrQueueFull) && !errors.Is(err, domain.ErrPeerBusy) {
				return err
			}
			nextVia = nextVia.With(contact.Name())
			if len(nextVia) >= viaCap {
				return domain.ErrOwnerUnavailable
			}
			if current == root {
				goto deleteLocal
			}
			current = placementParent(current, root)
			continue
		}

	deleteLocal:
		if n.remove(item) {
			return nil
		}
		if child, handoff, ok := n.delegatedCover(item.Collection, item.Location); ok {
			return n.forwardOrParkDelegated(item, root, nextVia, child, handoff.Peer, n.forwardDelete)
		}
		// Do not create ownership here: minting a root just to plant a
		// tombstone duplicates authority and never reaches the real owner.

		current = placementParent(current, root)
	}

	return domain.ErrOwnerUnavailable
}

func (n *Node) create(col, root string) {

	collection := n.collections.Ensure(col, root, encoding.BASE64)

	collection.EnsureSet(root)

	n.own(collection, collection.Complete(root))
}

func (n *Node) add(item *domain.Item, meter bool) bool {

	collection, exist := n.collections.Get(item.Collection)
	if !exist {
		return false
	}

	areas := collection.Add(item.Location, item.Id, item.Metrics, n.settings.delegation)
	if areas == nil {
		return false
	}

	if n.ready {
		n.storage.Append(item.Content())
	}

	n.markOwnedDirtyForItem(item.Collection, item.Location)

	if len(areas) > 0 {
		n.own(collection, areas)
	}

	if meter && n.autoscale != nil && !n.leaving.Load() {
		n.autoscale.RecordInsertAt(item.Collection, item.Location)
	}

	return true
}

func (n *Node) remove(item *domain.Item) bool {
	collection, exist := n.collections.Get(item.Collection)
	if !exist {
		return false
	}

	_, gen, ok := collection.Remove(item.Location, item.Id)
	if !ok {
		return false
	}

	if n.ready {
		n.storage.Append((&domain.Item{
			Collection: item.Collection,
			Location:   item.Location,
			Id:         item.Id,
			Tombstone:  true,
			Gen:        gen,
		}).Content())
	}
	n.markOwnedDirtyForItem(item.Collection, item.Location)
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

			slog.Error("skipping unencodable ownership", "collection", collection.Name(), "location", location, "err", err)
			continue
		}

		n.owned.Upsert(0, id, map[domain.Key]any{}, func(i int, b []byte, m map[domain.Key]any) {
			m[domain.Key{Collection: collection.Name(), Location: location}] = nil
		})
		collection.Own(location, delegation)
		if parent := collection.Base().Parent(location); parent != "" {
			collection.MarkDelegatedTo(parent, location, n.Name())
		}
	}
	n.tryPublishClientReady()
}

func (n *Node) allowForward() bool {
	rate := n.settings.forwardRate
	if rate <= 0 {
		return true
	}

	n.fwdMu.Lock()
	defer n.fwdMu.Unlock()

	now := time.Now()
	elapsed := now.Sub(n.fwdLast).Seconds()
	n.fwdLast = now
	n.fwdTokens += elapsed * float64(rate)
	if n.fwdTokens > float64(rate) {
		n.fwdTokens = float64(rate)
	}
	if n.fwdTokens < 1 {
		return false
	}
	n.fwdTokens--
	return true
}

const (
	peerBusyBase = 100 * time.Millisecond
	peerBusyCap  = 2 * time.Second
)

type peerBackoff struct {
	until time.Time
	step  time.Duration
}

func (n *Node) markPeerBusy(name string) {
	if name == "" || name == n.Name() {
		return
	}
	n.peerBusyMu.Lock()
	defer n.peerBusyMu.Unlock()
	now := time.Now()
	cur, exist := n.peerBusy[name]
	if exist && now.Before(cur.until) {
		return
	}
	step := peerBusyBase
	if exist && cur.step > 0 {
		step = cur.step * 2
		if step > peerBusyCap {
			step = peerBusyCap
		}
	}
	n.peerBusy[name] = peerBackoff{until: now.Add(step), step: step}
}

func (n *Node) clearPeerBusy(name string) {
	if name == "" {
		return
	}
	n.peerBusyMu.Lock()
	delete(n.peerBusy, name)
	n.peerBusyMu.Unlock()
}

func (n *Node) isPeerBusy(name string) bool {
	n.peerBusyMu.Lock()
	defer n.peerBusyMu.Unlock()
	cur, exist := n.peerBusy[name]
	if !exist {
		return false
	}
	if time.Now().After(cur.until) {
		delete(n.peerBusy, name)
		return false
	}
	return true
}

func (n *Node) forwardNew(contact domain.Contact, item *domain.Item, root string, via domain.Visited) error {
	if n.isPeerBusy(contact.Name()) {
		return domain.ErrPeerBusy
	}
	if !n.allowForward() {
		return ErrQueueFull
	}
	err := contact.New(item, root, via)
	if err != nil {
		n.markPeerBusy(contact.Name())
		return err
	}
	n.clearPeerBusy(contact.Name())
	return nil
}

func (n *Node) forwardDelete(contact domain.Contact, item *domain.Item, root string, via domain.Visited) error {
	if n.isPeerBusy(contact.Name()) {
		return domain.ErrPeerBusy
	}
	if !n.allowForward() {
		return ErrQueueFull
	}
	err := contact.Delete(item, root, via)
	if err != nil {
		n.markPeerBusy(contact.Name())
		return err
	}
	n.clearPeerBusy(contact.Name())
	return nil
}
