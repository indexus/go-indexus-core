package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

var ErrQueueFull = errors.New("ingress queue full")

var ErrNodeFull = errors.New("node out of memory headroom")

var ErrJoining = errors.New("node still joining ownership")

var ErrLeaving = domain.ErrLeaving

type ingressSource int

const (
	fromClient ingressSource = iota
	fromPeer
)

func (n *Node) New(item *domain.Item, root string, via domain.Visited) error {
	// Client writes always start a fresh attempt; ignore any via a sticky
	// client might echo.
	return n.enqueue(item, root, nil, fromClient, true)
}

func (n *Node) Handoff(item *domain.Item, root string, via domain.Visited) error {
	return n.enqueue(item, root, via, fromPeer, false)
}

func (n *Node) enqueue(item *domain.Item, root string, via domain.Visited, src ingressSource, meter bool) error {
	if err := checkKey(item, root); err != nil {
		return err
	}
	if n.leaving.Load() {
		return ErrLeaving
	}

	if src == fromClient && !n.ClientReady() {
		return ErrJoining
	}
	if src == fromClient && (n.full.Load() || n.autoscale.ClientWritesBlocked()) {
		return ErrNodeFull
	}
	ceiling := n.settings.queueMax
	if src == fromPeer {
		ceiling = n.settings.peerQueueMax
	}

	if err := n.logIngress(item, root); err != nil {
		return err
	}
	el := NewElement(item, root)
	el.via = via
	el.meter = meter
	if !n.queue.TryAdd(el, ceiling) {
		return ErrQueueFull
	}
	return nil
}

// logIngress records an incoming write before anything acts on it. The middle
// field is kept for WAL format compatibility (legacy "current") and is ignored
// on restore; placement always starts at the address.
func (n *Node) logIngress(item *domain.Item, root string) error {
	line := fmt.Sprintf("ingress|%s|%s|%s", root, item.Location, item.Content())
	if sa, ok := n.storage.(interface{ SyncAppend(string) error }); ok {
		if err := sa.SyncAppend(line); err != nil {
			return fmt.Errorf("ingress wal: %w", err)
		}
		return nil
	}
	if n.ready {
		n.storage.Append(line)
	}
	return nil
}

func (n *Node) Delete(item *domain.Item, root string, via domain.Visited) error {
	if err := checkKey(item, root); err != nil {
		return err
	}
	if n.leaving.Load() {
		return ErrLeaving
	}
	line := fmt.Sprintf("delete|%s|%s|%s|%s|%s", root, item.Location, item.Collection, item.Location, item.Id)
	if sa, ok := n.storage.(interface{ SyncAppend(string) error }); ok {
		if err := sa.SyncAppend(line); err != nil {
			return fmt.Errorf("delete wal: %w", err)
		}
	} else if n.ready {
		n.storage.Append(line)
	}
	element := NewDeleteElement(item, root)
	element.via = via
	ceiling := n.settings.queueMax
	if len(via) > 0 {
		ceiling = n.settings.peerQueueMax
	}
	if !n.queue.TryAdd(element, ceiling) {
		return ErrQueueFull
	}
	return nil
}

func checkKey(item *domain.Item, root string) error {
	switch {
	case item == nil:
		return errors.New("nil item")
	case item.Location == "":
		return errors.New("item without location")
	case root == "":
		return errors.New("empty root")
	case root != encoding.BASE64.Root() && !strings.HasPrefix(item.Location, root):
		return fmt.Errorf("root %q is not an ancestor of %q", root, item.Location)
	case len(item.Collection) > encoding.BASE64.IDLength(),
		len(item.Location) > encoding.BASE64.IDLength():
		return fmt.Errorf("key %s/%s is wider than an identifier", item.Collection, item.Location)
	}
	_, err := zoneKeyID(item.Collection, item.Location)
	return err
}

func (n *Node) GuardMemory(ctx context.Context) {
	limit := n.settings.memRefusePct
	if limit <= 0 {
		return
	}

	const every = time.Second

	for {
		used := memoryUsedPct()
		if used > 0 {

			if n.full.Load() {
				n.full.Store(used > limit-5)
			} else if used >= limit {
				slog.Warn("refusing writes, out of memory headroom",
					"mem_pct", used, "limit_pct", limit, "queue", n.queue.Length())
				n.full.Store(true)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

func (n *Node) Feed() error {
	workers := n.settings.feedWorkers
	if workers < 1 {
		workers = 1
	}

	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			n.feedLoop()
		}()
	}
	group.Wait()

	return nil
}

func (n *Node) feedLoop() {
	failures := 0
	for {
		element, exist := n.queue.Consume()
		if !exist {
			continue
		}
		if n.feedOnce(element) {
			failures = 0
			continue
		}

		failures++
		time.Sleep(feedBackoff(failures))
	}
}

func feedBackoff(failures int) time.Duration {
	backoff := time.Duration(failures) * 2 * time.Millisecond
	if backoff > 100*time.Millisecond {
		return 100 * time.Millisecond
	}
	return backoff
}

func (n *Node) feedOnce(element *Element) bool {
	if n.leaving.Load() || n.rebalancing.Load() {
		n.queue.Add(element)
		return false
	}

	if err := n.process(element); err != nil {
		if n.park(element, err) {
			return false
		}
		n.recordStall(element, err)
		// A refusal is local to this attempt. Clear via so the retry is
		// identical to a fresh write.
		element.via = nil
		n.queue.Add(element)
		return false
	}
	return true
}

func (n *Node) process(element *Element) error {
	if element.op == OpDelete {
		return n.deleteOp(element.item, element.root, element.via)
	}
	return n.insert(element.item, element.root, element.via, element.meter)
}

const stallLogEvery = 5 * time.Second

func (n *Node) recordStall(element *Element, err error) {
	reason := err.Error()

	n.stallMu.Lock()
	n.requeues++
	n.stalls[reason]++
	total := n.requeues
	report := time.Since(n.stallLoggedAt) >= stallLogEvery
	if report {
		n.stallLoggedAt = time.Now()
	}
	n.stallMu.Unlock()

	if report {
		loc := ""
		id := ""
		if element.item != nil {
			loc = element.item.Location
			id = element.item.Id
		}
		slog.Warn("ingress element re-queued",
			"op", element.op.String(),
			"collection", element.item.Collection,
			"root", element.root,
			"via", element.via.String(),
			"location", loc,
			"id", id,
			"pending", n.queue.Length(),
			"requeues", total,
			"err", err)
	}
}

func (n *Node) IngressStats() map[string]any {
	n.stallMu.Lock()
	reasons := make(map[string]int64, len(n.stalls))
	for reason, count := range n.stalls {
		reasons[reason] = count
	}
	requeues := n.requeues
	n.stallMu.Unlock()

	const sampleCap = 16
	pending := n.queue.Snapshot()
	samples := make([]map[string]any, 0, sampleCap)
	for _, el := range pending {
		if len(samples) >= sampleCap {
			break
		}
		if el == nil || el.item == nil {
			continue
		}
		samples = append(samples, map[string]any{
			"op":         el.op.String(),
			"collection": el.item.Collection,
			"id":         el.item.Id,
			"location":   el.item.Location,
			"root":       el.root,
			"via":        el.via.String(),
			"parked":     false,
		})
	}
	n.parkMu.Lock()
	parkedByCollection := make(map[string]int)
	for key, batch := range n.parked {
		parkedByCollection[key.Collection] += len(batch)
		for _, el := range batch {
			if len(samples) >= sampleCap {
				break
			}
			if el == nil || el.item == nil {
				continue
			}
			samples = append(samples, map[string]any{
				"op":         el.op.String(),
				"collection": el.item.Collection,
				"id":         el.item.Id,
				"location":   el.item.Location,
				"root":       el.root,
				"via":        el.via.String(),
				"parked":     true,
				"child":      key.Child,
			})
		}
	}
	parked := 0
	for _, batch := range n.parked {
		parked += len(batch)
	}
	n.parkMu.Unlock()

	return map[string]any{
		"pending":              len(pending),
		"requeues":             requeues,
		"parked":               parked,
		"parked_by_collection": parkedByCollection,
		"stalls":               reasons,
		"samples":              samples,
	}
}
