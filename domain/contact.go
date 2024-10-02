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
	Get(string, string) (Contact, *Set, error)
	New(*Item, string, string) error
}

func ConvertToContactSlice[T Contact](items []T) []Contact {
	result := make([]Contact, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}
