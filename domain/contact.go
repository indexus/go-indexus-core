package domain

import "strings"

type Peer interface {
	ID() []byte
	Name() string
}

// Visited names the nodes a deep read has already crossed, in order. It is what
// terminates a forwarded lookup: a node finding itself in the list cancels
// instead of answering again, so a routing cycle dies where it closes rather
// than after an arbitrary number of steps. Its length is also the true distance
// from the client, which is what the cache stamps as hops.
type Visited []string

func (v Visited) Has(name string) bool {
	for _, n := range v {
		if n == name {
			return true
		}
	}
	return false
}

func (v Visited) With(name string) Visited {
	return append(append(make(Visited, 0, len(v)+1), v...), name)
}

func (v Visited) String() string {
	return strings.Join(v, ",")
}

func ParseVisited(raw string) Visited {
	var out Visited
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

type Contact interface {
	Peer

	IPs() map[string]any
	Port() int
	IP() string
	Host() string

	Ping(Contact) (Contact, error)
	Neighbors(Peer) ([]Contact, error)
	Random(Peer) (Contact, error)
	Transfer(Peer, Key, []*Item) (string, error)
	Get(collection, location string, deep bool, via Visited, refresh bool) (Contact, *Set, error)
	// Children asks a peer which direct children of parent it owns, or has
	// delegated away with a redirect.
	Children(collection, parent string) (map[string]*ChildEntry, error)
	// New and Delete place a write. The third argument is via — peers that
	// already declined on this attempt — not a walk position. Placement always
	// climbs from item.Location.
	New(*Item, string, Visited) error
	Delete(*Item, string, Visited) error
}

func ConvertToContactSlice[T Contact](items []T) []Contact {
	result := make([]Contact, len(items))
	for i, item := range items {
		result[i] = item
	}
	return result
}

// ChildEntry is one row of GET /children: the abelian stub of a direct child,
// plus a redirect when the callee has delegated that child away.
type ChildEntry struct {
	Abelian      *Abelian       `json:"abelian"`
	RedirectName string         `json:"redirect_name,omitempty"`
	RedirectIP   string         `json:"redirect_ip,omitempty"`
	RedirectPort int            `json:"redirect_port,omitempty"`
	RedirectIPs  map[string]any `json:"redirect_ips,omitempty"`
}

func (e *ChildEntry) HasRedirect() bool {
	return e != nil && e.RedirectName != "" && e.RedirectPort > 0
}

type ZoneSnapshotRef struct {
	Collection string `json:"collection"`
	Location   string `json:"location"`
	Seq        int64  `json:"seq"`
	Key        string `json:"key"`
	ItemCount  int    `json:"item_count"`
}

// ClaimPayload is the child→parent announcement used by convergent repair:
// the owner of Child tells the holder of Parent to record a named mark.
type ClaimPayload struct {
	Collection string `json:"collection"`
	Parent     string `json:"parent"`
	Child      string `json:"child"`
	Peer       string `json:"peer"`
}
