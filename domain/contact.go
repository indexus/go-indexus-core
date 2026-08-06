package domain

type Contact interface {
	Peer

	IPs() map[string]any
	Port() int
	IP() string
	Host() string

	Ping(Contact) (Contact, error)
	Neighbors(Peer) ([]Contact, error)
	Random(Peer) (Contact, error)
	Transfer(Peer, Key, []*Item) error
	// Get resolves a set. deep path-fills via peers; hop is the remaining
	// recursion budget (0 with deep=true means use the peer default).
	Get(collection, location string, deep bool, hop int) (Contact, *Set, error)
	New(*Item, string, string) error
	Delete(*Item, string, string) error
}

func ConvertToContactSlice[T Contact](items []T) []Contact {
	result := make([]Contact, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}
