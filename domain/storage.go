package domain

type Storage interface {
	Exist() bool
	Reset() error

	// Cluster state (contacts, collections, ownership, delegations).
	SaveCluster([]string) error
	LoadCluster() ([]string, error)

	// Shards: one (collection, ownership location) subtree per key.
	Shards() ([]Key, error)
	LoadShard(Key) (snapshot []string, logs []string, err error)
	SnapshotShard(Key, []string) error // atomic: write snapshot + truncate log
	AppendShard(Key, string)
	DropShard(Key) error
}
