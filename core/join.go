package core

import (
	"log/slog"
	"time"
)

func (n *Node) ClientReady() bool {
	if n == nil || !n.ready || n.leaving.Load() {
		return false
	}
	if n.autoscale == nil || n.autoscale.cfg.Role != "spawned" {
		return true
	}
	if n.clientPublished.Load() {
		return true
	}
	n.tryPublishClientReady()
	return n.clientPublished.Load()
}

const joinPublishGrace = 5 * time.Second

func (n *Node) tryPublishClientReady() {
	if n == nil || n.clientPublished.Load() {
		return
	}
	if n.autoscale == nil || n.autoscale.cfg.Role != "spawned" {
		n.clientPublished.Store(true)
		return
	}
	zones := n.ownedZoneCount()
	if zones == 0 {
		return
	}
	first := n.joinFirstOwn.Load()
	if first == 0 {
		n.joinFirstOwn.CompareAndSwap(0, time.Now().UnixNano())
		first = n.joinFirstOwn.Load()
	}
	q := n.Queue()
	ceiling := n.settings.peerQueueMax
	if ceiling <= 0 {
		ceiling = n.settings.queueMax
	}

	settled := q < 64 || (ceiling > 0 && q <= ceiling/8)
	aged := time.Since(time.Unix(0, first)) >= joinPublishGrace

	if n.pendingInbound() > 0 && !aged {
		return
	}
	if !settled && !aged {
		return
	}
	if n.clientPublished.CompareAndSwap(false, true) {
		slog.Info("client routing published",
			"owned_zones", zones,
			"queue", q,
			"settled", settled,
			"grace", aged)
	}
}
