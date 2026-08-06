package core

import (
	"log/slog"
	"os"
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

// DefaultJoinPublishGrace is how long a spawned node may wait for the ingress
// queue to settle after first ownership before publishing client routing.
// Override with INDEXUS_JOIN_PUBLISH_GRACE (Go duration). Does not bypass an
// incomplete inbound snapshot-delegation session.
const DefaultJoinPublishGrace = 5 * time.Second

func joinPublishGrace() time.Duration {
	if raw := os.Getenv("INDEXUS_JOIN_PUBLISH_GRACE"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			return d
		}
	}
	return DefaultJoinPublishGrace
}

func (n *Node) tryPublishClientReady() {
	if n == nil || n.clientPublished.Load() {
		return
	}
	if n.autoscale == nil || n.autoscale.cfg.Role != "spawned" {
		n.clientPublished.Store(true)
		return
	}
	// Snapshot handoff still in flight: data may be loaded but ownership is
	// not official yet. Never advertise client_ready until SwitchAck clears
	// inbound sessions (donor still serves those zones).
	if n.pendingInbound() > 0 {
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
	aged := time.Since(time.Unix(0, first)) >= joinPublishGrace()

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
