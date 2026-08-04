package core

import (
	"bytes"
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

func (n *Node) insert(item *domain.Item, root, current string, meter bool) error {

	contact, err := n.find(item.Collection, current)
	if err != nil {
		return err
	}

	if n.Name() != contact.Name() {

		return n.forwardNew(contact, item, root, current)
	}

	if current == root {
		n.create(item.Collection, root)
	}

	if n.add(item, meter) {
		return nil
	}

	current = encoding.BASE64.Parent(current)

	if len(current) == 0 {
		el := NewElement(item, root, item.Location)
		el.meter = meter
		n.queue.Add(el)
		return nil
	}

	return n.insert(item, root, current, meter)
}

func (n *Node) deleteOp(item *domain.Item, root, current string) error {
	contact, err := n.find(item.Collection, current)
	if err != nil {
		return err
	}

	if n.Name() != contact.Name() {
		return n.forwardDelete(contact, item, root, current)
	}

	if current == root {
		n.create(item.Collection, root)
	}

	if n.remove(item) {
		return nil
	}

	current = encoding.BASE64.Parent(current)
	if len(current) == 0 {
		n.queue.Add(NewDeleteElement(item, root, item.Location))
		return nil
	}
	return n.deleteOp(item, root, current)
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
	n.mirrorWrite(item)

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
	tomb := &domain.Item{
		Collection: item.Collection,
		Location:   item.Location,
		Id:         item.Id,
		Tombstone:  true,
		Gen:        gen,
	}
	n.markOwnedDirtyForItem(item.Collection, item.Location)
	n.mirrorWrite(tomb)
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

const peerBusyWindow = 100 * time.Millisecond

func (n *Node) markPeerBusy(name string) {
	if name == "" || name == n.Name() {
		return
	}
	n.peerBusyMu.Lock()
	defer n.peerBusyMu.Unlock()
	until := time.Now().Add(peerBusyWindow)
	if current, exist := n.peerBusy[name]; exist && current.After(until) {
		return
	}
	n.peerBusy[name] = until
}

func (n *Node) isPeerBusy(name string) bool {
	n.peerBusyMu.Lock()
	defer n.peerBusyMu.Unlock()
	until, exist := n.peerBusy[name]
	if !exist {
		return false
	}
	if time.Now().After(until) {
		delete(n.peerBusy, name)
		return false
	}
	return true
}

func (n *Node) forwardNew(contact domain.Contact, item *domain.Item, root, current string) error {
	if n.isPeerBusy(contact.Name()) {
		return domain.ErrPeerBusy
	}
	if !n.allowForward() {
		return ErrQueueFull
	}
	err := contact.New(item, root, current)
	if err != nil {
		n.markPeerBusy(contact.Name())
	}
	return err
}

func (n *Node) forwardDelete(contact domain.Contact, item *domain.Item, root, current string) error {
	if n.isPeerBusy(contact.Name()) {
		return domain.ErrPeerBusy
	}
	if !n.allowForward() {
		return ErrQueueFull
	}
	err := contact.Delete(item, root, current)
	if err != nil {
		n.markPeerBusy(contact.Name())
	}
	return err
}
