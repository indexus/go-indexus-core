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

func (n *Node) New(item *domain.Item, root, current string) error {
	return n.enqueue(item, root, current, fromClient, true)
}

func (n *Node) Handoff(item *domain.Item, root, current string) error {
	return n.enqueue(item, root, current, fromPeer, false)
}

func (n *Node) enqueue(item *domain.Item, root, current string, src ingressSource, meter bool) error {
	if err := checkKey(item, root, current); err != nil {
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

	line := fmt.Sprintf("ingress|%s|%s|%s", root, current, item.Content())
	if sa, ok := n.storage.(interface{ SyncAppend(string) error }); ok {
		if err := sa.SyncAppend(line); err != nil {
			return fmt.Errorf("ingress wal: %w", err)
		}
	} else if n.ready {
		n.storage.Append(line)
	}
	el := NewElement(item, root, current)
	el.meter = meter
	if !n.queue.TryAdd(el, ceiling) {
		return ErrQueueFull
	}
	return nil
}

func (n *Node) Delete(item *domain.Item, root, current string) error {
	if err := checkKey(item, root, current); err != nil {
		return err
	}
	if n.leaving.Load() {
		return ErrLeaving
	}
	line := fmt.Sprintf("delete|%s|%s|%s|%s|%s", root, current, item.Collection, item.Location, item.Id)
	if sa, ok := n.storage.(interface{ SyncAppend(string) error }); ok {
		if err := sa.SyncAppend(line); err != nil {
			return fmt.Errorf("delete wal: %w", err)
		}
	} else if n.ready {
		n.storage.Append(line)
	}
	if !n.queue.TryAdd(NewDeleteElement(item, root, current), n.settings.queueMax) {
		return ErrQueueFull
	}
	return nil
}

func checkKey(item *domain.Item, root, current string) error {
	switch {
	case item == nil:
		return errors.New("nil item")
	case item.Location == "":
		return errors.New("item without location")
	case root == "" || current == "":
		return errors.New("empty root or current location")
	case root != encoding.BASE64.Root() && !strings.HasPrefix(current, root):
		return fmt.Errorf("root %q is not an ancestor of %q", root, current)
	case len(item.Collection) > encoding.BASE64.IDLength(),
		len(item.Location) > encoding.BASE64.IDLength():
		return fmt.Errorf("key %s/%s is wider than an identifier", item.Collection, item.Location)
	}
	if _, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, item.Location, item.Collection); err != nil {
		return fmt.Errorf("key %s/%s: %w", item.Collection, item.Location, err)
	}
	return nil
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
		n.recordStall(element, err)
		n.queue.Add(element)
		return false
	}
	return true
}

func (n *Node) process(element *Element) error {
	if element.op == OpDelete {
		return n.deleteOp(element.item, element.root, element.current)
	}
	return n.insert(element.item, element.root, element.current, element.meter)
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
		slog.Warn("ingress element re-queued",
			"op", element.op.String(),
			"collection", element.item.Collection,
			"location", element.root,
			"pending", n.queue.Length(),
			"requeues", total,
			"err", err)
	}
}

func (n *Node) IngressStats() map[string]any {
	n.stallMu.Lock()
	defer n.stallMu.Unlock()

	reasons := make(map[string]int64, len(n.stalls))
	for reason, count := range n.stalls {
		reasons[reason] = count
	}

	return map[string]any{
		"pending":  n.queue.Length(),
		"requeues": n.requeues,
		"stalls":   reasons,
	}
}
