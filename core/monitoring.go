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

// Ownership returns the last cached ownership map. It never blocks on
// Collection.mu: a TryRLock refresh is attempted, and on contention the
// previous snapshot is served (empty if none yet).
func (n *Node) Ownership() (map[string]map[string]map[string]any, error) {
	n.tryRefreshOwnershipSnap()
	if v := n.ownershipSnap.Load(); v != nil {
		return v.(map[string]map[string]map[string]any), nil
	}
	return map[string]map[string]map[string]any{}, nil
}

// tryRefreshOwnershipSnap copies Collection.owned under TryRLock per collection.
// Returns false if any collection was write-locked (keeps the previous snap).
func (n *Node) tryRefreshOwnershipSnap() bool {
	out := make(map[string]map[string]map[string]any)
	for _, collection := range n.collections.List() {
		snap, ok := collection.TryOwnershipBrowse()
		if !ok {
			return false
		}
		out[collection.Name()] = snap
	}
	n.ownershipSnap.Store(out)
	return true
}

// OwnershipStats returns collection/zone counts from the XOR owned index.
// Used by /status so monitoring never takes Collection.mu (Browse/Traverse)
// and cannot stall the data-plane protocol under load.
func (n *Node) OwnershipStats() (collections, zones int) {
	seen := make(map[string]struct{})
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(_ int, _ []byte, keys map[domain.Key]any) {
		for key := range keys {
			zones++
			seen[key.Collection] = struct{}{}
		}
	})
	return len(seen), zones
}

// Count walks owned zones under Collection RLock and updates the Items atomics
// plus the ownership snapshot. Prefer Items()/Ownership() on request paths —
// this is for MeasureItems / explicit exact recount only.
func (n *Node) Count() (int, error) {
	total := 0

	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				continue
			}
			count := collection.ItemCount(key.Location)
			total += count
		}
	})
	n.items.Store(int64(total))
	_ = n.tryRefreshOwnershipSnap()
	return total, nil
}

// Items returns the last background MeasureItems/Count total. Never walks the
// collection (that is /count or MeasureItems) so monitoring cannot take
// Collection.mu on the request path.
func (n *Node) Items() int {
	return int(n.items.Load())
}

// ItemsPreparing is retained for monitoring compatibility. Repeatable
// handoffs have no mounted pre-ownership session, so it is always zero.
func (n *Node) ItemsPreparing() int {
	return 0
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
			func(_, _ string, _ domain.Handoff) {},
		)

		for _, ownership := range ownerships {
			id, err := zoneKeyID(collection.Name(), ownership)
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
		"store":           n.Store() != nil,
		"delegation":      false, // legacy status key; five-state S3 path removed
		"delegation_size": 0,     // -delegation / INDEXUS_DELEGATION
		"deleg_in":        0,
		"deleg_out":       0,
		"dirty":           0,
		"snapped":         0,
		"wal_segments":    0,
	}
	if n == nil {
		return info
	}
	if n.settings != nil {
		info["delegation_size"] = n.settings.DelegationSize()
	}
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
