package domain

type Storage interface {
	Exist() bool
	Reset() error

	// Cluster state (contacts, collections, ownership, delegations).
	SaveCluster([]string) error
	LoadCluster() ([]string, error)

	// Shards: one (collection, ownership location) subtree per key.
	Shards() ([]Key, error)
	// LoadShardHeader reads only the aggregate header from a .snapshot file.
	// Used by lazy Restore to inject aggregates without loading item data.
	LoadShardHeader(Key) (map[string]*Abelian, error)
	// LoadShard reads the full shard: header, snapshot items, and WAL log lines.
	LoadShard(Key) (header map[string]*Abelian, items []string, logs []string, err error)
	// SnapshotShard atomically writes header + items and truncates the WAL.
	SnapshotShard(Key, map[string]*Abelian, []string) error
	AppendShard(Key, string)
	DropShard(Key) error
}
