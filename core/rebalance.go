package core

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Rebalancing() bool {
	return n != nil && n.rebalancing.Load()
}

// TransferBusy is true while the serialized repeatable handoff round is moving
// ownership. There are no asynchronous delegation sessions.
func (n *Node) TransferBusy() bool {
	if n == nil {
		return false
	}
	return n.rebalancing.Load()
}

func (n *Node) Refresh() error {

	if !n.refreshBusy.CompareAndSwap(false, true) {
		return nil
	}
	defer n.refreshBusy.Store(false)

	toRegister := make([]domain.Contact, 0)

	peers := n.traverseRouting(false)
	if len(peers) == 0 {
		peers = n.traverseRegistered(false)
	}
	for _, contact := range peers {
		contacts, err := contact.Neighbors(n)
		if err != nil {
			continue
		}
		toRegister = append(toRegister, contacts...)
	}

	n.register(toRegister)

	if !n.leaving.Load() && n.transferMu.TryLock() {
		plan := n.control()
		if len(plan) > 0 {
			n.rebalancing.Store(true)
		}
		var wg sync.WaitGroup
		var transferred atomic.Int64
		for candidate, keys := range plan {
			candidate, keys := candidate, keys
			wg.Add(1)
			go func() {
				defer wg.Done()
				if handed := n.offerZones(candidate, keys); handed > 0 {
					transferred.Add(int64(handed))
					if n.autoscale != nil {
						n.autoscale.MarkReliefArrived()
					}
				}
			}()
		}
		wg.Wait()
		n.rebalancing.Store(false)
		n.transferMu.Unlock()
		if total := int(transferred.Load()); total > 0 {

			releaseHeapAfterTransfer(total)
			n.MeasureItems()
		}
	}

	if err := n.clean(); err != nil {
		return err
	}
	if err := n.Checkpoint(); err != nil {
		return err
	}

	return nil
}

func (n *Node) control() map[domain.Contact][]domain.Key {
	plan := make(map[domain.Contact][]domain.Key)
	claimed := make(map[domain.Key]struct{})

	for _, candidate := range n.traverseRouting(false) {
		if n.suspended(candidate.Name()) {
			continue
		}

		keys := make([]domain.Key, 0)
		n.owned.Range(0, n.ID(), candidate.ID(), encoding.BASE64.NewID(), func(idx int, id []byte, sets map[domain.Key]any) {
			for key := range sets {
				if _, ok := claimed[key]; ok {
					continue
				}
				claimed[key] = struct{}{}
				keys = append(keys, key)
			}
		})
		if len(keys) > 0 {
			plan[candidate] = keys
		}
	}

	return plan
}

// offerZones is one repeatable handoff round. Each zone is copied in one
// request, the HTTP response is the receiver's nominative ACK, and only that
// ACKed zone is dropped. A refusal restores it locally; untouched keys remain
// residue for the next Refresh tick.
func (n *Node) offerZones(peer domain.Contact, keys []domain.Key) int {
	return n.transferToPeer(peer, keys)
}

func (n *Node) transferToPeer(candidate domain.Contact, keys []domain.Key) int {
	keySet := make(map[domain.Key]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}

	handed := 0
	refused := false
	for _, key := range keys {

		if refused || n.leaving.Load() {
			continue
		}

		transferKey := key
		if collection, ok := n.collections.Get(key.Collection); ok {
			parent := key.Location
			for {
				k := domain.Key{
					Collection: key.Collection,
					Location:   parent,
				}
				if _, ok := keySet[k]; !ok {
					break
				}
				if parent == collection.Base().Root() {
					break
				}
				transferKey.Location, parent = parent, collection.Base().Parent(parent)
			}
		}

		collection, ok := n.collections.Get(key.Collection)
		if !ok {
			n.removeOwnedKey(key)
			continue
		}

		items, empty := collection.Delegate(key.Location)
		ackedPeer, err := candidate.Transfer(n, transferKey, items)
		if err != nil {
			refused = true
			slog.Warn("rebalance transfer failed, keeping items local",
				"peer", candidate.Name(),
				"collection", key.Collection,
				"location", key.Location,
				"items", len(items),
				"err", err)
			n.restoreDelegated(key, items)
			continue
		}

		// Nominative mark: the ACK names the final receiver, so parked writes and
		// repair know who holds the child. Anonymous marks never block progress.
		if parent := collection.Base().Parent(key.Location); parent != "" {
			collection.MarkDelegatedTo(parent, key.Location, ackedPeer)
		}
		n.wakeParked(key.Collection, key.Location)

		n.removeOwnedKey(key)
		if empty {
			n.collections.Delete(key.Collection)
		}

		forget := key.Location
		if forget == collection.Base().Root() {
			forget = ""
		}
		n.cache.ForgetUnder(key.Collection, forget)
		count := len(items)
		handed += count
		dropTransferredRefs(items)
		if count >= 64 {
			releaseHeapAfterTransfer(count)
		}
		// Parent abelian still holds the pre-handoff child entry — pull the
		// receiver's summary so parent blocks start converging immediately.
		n.pullDelegatedAggregate(key.Collection, key.Location, candidate)
	}
	return handed
}

func (n *Node) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	if n.leaving.Load() {
		return "", domain.ErrLeaving
	}
	for _, item := range items {
		if item == nil {
			return "", errors.New("transfer carries an empty item")
		}
		if item.Collection != key.Collection {
			return "", fmt.Errorf("transfer item collection %q does not match key %q", item.Collection, key.Collection)
		}
	}
	holder, err := n.find(key.Collection, key.Location)
	if err != nil {
		return "", err
	}
	if holder.Name() != n.Name() {
		if origin != nil && holder.Name() == origin.Name() {
			return "", domain.ErrOwnerUnavailable
		}
		return holder.Transfer(n, key, items)
	}

	// The payload is now fully copied and validated. Install ownership only on
	// this final step, immediately before applying the round's items.
	n.create(key.Collection, key.Location)
	for _, item := range items {
		if item.Tombstone {
			if c, ok := n.collections.Get(item.Collection); ok {
				c.ApplyTombstone(item.Location, item.Id, item.Gen)
				if n.ready {
					n.storage.Append(item.Content())
				}
			}
			continue
		}
		// The pre-flight above accepted this whole zone on this node. Applying
		// its leaves through insert would restart routing at each leaf address;
		// leaves can have a different XOR-nearest peer than the zone root and
		// would recreate ownership on the donor while the handoff was in flight.
		// Install directly into the accepted zone instead.
		if n.add(item, false) {
			continue
		}
		if child, handoff, ok := n.delegatedCover(item.Collection, item.Location); ok {
			blocked := domain.ErrDelegatedTo{Child: child, Peer: handoff.Peer}
			if n.park(NewElement(item, key.Location), blocked) {
				continue
			}
		}
		return "", fmt.Errorf("transfer item %s/%s is outside accepted zone %s", item.Collection, item.Location, key.Location)
	}

	n.MeasureItems()
	n.tryPublishClientReady()
	return n.Name(), nil
}

func (n *Node) restoreDelegated(key domain.Key, items []*domain.Item) {
	n.create(key.Collection, key.Location)
	for _, item := range items {
		if item == nil {
			continue
		}
		if item.Tombstone {
			if c, ok := n.collections.Get(key.Collection); ok {
				c.ApplyTombstone(item.Location, item.Id, item.Gen)
				if n.ready {
					n.storage.Append(item.Content())
				}
			}
			continue
		}
		_ = n.add(item, false)
	}
}

var lastHeapRelease atomic.Int64

func releaseHeapAfterTransfer(itemCount int) {
	if itemCount <= 0 {
		return
	}
	now := time.Now().UnixNano()
	prev := lastHeapRelease.Load()

	minGap := int64(5 * time.Second)
	if itemCount < 256 {
		minGap = int64(15 * time.Second)
	}
	if prev != 0 && now-prev < minGap {
		return
	}
	if !lastHeapRelease.CompareAndSwap(prev, now) {
		return
	}
	runtime.GC()
	debug.FreeOSMemory()
}

func dropTransferredRefs(items []*domain.Item) {
	for i := range items {
		items[i] = nil
	}
}
