package core

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func ownedKeyString(k domain.Key) string {
	return k.Collection + "\x00" + k.Location
}

// hashCommands fingerprints a snapshot command list so we can skip writing
// cluster.snapshot when it is identical to the previously saved version.
func hashCommands(commands []string) uint64 {
	h := fnv.New64a()
	for _, c := range commands {
		_, _ = h.Write([]byte(c))
		_, _ = h.Write([]byte{0})
	}
	return h.Sum64()
}

// SaveClusterIfDirty writes cluster.snapshot only when the snapshot content
// differs from the last successful save. Returns whether a write happened.
func (n *Node) SaveClusterIfDirty() (bool, error) {
	commands := n.Snapshot()
	h := hashCommands(commands)
	if h == n.lastClusterHash && n.lastClusterHash != 0 {
		return false, nil
	}
	if err := n.storage.SaveCluster(commands); err != nil {
		return false, err
	}
	n.lastClusterHash = h
	return true, nil
}

// Snapshot serialises the cluster-level state (contacts, collections,
// ownership, delegations) as a list of replay-able command strings.
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

// extractShard returns the header (owner-level Set.List()) and the flat
// item-content lines for the given owned subtree. Used by Refresh to build
// the ShardSnapshot written to disk.
func extractShard(collection *domain.Collection, owner string) (map[string]*domain.Abelian, []string) {
	set, ok := collection.Get(owner)
	if !ok {
		return nil, nil
	}
	header := set.List()
	items := make([]string, 0, 256)
	name := collection.Name()
	collection.Traverse(
		owner,
		func(_ string, _ *domain.Abelian) {},
		func(_, loc, id string, a *domain.Abelian) {
			items = append(items, domain.ItemContent(name, loc, id, a.Metrics()))
		},
	)
	return header, items
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

// loadShard fetches header + items + logs from storage and injects them into
// the collection. Called by EnsureLoaded when a shard misses in RAM.
func (n *Node) loadShard(k domain.Key) error {
	c, ok := n.collections.Get(k.Collection)
	if !ok {
		return nil
	}
	header, snapLines, logLines, err := n.storage.LoadShard(k)
	if err != nil {
		return fmt.Errorf("load shard %v: %w", k, err)
	}
	if header != nil {
		c.SetOwnerAggregate(k.Location, header)
	}

	allLines := append(snapLines, logLines...)
	items := make([]*domain.Item, 0, len(allLines))
	for _, line := range allLines {
		item, err := parseItemContentLine(line)
		if err != nil {
			return fmt.Errorf("shard %v line parse: %w", k, err)
		}
		items = append(items, item)
	}
	return c.LoadShardItems(k.Location, items)
}

// Restore replays the cluster snapshot and then registers every on-disk shard
// as Evicted in the ShardManager. Shard item data is NOT loaded eagerly —
// only the header (owner-level aggregates) is injected so that /set queries
// at the ownership boundary are served immediately. Deep items are loaded on
// first access via EnsureLoaded.
func (n *Node) Restore() error {
	if !n.storage.Exist() {
		return nil
	}

	commands, err := n.storage.LoadCluster()
	if err != nil {
		return err
	}

	// Two-phase replay so that delegation markers never clobber owned subtrees
	// that happen to be replayed first (map iteration is non-deterministic).
	// Phase 1: contacts, collections, ownership.
	// Phase 2: structural delegations.
	var (
		collection         string
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

	// Build the set of keys that this node owns so we can skip orphaned shards.
	ownedKeys := make(map[string]struct{})
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(_ int, _ []byte, sets map[domain.Key]any) {
		for key := range sets {
			ownedKeys[ownedKeyString(key)] = struct{}{}
		}
	})

	// For every shard on disk: inject the header into the collection and
	// register as Evicted. Items are loaded lazily on first access.
	shardKeys, err := n.storage.Shards()
	if err != nil {
		return err
	}

	for _, sk := range shardKeys {
		if _, ok := ownedKeys[ownedKeyString(sk)]; !ok {
			continue
		}
		header, err := n.storage.LoadShardHeader(sk)
		if err != nil {
			return fmt.Errorf("shard %v header: %w", sk, err)
		}
		c, ok := n.collections.Get(sk.Collection)
		if !ok {
			continue
		}
		if header != nil {
			c.SetOwnerAggregate(sk.Location, header)
		}
		n.shards.Register(sk)
	}

	return nil
}
