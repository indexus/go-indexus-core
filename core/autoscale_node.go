package core

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// EnableAutoscale wires the controller to this node: ownership is what it reads
// pressure from, and what it partitions to place a spawned neighbour.
func (n *Node) EnableAutoscale(cfg AutoscaleConfig) {
	n.autoscale = NewAutoscaleController(cfg)
	n.autoscale.SetNearBuilder(n.preferNearsForLoadSplit)
}

// preferNearsForLoadSplit: PreferNearSplit on residual exclusive leaves (~½).
// No exclusive load → mild PreferNear near self.
func (n *Node) preferNearsForLoadSplit(spawnN int, fallback string) []string {
	if spawnN < 1 {
		spawnN = 1
	}
	keys := n.listOwnedKeys()
	known := n.knownPeerIDs()
	selfID := n.ID()

	exclusive := make([]weightedZone, 0, len(keys))
	claimed := make([][]byte, 0)
	skippedClaimed := 0
	for _, key := range keys {
		ownID, err := zoneKeyID(key.Collection, key.Location)
		if err != nil || len(ownID) == 0 {
			continue
		}
		if closestKnownPeer(ownID, selfID, known) >= 0 {
			skippedClaimed++
			claimed = append(claimed, ownID)
			continue
		}
		w := n.zoneItemCount(key)
		if w < 1 {
			w = 1
		}
		exclusive = append(exclusive, weightedZone{key: key, ownID: ownID, weight: w})
	}
	if len(exclusive) == 0 {
		nears, _ := encoding.BASE64.PreferNearTargets(fallback, spawnN)
		return nears
	}

	leaves := residualExclusiveZones(exclusive)
	if len(leaves) == 0 {
		leaves = exclusive
	}

	ownIDs := make([][]byte, len(leaves))
	weights := make([]int, len(leaves))
	total := 0
	for i, z := range leaves {
		ownIDs[i] = z.ownID
		weights[i] = z.weight
		total += z.weight
	}

	name, cand, err := encoding.BASE64.PreferNearSplit(selfID, ownIDs, weights, claimed, known)
	if err != nil || name == "" {
		slog.Info("load-split skipped: no PreferNearSplit",
			"leaf_zones", len(leaves), "leaf_items", total, "owned_items", n.Items(),
			"skipped_claimed", skippedClaimed, "err", err)
		return nil
	}

	slog.Info("load-split prefer_near",
		"prefer_near", name,
		"fork_bit", cand.ForkBit,
		"donate_items", cand.Donate,
		"keep_items", cand.Keep,
		"leaf_items", total,
		"owned_items", n.Items(),
		"claimed_zones", skippedClaimed,
	)
	return []string{name}
}

type weightedZone struct {
	key    domain.Key
	ownID  []byte
	weight int
}

// residualExclusiveZones keeps every exclusive zone with a non-overlapping
// item weight: parent Traverse includes children, so weight = parent − Σ children
// under the same collection. Pure parents (residual 0) are dropped; leaves and
// parents with leftover items both participate in the XOR split.
func residualExclusiveZones(zones []weightedZone) []weightedZone {
	out := make([]weightedZone, 0, len(zones))
	for _, z := range zones {
		loc := z.key.Location
		root := loc == "" || loc == "@" || loc == encoding.BASE64.Root()
		childSum := 0
		for _, o := range zones {
			if o.key.Collection != z.key.Collection || o.key.Location == loc {
				continue
			}
			ol := o.key.Location
			if root || (loc != "" && strings.HasPrefix(ol, loc) && len(ol) > len(loc)) {
				childSum += o.weight
			}
		}
		residual := z.weight - childSum
		if residual < 1 {
			continue
		}
		zz := z
		zz.weight = residual
		out = append(out, zz)
	}
	return out
}

func (n *Node) knownPeerNames() []string {
	peers := n.traverseRegistered(false)
	out := make([]string, 0, len(peers)+1)
	seen := map[string]struct{}{}
	if name := n.Name(); name != "" {
		seen[name] = struct{}{}
		out = append(out, name)
	}
	for _, c := range peers {
		name := c.Name()
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (n *Node) knownPeerIDs() [][]byte {
	peers := n.traverseRegistered(false)
	out := make([][]byte, 0, len(peers))
	seen := map[string]struct{}{n.Name(): {}}
	for _, c := range peers {
		name := c.Name()
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		id := c.ID()
		if len(id) == 0 {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, id)
	}
	return out
}

// closestKnownPeer mirrors encoding.closestPeer: index of XOR-closest known
// peer that beats owner, or -1 if owner remains closest.
func closestKnownPeer(key, owner []byte, peers [][]byte) int {
	best := -1
	bestD := xorDistance(key, owner)
	for i, p := range peers {
		d := xorDistance(key, p)
		if bytes.Compare(d, bestD) < 0 {
			bestD = d
			best = i
		}
	}
	return best
}

func (n *Node) zoneItemCount(key domain.Key) int {
	collection, ok := n.collections.Get(key.Collection)
	if !ok {
		return 0
	}
	return collection.ItemCount(key.Location)
}

func (n *Node) AutoscaleSnapshot() map[string]any {
	if n.autoscale == nil {
		return map[string]any{"enabled": false}
	}
	return n.autoscale.Snapshot()
}

// retainedOwnedMetrics reports the current stable ownership snapshot. Handoff
// rounds are synchronous under transferMu, so there is no pending-outbound set.
func (n *Node) retainedOwnedMetrics() (zones, items int) {
	return n.ownedZoneCount(), n.Items()
}

func (n *Node) AutoscaleTick() {
	if n.autoscale == nil {
		return
	}
	cfg := n.autoscale.cfg
	diskPath := os.Getenv("INDEXUS_STORAGE")
	if diskPath == "" {
		diskPath = "."
	}
	zones, items := n.retainedOwnedMetrics()
	in := PressureInput{
		Queue:      n.Queue(),
		OwnedZones: zones,
		OwnedItems: items,
		Resources:  SampleResources(diskPath),
		SelfName:   n.Name(),
	}
	n.autoscale.Tick(in,
		func(req ScaleUpRequest) error {
			inserts := n.autoscale.window.Sum()
			spawnN := req.SpawnCount
			if spawnN < 1 {
				spawnN = 1
			}
			slog.Info("under pressure, asking for peers",
				"reason", req.Reason,
				"spawn_count", spawnN,
				"prefer_near", req.PreferNear,
				"prefer_nears", req.PreferNears,
				"queue", in.Queue,
				"inserts_window", inserts,
				"owned_items_retained", in.OwnedItems,
				"cpu_pct", in.Resources.CPUPct,
				"mem_pct", in.Resources.MemPct,
				"disk_free_pct", in.Resources.DiskFreePct,
			)
			return n.autoscale.PostScale(
				cfg.IssuerURL, n.Name(), inserts, zones,
				req.PreferNear, req.Reason, spawnN, req.PreferNears,
			)
		},
		func() error {
			instance, err := IMDSInstanceID()
			if err != nil {
				return fmt.Errorf("no instance id for downscale: %w", err)
			}
			slog.Info("requesting scale-down", "instance", instance)

			if err := n.autoscale.PostDrainLock(cfg.IssuerURL, instance, n.Name()); err != nil {
				return fmt.Errorf("drain lock: %w", err)
			}

			defer func() { _ = n.autoscale.PostDrainUnlock(cfg.IssuerURL, instance, n.Name()) }()

			done := make(chan struct{})
			defer close(done)
			go func() {
				t := time.NewTicker(60 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-done:
						return
					case <-t.C:
						if err := n.autoscale.PostDrainLock(cfg.IssuerURL, instance, n.Name()); err != nil {
							slog.Warn("drain lock renew failed", "instance", instance, "err", err)
						}
					}
				}
			}()

			result := n.SoftLeave(leaveTimeout())
			if !result.OK {
				return fmt.Errorf("soft-leave incomplete: owned=%d queue=%d: %s",
					result.RemainingOwned, result.RemainingQueue, result.Error)
			}
			slog.Info("drained, asking the issuer to terminate",
				"instance", instance,
				"transferred_items", result.TransferredItems,
				"elapsed_ms", result.ElapsedMS)

			if err := n.autoscale.PostDownscale(cfg.IssuerURL, instance, n.Name()); err != nil {

				n.leaving.Store(false)
				return err
			}
			return nil
		},
	)
}

func leaveTimeout() time.Duration {
	if raw := os.Getenv("INDEXUS_LEAVE_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed
		}
		slog.Warn("ignoring invalid INDEXUS_LEAVE_TIMEOUT", "value", raw)
	}
	return 90 * time.Second
}
