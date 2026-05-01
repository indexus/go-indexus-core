package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func ownedKeyString(k domain.Key) string {
	return k.Collection + "\x00" + k.Location
}

func (n *Node) Snapshot() []string {
	snapshot := make([]string, 0)

	n.routing.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, p domain.Peer) {
		c, exist := n.registered.Get(0, p.ID())
		if exist {
			arr := make([]string, 0)
			for ip := range c.IPs() {
				arr = append(arr, ip)
			}
			snapshot = append(snapshot, fmt.Sprintf("contact|%s|%s|%d", c.Name(), strings.Join(arr, ","), c.Port()))
		}
	})

	for _, collection := range n.collections.List() {
		snapshot = append(snapshot, fmt.Sprintf("collection|%s", collection.Name()))

		collection.Browse(
			func(ownership string) {
				snapshot = append(snapshot, fmt.Sprintf("ownership|%s", ownership))
			},
			func(ownership, delegation string) {
				snapshot = append(snapshot, fmt.Sprintf("delegation|%s", delegation))
			},
		)
	}
	return snapshot
}

// SnapshotShard persists items under one owned subtree (collection + ownership location).
func (n *Node) SnapshotShards() error {
	var keys []domain.Key
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, sets map[domain.Key]any) {
		for key := range sets {
			keys = append(keys, key)
		}
	})

	var firstErr error
	for _, key := range keys {
		collection, ok := n.collections.Get(key.Collection)
		if !ok {
			continue
		}
		if _, ok := collection.Get(key.Location); !ok {
			// Some owned locations can be logical (delegation graph) without an
			// instantiated set yet; skip snapshot for those.
			continue
		}
		items := make([]string, 0, 256)
		collection.Traverse(
			key.Location,
			func(s string, a *domain.Abelian) {},
			func(s1, s2, s3 string, a *domain.Abelian) {
				item := &domain.Item{Collection: collection.Name(), Location: s2, Id: s3, Metrics: a.Metrics()}
				items = append(items, item.Content())
			},
		)
		if err := n.storage.SnapshotShard(key, items); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func parseItemContentLine(line string) (*domain.Item, error) {
	parts := strings.SplitN(line, "|", 4)
	if len(parts) != 4 {
		return nil, fmt.Errorf("expected 4 fields, got %d", len(parts))
	}
	var metrics []float64
	if parts[3] != "" {
		for _, m := range strings.Split(parts[3], ":") {
			f, err := strconv.ParseFloat(m, 64)
			if err != nil {
				return nil, fmt.Errorf("metric %q: %w", m, err)
			}
			metrics = append(metrics, f)
		}
	}
	return &domain.Item{
		Collection: parts[0],
		Location:   parts[1],
		Id:         parts[2],
		Metrics:    metrics,
	}, nil
}

func (n *Node) Restore() error {
	if !n.storage.Exist() {
		return nil
	}

	commands, err := n.storage.LoadCluster()
	if err != nil {
		return err
	}

	// Replay in two phases so the (non-deterministic) Browse() order in the
	// snapshot can never cause a `delegation|Y` to clobber an `ownership|Y`
	// that happens to be replayed first. Phase 1 only creates contacts,
	// collections and owned subtrees; phase 2 attaches the structural
	// delegations to their parent's owned map.
	var (
		collection string
		// (collection, location) pairs collected for phase 2.
		pendingDelegations []struct{ collection, location string }
	)

	for _, command := range commands {
		arr := strings.Split(command, "|")
		if len(arr) == 0 {
			return errors.New("backup file is corrupted and cannot be restored")
		}

		switch arr[0] {
		case "contact":
			if len(arr) != 4 {
				continue
			}
			name := arr[1]
			ips := strings.Split(arr[2], ",")
			mIps := make(map[string]any)
			for _, ip := range ips {
				mIps[ip] = nil
			}
			port, err := strconv.Atoi(arr[3])
			if err != nil {
				continue
			}
			id, err := encoding.BASE64.Decode(name)
			if err != nil {
				continue
			}
			n.acknowledged.Insert(0, id, n.newContact(name, mIps, port))
		case "collection":
			collection = arr[1]
		case "ownership":
			ownership := arr[1]
			c, exist := n.collections.Get(collection)
			if !exist {
				// First ownership for this collection bootstraps it via the
				// usual constructor (root is always BASE64.Root()).
				n.create(collection, encoding.BASE64.Root())
				c, exist = n.collections.Get(collection)
				if !exist {
					continue
				}
			}
			c.RestoreOwned(ownership)
			n.registerOwnedLocation(c, ownership)
		case "delegation":
			pendingDelegations = append(pendingDelegations, struct{ collection, location string }{collection, arr[1]})
		default:
			return errors.New("backup file is corrupted and cannot be restored")
		}
	}

	for _, p := range pendingDelegations {
		c, ok := n.collections.Get(p.collection)
		if !ok {
			continue
		}
		c.AppendDelegation(p.location)
	}

	ownedKeys := make(map[string]struct{})
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, sets map[domain.Key]any) {
		for key := range sets {
			ownedKeys[ownedKeyString(key)] = struct{}{}
		}
	})

	shardKeys, err := n.storage.Shards()
	if err != nil {
		return err
	}

	for _, sk := range shardKeys {
		if _, ok := ownedKeys[ownedKeyString(sk)]; !ok {
			continue
		}
		snapLines, logLines, err := n.storage.LoadShard(sk)
		if err != nil {
			return err
		}
		for _, line := range snapLines {
			item, err := parseItemContentLine(line)
			if err != nil {
				return fmt.Errorf("shard %v snapshot line: %w", sk, err)
			}
			if _, exist := n.collections.Get(item.Collection); exist {
				_ = n.add(item)
			}
		}
		for _, line := range logLines {
			item, err := parseItemContentLine(line)
			if err != nil {
				return fmt.Errorf("shard %v log line: %w", sk, err)
			}
			if _, exist := n.collections.Get(item.Collection); exist {
				_ = n.add(item)
			}
		}
	}

	return nil
}
