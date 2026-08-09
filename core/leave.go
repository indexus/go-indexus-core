package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

type LeaveResult struct {
	OK               bool     `json:"ok"`
	TransferredKeys  int      `json:"transferred_keys"`
	TransferredItems int      `json:"transferred_items"`
	FailedKeys       []string `json:"failed_keys,omitempty"`
	RemainingOwned   int      `json:"remaining_owned"`
	RemainingQueue   int      `json:"remaining_queue"`
	SnapshotPath     string   `json:"snapshot_path,omitempty"`
	S3URI            string   `json:"s3_uri,omitempty"`
	ElapsedMS        int64    `json:"elapsed_ms"`
	Error            string   `json:"error,omitempty"`
}

func (n *Node) SoftLeave(idleBudget time.Duration) LeaveResult {
	start := time.Now()
	result := LeaveResult{OK: false}

	n.leaving.Store(true)
	n.transferMu.Lock()
	defer n.transferMu.Unlock()

	if idleBudget <= 0 {
		idleBudget = 90 * time.Second
	}
	hardCap := leaveHardCap(idleBudget)
	hardDeadline := time.Now().Add(hardCap)
	idleDeadline := time.Now().Add(idleBudget)

	if err := n.Checkpoint(); err != nil {
		slog.Warn("leave snapshot not saved", "err", err)
	}

	if err := n.forceZones(context.Background(), nil); err != nil {
		slog.Warn("leave zone checkpoint failed", "err", err)
	}
	result.SnapshotPath = fmt.Sprintf("%s.snapshot", storageFilename())

	dump := newLeaveDump()
	defer dump.Close()

	targets := newDrainTargets(n.liveTransferPeers())

	for time.Now().Before(idleDeadline) && time.Now().Before(hardDeadline) {
		keys := n.listOwnedKeys()
		if len(keys) == 0 {
			break
		}

		if targets.empty() {
			targets = newDrainTargets(n.liveTransferPeers())
			if targets.empty() {

				result.Error = "no live peers available for soft-transfer"
				result.RemainingOwned = len(keys)
				time.Sleep(500 * time.Millisecond)
				continue
			}
			result.Error = ""
		}

		progress := false
		for _, key := range keys {
			if time.Now().After(idleDeadline) || time.Now().After(hardDeadline) {
				break
			}

			collection, ok := n.collections.Get(key.Collection)
			if !ok {
				n.removeOwnedKey(key)
				progress = true
				idleDeadline = time.Now().Add(idleBudget)
				continue
			}

			items, empty := collection.Delegate(key.Location)
			dump.Write(items)

			if err := targets.transfer(n, key, items); err != nil {
				slog.Warn("leave transfer failed, keeping the zone",
					"collection", key.Collection,
					"location", key.Location,
					"items", len(items),
					"err", err)
				n.restoreDelegated(key, items)
				result.FailedKeys = append(result.FailedKeys, key.Collection+":"+key.Location)
				continue
			}

			n.removeOwnedKey(key)
			if empty {
				n.collections.Delete(key.Collection)
			}
			n.cache.ForgetUnder(key.Collection, key.Location)
			count := len(items)
			dropTransferredRefs(items)
			if count >= 64 {
				releaseHeapAfterTransfer(count)
			}

			result.TransferredKeys++
			result.TransferredItems += count
			progress = true
			idleDeadline = time.Now().Add(idleBudget)
			result.Error = ""
		}

		if !progress {

			targets = newDrainTargets(n.liveTransferPeers())
			time.Sleep(200 * time.Millisecond)
			continue
		}
		time.Sleep(200 * time.Millisecond)
	}

	grace := time.Now().Add(2 * time.Second)
	if grace.After(hardDeadline) {
		grace = hardDeadline
	}
	for time.Now().Before(grace) && n.Queue() > 0 {
		time.Sleep(200 * time.Millisecond)
	}

	for pass := 0; pass < 3; pass++ {
		n.drainQueue(hardDeadline, &result)
		if n.Queue() == 0 || time.Now().After(hardDeadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	result.RemainingOwned = len(n.listOwnedKeys())
	result.RemainingQueue = n.Queue()
	if result.RemainingOwned == 0 && result.RemainingQueue == 0 {
		result.OK = true
		result.Error = ""
	} else if result.Error == "" {
		result.Error = fmt.Sprintf("owned=%d queue=%d remain", result.RemainingOwned, result.RemainingQueue)
	}
	if !result.OK {

		n.leaving.Store(false)
	}

	_ = os.Setenv("INDEXUS_NODE_ID", n.Name())
	if uri := uploadLeaveArtifacts(dump.path, result.SnapshotPath); uri != "" {
		result.S3URI = uri
	}

	result.ElapsedMS = time.Since(start).Milliseconds()
	return result
}

type drainTargets struct {
	peers []domain.Contact
	skip  map[string]bool
	fails map[string]int
}

func newDrainTargets(peers []domain.Contact) *drainTargets {
	return &drainTargets{
		peers: peers,
		skip:  make(map[string]bool),
		fails: make(map[string]int),
	}
}

func (d *drainTargets) empty() bool {
	if d == nil {
		return true
	}
	for _, p := range d.peers {
		if !d.skip[p.Name()] {
			return false
		}
	}
	return true
}

func (d *drainTargets) transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {
	var lastErr error
	tried := 0
	for _, peer := range d.peers {
		name := peer.Name()
		if d.skip[name] {
			continue
		}
		tried++
		_, err := peer.Transfer(origin, key, items)
		if err == nil {
			d.fails[name] = 0
			return nil
		}
		lastErr = err
		if errors.Is(err, domain.ErrLeaving) {
			slog.Info("leave skipping peer that is itself leaving", "peer", name)
			d.skip[name] = true
			continue
		}
		d.fails[name]++
		if d.fails[name] >= 2 {
			slog.Warn("leave skipping peer after repeated failures", "peer", name, "err", err)
			d.skip[name] = true
		}
	}
	if tried == 0 {
		return fmt.Errorf("no live peers available for soft-transfer")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no live peers available for soft-transfer")
	}
	return lastErr
}

func (n *Node) drainQueue(deadline time.Time, result *LeaveResult) {
	targets := newDrainTargets(n.liveTransferPeers())
	if targets.empty() {
		return
	}

	for time.Now().Before(deadline) {
		element, exist := n.queue.TryConsume()
		if !exist {
			return
		}

		key := domain.Key{Collection: element.item.Collection, Location: element.root}
		if err := n.handOffTargets(targets, key, element); err != nil {
			slog.Warn("leave hand-off failed, keeping the write",
				"op", element.op.String(),
				"collection", key.Collection,
				"location", key.Location,
				"err", err)
			n.queue.Add(element)
			return
		}

		result.TransferredItems++
	}
}

func (n *Node) handOffTargets(targets *drainTargets, key domain.Key, element *Element) error {
	if element.op == OpDelete {
		var lastErr error
		for _, peer := range targets.peers {
			if targets.skip[peer.Name()] {
				continue
			}
			if err := peer.Delete(element.item, element.root, nil); err != nil {
				lastErr = err
				if errors.Is(err, domain.ErrLeaving) {
					targets.skip[peer.Name()] = true
					continue
				}
				continue
			}
			return nil
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("no live peers available for soft-transfer")
		}
		return lastErr
	}
	return targets.transfer(n, key, []*domain.Item{element.item})
}

func (n *Node) listOwnedKeys() []domain.Key {
	out := make([]domain.Key, 0)
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(_ int, _ []byte, sets map[domain.Key]any) {
		for key := range sets {
			out = append(out, key)
		}
	})
	return out
}

func (n *Node) liveTransferPeers() []domain.Contact {
	isBootstrap := make(map[string]bool, len(n.bootstraps))
	for _, contact := range n.bootstraps {
		isBootstrap[contact.Name()] = true
	}

	var bootstraps, others []domain.Contact
	seen := make(map[string]bool)

	for _, contact := range append(n.traverseRegistered(false), n.bootstraps...) {
		if contact.Name() == n.Name() || seen[contact.Name()] || !contactDialable(contact) {
			continue
		}
		seen[contact.Name()] = true
		if isBootstrap[contact.Name()] {
			bootstraps = append(bootstraps, contact)
		} else {
			others = append(others, contact)
		}
	}

	return append(bootstraps, others...)
}

func (n *Node) removeOwnedKey(key domain.Key) {
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, key.Location, key.Collection)
	if err != nil {
		return
	}
	n.owned.Update(0, id, func(_ int, _ []byte, sets map[domain.Key]any) {
		delete(sets, key)
	})
	if sets, ok := n.owned.Get(0, id); ok && len(sets) == 0 {
		n.owned.Remove(0, id)
	}
}

func leaveHardCap(idleBudget time.Duration) time.Duration {
	if raw := os.Getenv("INDEXUS_LEAVE_MAX"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed
		}
		slog.Warn("ignoring invalid INDEXUS_LEAVE_MAX", "value", raw)
	}
	cap := idleBudget * 10
	if cap < 5*time.Minute {
		cap = 5 * time.Minute
	}
	if cap > 30*time.Minute {
		cap = 30 * time.Minute
	}
	return cap
}

type leaveDump struct {
	path    string
	file    *os.File
	encoder *json.Encoder
}

func newLeaveDump() *leaveDump {
	dir := os.Getenv("INDEXUS_LEAVE_DIR")
	if dir == "" {
		dir = "/var/lib/indexus/leave"
	}
	path := filepath.Join(dir, fmt.Sprintf("leave-%d.jsonl", time.Now().Unix()))

	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("no leave dump, directory unavailable", "dir", dir, "err", err)
		return &leaveDump{}
	}
	file, err := os.Create(path)
	if err != nil {
		slog.Warn("no leave dump, file unavailable", "path", path, "err", err)
		return &leaveDump{}
	}

	return &leaveDump{path: path, file: file, encoder: json.NewEncoder(file)}
}

func (d *leaveDump) Write(items []*domain.Item) {
	if d.encoder == nil {
		return
	}
	for _, item := range items {
		_ = d.encoder.Encode(item)
	}
}

func (d *leaveDump) Close() {
	if d.file != nil {
		_ = d.file.Close()
	}
}

func storageFilename() string {
	if path := os.Getenv("INDEXUS_STORAGE"); path != "" {
		return path
	}
	return "/var/lib/indexus/backup"
}

func uploadLeaveArtifacts(dumpPath, snapshotPath string) string {
	bucket := os.Getenv("SNAPSHOT_BUCKET")
	if bucket == "" {
		return ""
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "eu-west-3"
	}
	nodeID := os.Getenv("INDEXUS_NODE_ID")
	if nodeID == "" {
		nodeID = "node"
	}
	prefix := fmt.Sprintf("snapshots/%s/%d", nodeID, time.Now().Unix())
	uploaded := ""
	for _, local := range []string{dumpPath, snapshotPath} {
		if local == "" {
			continue
		}
		if st, err := os.Stat(local); err != nil || st.Size() == 0 {
			continue
		}
		key := prefix + "/" + filepath.Base(local)
		dest := fmt.Sprintf("s3://%s/%s", bucket, key)
		out, err := exec.Command("aws", "s3", "cp", local, dest, "--region", region).CombinedOutput()
		if err != nil {
			slog.Warn("leave artifact not uploaded", "dest", dest, "err", err, "output", string(out))
			continue
		}
		uploaded = fmt.Sprintf("s3://%s/%s", bucket, prefix)
	}
	return uploaded
}

func (n *Node) Leave(timeoutSeconds int) any {
	return n.SoftLeave(time.Duration(timeoutSeconds) * time.Second)
}

func (n *Node) Leaving() bool {
	return n.leaving.Load()
}
