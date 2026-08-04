package core

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

type Manifest struct {
	Node      string                   `json:"node"`
	UpdatedAt time.Time                `json:"updated_at"`
	Zones     []domain.ZoneSnapshotRef `json:"zones"`
	WALSegs   []string                 `json:"wal_segments,omitempty"`
}

type zoneSnapState struct {
	mu       sync.Mutex
	dirty    map[domain.Key]struct{}
	seq      map[domain.Key]int64
	walSegs  []string
	lastFull time.Time
}

func newZoneSnapState() *zoneSnapState {
	return &zoneSnapState{
		dirty: make(map[domain.Key]struct{}),
		seq:   make(map[domain.Key]int64),
	}
}

func (n *Node) markZoneDirty(collection, location string) {
	if n == nil || n.zoneSnap == nil {
		return
	}
	key := domain.Key{Collection: collection, Location: location}
	n.zoneSnap.mu.Lock()
	n.zoneSnap.dirty[key] = struct{}{}
	n.zoneSnap.mu.Unlock()
}

func (n *Node) markOwnedDirtyForItem(collection, location string) {
	if n == nil || n.zoneSnap == nil {
		return
	}
	col, ok := n.collections.Get(collection)
	if !ok {
		n.markZoneDirty(collection, location)
		return
	}
	for loc := location; loc != ""; loc = col.Base().Parent(loc) {
		id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, loc, collection)
		if err != nil {
			continue
		}
		if sets, exist := n.owned.Get(0, id); exist {
			if _, has := sets[domain.Key{Collection: collection, Location: loc}]; has {
				n.markZoneDirty(collection, loc)
				return
			}
		}
		if loc == col.Base().Root() {
			break
		}
	}
	n.markZoneDirty(collection, location)
}

func (n *Node) zoneLines(key domain.Key) ([]string, int, error) {
	collection, ok := n.collections.Get(key.Collection)
	if !ok {
		return nil, 0, fmt.Errorf("collection %q missing", key.Collection)
	}
	lines := []string{
		fmt.Sprintf("collection|%s", key.Collection),
		fmt.Sprintf("ownership|%s", key.Location),
	}
	count := 0
	collection.Traverse(
		key.Location,
		func(string, *domain.Abelian) {},
		func(_ string, location, id string, abelian *domain.Abelian) {
			item := &domain.Item{
				Collection: key.Collection,
				Location:   location,
				Id:         id,
				Metrics:    abelian.Metrics(),
			}
			lines = append(lines, "item|"+item.Content())
			count++
		},
	)
	for _, tomb := range collection.TombstonesUnder(key.Location) {
		lines = append(lines, tomb.Content())
	}
	return lines, count, nil
}

func zoneObjectKey(collection, location string, seq int64) string {
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, location, collection)
	enc := location
	if err == nil {
		enc = encoding.BASE64.Encode(id)
	}
	return fmt.Sprintf("zones/%s/%s/%d.snap", collection, enc, seq)
}

func nodeManifestKey(nodeName string) string {
	return fmt.Sprintf("nodes/%s/manifest.json", nodeName)
}

func nodeWALKey(nodeName, basename string) string {
	return fmt.Sprintf("nodes/%s/wal/%s", nodeName, basename)
}

func gobEncodeStrings(lines []string) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&lines); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gobDecodeStrings(body []byte) ([]string, error) {
	var lines []string
	if err := gob.NewDecoder(bytes.NewReader(body)).Decode(&lines); err != nil {
		return nil, err
	}
	return lines, nil
}

func (n *Node) checkpointZones(ctx context.Context) error {
	store := n.Store()
	if store == nil || n.zoneSnap == nil {
		return nil
	}
	minGap := 30 * time.Second
	if raw := os.Getenv("INDEXUS_ZONE_SNAP_MIN"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			minGap = parsed
		}
	}

	n.zoneSnap.mu.Lock()
	if !n.zoneSnap.lastFull.IsZero() && time.Since(n.zoneSnap.lastFull) < minGap && len(n.zoneSnap.dirty) == 0 {
		n.zoneSnap.mu.Unlock()
		return nil
	}
	dirty := make([]domain.Key, 0, len(n.zoneSnap.dirty))
	for k := range n.zoneSnap.dirty {
		dirty = append(dirty, k)
	}
	forceAll := n.zoneSnap.lastFull.IsZero() || time.Since(n.zoneSnap.lastFull) >= minGap*10
	n.zoneSnap.mu.Unlock()

	if forceAll {
		dirty = n.listOwnedKeys()
	}
	if len(dirty) == 0 {
		return n.uploadManifest(ctx, store)
	}

	budget := 64
	if v := os.Getenv("INDEXUS_ZONE_SNAP_BUDGET"); v != "" {
		fmt.Sscanf(v, "%d", &budget)
	}
	if len(dirty) > budget {
		dirty = dirty[:budget]
	}

	for _, key := range dirty {
		lines, _, err := n.zoneLines(key)
		if err != nil {
			slog.Warn("zone snapshot skipped", "collection", key.Collection, "location", key.Location, "err", err)
			continue
		}
		n.zoneSnap.mu.Lock()
		seq := n.zoneSnap.seq[key] + 1
		n.zoneSnap.seq[key] = seq
		n.zoneSnap.mu.Unlock()

		body, err := gobEncodeStrings(lines)
		if err != nil {
			return err
		}
		objKey := zoneObjectKey(key.Collection, key.Location, seq)
		if err := store.Put(ctx, objKey, body); err != nil {
			slog.Warn("zone snapshot upload failed", "key", objKey, "err", err)
			continue
		}
		n.zoneSnap.mu.Lock()
		delete(n.zoneSnap.dirty, key)
		n.zoneSnap.mu.Unlock()
	}

	n.zoneSnap.mu.Lock()
	n.zoneSnap.lastFull = time.Now()
	n.zoneSnap.mu.Unlock()
	return n.uploadManifest(ctx, store)
}

func (n *Node) uploadManifest(ctx context.Context, store Store) error {
	if n.zoneSnap == nil {
		return nil
	}
	entries := make([]domain.ZoneSnapshotRef, 0)
	n.zoneSnap.mu.Lock()
	seqCopy := make(map[domain.Key]int64, len(n.zoneSnap.seq))
	for k, v := range n.zoneSnap.seq {
		seqCopy[k] = v
	}
	walSegs := append([]string(nil), n.zoneSnap.walSegs...)
	n.zoneSnap.mu.Unlock()

	for _, key := range n.listOwnedKeys() {
		seq := seqCopy[key]
		if seq == 0 {
			continue
		}
		entries = append(entries, domain.ZoneSnapshotRef{
			Collection: key.Collection,
			Location:   key.Location,
			Seq:        seq,
			Key:        zoneObjectKey(key.Collection, key.Location, seq),
		})
	}
	man := Manifest{
		Node:      n.Name(),
		UpdatedAt: time.Now().UTC(),
		Zones:     entries,
		WALSegs:   walSegs,
	}
	body, err := json.Marshal(man)
	if err != nil {
		return err
	}
	return store.Put(ctx, nodeManifestKey(n.Name()), body)
}

func (n *Node) UploadWAL(ctx context.Context, localPath string) error {
	store := n.Store()
	if store == nil || localPath == "" {
		return nil
	}
	st, err := os.Stat(localPath)
	if err != nil || st.Size() == 0 {
		return err
	}
	basename := filepath.Base(localPath)
	key := nodeWALKey(n.Name(), basename)
	if err := putFile(ctx, store, key, localPath); err != nil {
		return err
	}
	if n.zoneSnap != nil {
		n.zoneSnap.mu.Lock()
		n.zoneSnap.walSegs = append(n.zoneSnap.walSegs, key)
		const maxSegs = 32
		if len(n.zoneSnap.walSegs) > maxSegs {
			n.zoneSnap.walSegs = n.zoneSnap.walSegs[len(n.zoneSnap.walSegs)-maxSegs:]
		}
		n.zoneSnap.mu.Unlock()
	}
	slog.Info("wal segment uploaded", "key", key)
	return n.uploadManifest(ctx, store)
}

func pullManifest(ctx context.Context, store Store, nodeName string) (*Manifest, error) {
	if store == nil || nodeName == "" {
		return nil, fmt.Errorf("missing store or node name")
	}
	body, err := store.Get(ctx, nodeManifestKey(nodeName))
	if err != nil {
		return nil, err
	}
	var man Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return nil, err
	}
	return &man, nil
}

func (n *Node) applyZoneLines(lines []string) error {
	var collection string
	for _, command := range lines {
		arr := strings.Split(command, "|")
		if len(arr) < 2 {
			continue
		}
		switch arr[0] {
		case "collection":
			collection = arr[1]
		case "ownership":
			n.create(collection, arr[1])
		case "item":
			item, ok := domain.ParseContent(command)
			if !ok || item.Tombstone {
				continue
			}
			if _, exist := n.collections.Get(item.Collection); !exist {
				continue
			}
			n.add(item, false)
		case "tombstone":
			item, ok := domain.ParseContent(command)
			if !ok || !item.Tombstone {
				continue
			}
			if c, ok := n.collections.Get(item.Collection); ok {
				c.ApplyTombstone(item.Location, item.Id, item.Gen)
			}
		}
	}
	return nil
}

func (n *Node) applyZone(ctx context.Context, store Store, ref domain.ZoneSnapshotRef) error {
	body, err := store.Get(ctx, ref.Key)
	if err != nil {
		return err
	}
	lines, err := gobDecodeStrings(body)
	if err != nil {
		return err
	}
	return n.applyZoneLines(lines)
}

func (n *Node) forceZones(ctx context.Context, keys []domain.Key) error {
	if n.Store() == nil || n.zoneSnap == nil {
		return nil
	}
	if len(keys) == 0 {
		keys = n.listOwnedKeys()
	}
	for _, key := range keys {
		n.markZoneDirty(key.Collection, key.Location)
	}
	n.zoneSnap.mu.Lock()
	n.zoneSnap.lastFull = time.Time{}
	n.zoneSnap.mu.Unlock()
	return n.checkpointZones(ctx)
}

func (n *Node) zoneRef(key domain.Key) (domain.ZoneSnapshotRef, bool) {
	if n.zoneSnap == nil {
		return domain.ZoneSnapshotRef{}, false
	}
	n.zoneSnap.mu.Lock()
	seq := n.zoneSnap.seq[key]
	n.zoneSnap.mu.Unlock()
	if seq == 0 {
		return domain.ZoneSnapshotRef{}, false
	}
	return domain.ZoneSnapshotRef{
		Collection: key.Collection,
		Location:   key.Location,
		Seq:        seq,
		Key:        zoneObjectKey(key.Collection, key.Location, seq),
	}, true
}
