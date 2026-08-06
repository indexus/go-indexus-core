package core

import (
	"errors"
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

// TransferBusy is true while ownership is moving (rebalance flag or open
// inbound/outbound delegation sessions). Feed/ingress pause on this; autoscale
// may still spawn but must exclude outbound zones from PreferNear / item load.
func (n *Node) TransferBusy() bool {
	if n == nil {
		return false
	}
	if n.rebalancing.Load() {
		return true
	}
	return n.pendingOutbound() > 0 || n.pendingInbound() > 0
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
		// S3 offers finish asynchronously (CaughtUp). Do NOT hold rebalancing for
		// the whole mirror — that stalls Feed and balloons the ingress queue while
		// CaughtUp is still in flight. Flip freezes Feed only inside CaughtUp.
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
		if err := candidate.Transfer(n, transferKey, items); err != nil {
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

func (n *Node) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {
	if n.leaving.Load() {
		return domain.ErrLeaving
	}
	for _, item := range items {

		if item == nil {
			return errors.New("transfer carries an empty item")
		}
		if item.Tombstone {
			n.create(item.Collection, key.Location)
			if c, ok := n.collections.Get(item.Collection); ok {
				c.ApplyTombstone(item.Location, item.Id, item.Gen)
				if n.ready {
					n.storage.Append(item.Content())
				}
			}
			continue
		}
		if err := n.enqueue(item, key.Location, key.Location, fromPeer, false); err != nil {
			return err
		}
	}

	n.tryPublishClientReady()
	return nil
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
