package core

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
)

// parkedKey names a garage slot: elements waiting for a delegated child to
// become placeable again. Elements here are WAL-durable (logged before ACK)
// but invisible to aggregates — Shrink must never see them.
type parkedKey struct {
	Collection string
	Child      string
}

func (n *Node) park(element *Element, err error) bool {
	var blocked domain.ErrDelegatedTo
	if !errors.As(err, &blocked) {
		return false
	}
	if element == nil || element.item == nil {
		return false
	}
	key := parkedKey{Collection: element.item.Collection, Child: blocked.Child}
	element.via = nil

	n.parkMu.Lock()
	if n.parked == nil {
		n.parked = map[parkedKey][]*Element{}
	}
	n.parked[key] = append(n.parked[key], element)
	depth := len(n.parked[key])
	n.parkMu.Unlock()

	slog.Info("parked write under delegated child",
		"collection", key.Collection,
		"child", key.Child,
		"peer", blocked.Peer,
		"location", element.item.Location,
		"parked", depth)
	return true
}

// wakeParked moves every element garaged under collection/child back onto the
// hot queue with an empty via, so placement restarts from the address.
func (n *Node) wakeParked(collection, child string) int {
	key := parkedKey{Collection: collection, Child: child}
	n.parkMu.Lock()
	batch := n.parked[key]
	delete(n.parked, key)
	n.parkMu.Unlock()
	for _, el := range batch {
		if el == nil {
			continue
		}
		el.via = nil
		n.queue.Add(el)
	}
	if len(batch) > 0 {
		slog.Info("woke parked writes",
			"collection", collection, "child", child, "count", len(batch))
	}
	return len(batch)
}

func (n *Node) parkedCount() int {
	total, _ := n.parkedCounts()
	return total
}

func (n *Node) parkedCounts() (int, map[string]int) {
	n.parkMu.Lock()
	defer n.parkMu.Unlock()
	total := 0
	byCollection := make(map[string]int)
	for key, batch := range n.parked {
		total += len(batch)
		byCollection[key.Collection] += len(batch)
	}
	return total, byCollection
}

// snapshotParked serializes the garage separately from the hot queue. via is
// deliberately absent: a restored parked write must retry from item.Location.
func (n *Node) snapshotParked() []string {
	n.parkMu.Lock()
	defer n.parkMu.Unlock()

	var lines []string
	for key, batch := range n.parked {
		for _, el := range batch {
			if el == nil || el.item == nil {
				continue
			}
			kind := "parked-ingress"
			if el.op == OpDelete {
				kind = "parked-delete"
			}
			lines = append(lines, fmt.Sprintf("%s|%s|%s|%s",
				kind, key.Child, el.root, el.item.Content()))
		}
	}
	return lines
}

func (n *Node) restoreParked(command string, del bool) {
	arr := strings.SplitN(command, "|", 4)
	if len(arr) != 4 || arr[1] == "" || arr[2] == "" {
		return
	}
	item, ok := domain.ParseContent(arr[3])
	if !ok {
		return
	}
	var el *Element
	if del {
		el = NewDeleteElement(item, arr[2])
	} else {
		el = NewElement(item, arr[2])
	}
	el.via = nil
	key := parkedKey{Collection: item.Collection, Child: arr[1]}
	n.parkMu.Lock()
	n.parked[key] = append(n.parked[key], el)
	n.parkMu.Unlock()
}

// reclaimParkedChild is called when membership declares the named holder of a
// mark dead/quarantined: Own the child locally and requeue the garage.
func (n *Node) reclaimParkedChild(collection *domain.Collection, child string) {
	if collection == nil || child == "" {
		return
	}
	collection.Own(child, domain.Delegation{})
	collection.EnsureSet(child)
	id, err := zoneKeyID(collection.Name(), child)
	if err == nil {
		n.owned.Upsert(0, id, map[domain.Key]any{}, func(i int, b []byte, m map[domain.Key]any) {
			m[domain.Key{Collection: collection.Name(), Location: child}] = nil
		})
	}
	// The parent mark remains as the structural edge to the now-local child.
	// Add selects the child's own ownership before consulting that parent edge.
	n.wakeParked(collection.Name(), child)
}
