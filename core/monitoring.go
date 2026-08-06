package core

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// Snapshot store prefixes used by zone snaps, node manifests, and legacy latest dumps.
var snapshotPrefixes = []string{"zones/", "nodes/", "snapshots/"}

func (n *Node) Routing() ([]domain.Contact, error) {
	return n.traverseRouting(true), nil
}

func (n *Node) Acknowledged() ([]domain.Contact, error) {
	return n.traverseAcknowledged(true), nil
}

func (n *Node) Registered() ([]domain.Contact, error) {
	return n.traverseRegistered(true), nil
}

func (n *Node) Ownership() (map[string]map[string]map[string]any, error) {
	collections := make(map[string]map[string]map[string]any)

	for _, collection := range n.collections.List() {
		collections[collection.Name()] = make(map[string]map[string]any)

		collection.Browse(
			func(ownership string) {
				collections[collection.Name()][ownership] = make(map[string]any)
			},
			func(ownership, delegation string) {
				collections[collection.Name()][ownership][delegation] = nil
			},
		)
	}
	return collections, nil
}

func (n *Node) Count() (int, error) {
	pending := n.pendingInboundKeySet()
	total := 0
	prep := 0

	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				continue
			}
			count := 0
			collection.Traverse(
				key.Location,
				func(s string, a *domain.Abelian) {},
				func(s1, s2, s3 string, a *domain.Abelian) { count++ },
			)
			if keyInPendingInbound(pending, key) {
				prep += count
				continue
			}
			total += count
		}
	})
	n.items.Store(int64(total))
	n.itemsPrep.Store(int64(prep))
	return total, nil
}

func (n *Node) Items() int {
	if n.items.Load() == 0 && n.itemsPrep.Load() == 0 && n.lastCountAt.Load() == 0 {
		total, _ := n.Count()
		n.lastCountAt.Store(time.Now().UnixNano())
		return total
	}
	return int(n.items.Load())
}

// ItemsPreparing is the item count under inbound snapshot-delegation sessions
// (loaded, not yet SwitchAck'd). Official Items() excludes these so the donor
// remains the serving count until the receiver is ready.
func (n *Node) ItemsPreparing() int {
	if n.items.Load() == 0 && n.itemsPrep.Load() == 0 && n.lastCountAt.Load() == 0 {
		_, _ = n.Count()
		n.lastCountAt.Store(time.Now().UnixNano())
	}
	return int(n.itemsPrep.Load())
}

func (n *Node) MeasureItems() {
	if _, err := n.Count(); err != nil {
		return
	}
	n.lastCountAt.Store(time.Now().UnixNano())
}

func (n *Node) Check() []string {
	result := make([]string, 0)

	for _, collection := range n.collections.List() {
		ownerships := make([]string, 0)
		collection.Browse(
			func(ownership string) {
				ownerships = append(ownerships, ownership)
			},
			func(ownership, delegation string) {},
		)

		for _, ownership := range ownerships {
			id, err := encoding.MergeEncodings(
				encoding.BASE64,
				encoding.BASE64,
				ownership,
				collection.Name(),
			)
			if err != nil {
				continue
			}

			_, ok := n.owned.Get(0, id)
			if !ok {
				result = append(result, fmt.Sprintf("%s:%s", collection.Name(), ownership))
			}
		}
	}

	owned := make([]domain.Key, 0)
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			owned = append(owned, key)
		}
	})

	for _, key := range owned {
		collection, ok := n.collections.Get(key.Collection)
		if !ok {
			result = append(result, fmt.Sprintf("%s:%s", key.Collection, key.Location))
			continue
		}

		_, ok = collection.Get(key.Location)
		if !ok {
			result = append(result, fmt.Sprintf("%s:%s", key.Collection, key.Location))
		}
	}
	return result
}

func (n *Node) Queue() int {
	return n.queue.Length()
}

func (n *Node) SnapshotInfo() map[string]any {
	info := map[string]any{
		"store":              n.Store() != nil,
		"delegation":         n.Delegation(), // S3 / DirStore snapshot path enabled
		"delegation_size":    0,               // -delegation / INDEXUS_DELEGATION
		"transfer_threshold": TransferThreshold(),
		"delegation_timeout": DelegationTimeout().String(),
		"transfer_timeout":   ClassicTransferTimeout().String(),
		"deleg_in":           n.pendingInbound(),
		"deleg_out":          0,
		"dirty":              0,
		"snapped":            0,
		"wal_segments":       0,
	}
	if n == nil {
		return info
	}
	if n.settings != nil {
		info["delegation_size"] = n.settings.DelegationSize()
	}
	n.delegMu.Lock()
	out := 0
	for _, s := range n.delegOut {
		if s.State != delegSwitched && s.State != delegCancelled {
			out++
		}
	}
	n.delegMu.Unlock()
	info["deleg_out"] = out
	if n.zoneSnap != nil {
		n.zoneSnap.mu.Lock()
		info["dirty"] = len(n.zoneSnap.dirty)
		info["snapped"] = len(n.zoneSnap.seq)
		info["wal_segments"] = len(n.zoneSnap.walSegs)
		n.zoneSnap.mu.Unlock()
	}
	return info
}

// ListSnapshots returns object-store keys under zones/, nodes/, and snapshots/.
func (n *Node) ListSnapshots(ctx context.Context) (map[string]any, error) {
	store := n.Store()
	if store == nil {
		return map[string]any{"available": false, "objects": []ObjectInfo{}}, nil
	}
	seen := make(map[string]struct{})
	objects := make([]ObjectInfo, 0)
	for _, prefix := range snapshotPrefixes {
		listed, err := store.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		for _, o := range listed {
			if _, ok := seen[o.Key]; ok {
				continue
			}
			seen[o.Key] = struct{}{}
			objects = append(objects, o)
		}
	}
	sort.Slice(objects, func(i, j int) bool {
		if objects[i].LastModified.Equal(objects[j].LastModified) {
			return objects[i].Key > objects[j].Key
		}
		return objects[i].LastModified.After(objects[j].LastModified)
	})
	return map[string]any{
		"available": true,
		"objects":   objects,
		"count":     len(objects),
	}, nil
}

// ClearSnapshots deletes object-store keys under zones/, nodes/, and snapshots/.
func (n *Node) ClearSnapshots(ctx context.Context) (map[string]any, error) {
	store := n.Store()
	if store == nil {
		return map[string]any{"available": false, "cleared": 0}, nil
	}
	cleared := 0
	for _, prefix := range snapshotPrefixes {
		listed, err := store.List(ctx, prefix)
		if err != nil {
			return nil, err
		}
		if err := store.DeletePrefix(ctx, prefix); err != nil {
			return nil, err
		}
		cleared += len(listed)
	}
	return map[string]any{
		"available": true,
		"cleared":   cleared,
	}, nil
}
