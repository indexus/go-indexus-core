package core

import (
	"fmt"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

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
	total := 0

	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				continue
			}
			collection.Traverse(
				key.Location,
				func(s string, a *domain.Abelian) {},
				func(s1, s2, s3 string, a *domain.Abelian) { total++ },
			)
		}
	})
	n.items.Store(int64(total))
	return total, nil
}

func (n *Node) Items() int {
	if n.items.Load() == 0 && n.lastCountAt.Load() == 0 {
		total, _ := n.Count()
		n.lastCountAt.Store(time.Now().UnixNano())
		return total
	}
	return int(n.items.Load())
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
		"store":        n.Store() != nil,
		"delegation":   n.Delegation(),
		"deleg_in":     n.pendingInbound(),
		"deleg_out":    0,
		"dirty":        0,
		"snapped":      0,
		"wal_segments": 0,
	}
	if n == nil {
		return info
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
