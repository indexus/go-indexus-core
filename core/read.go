package core

import (
	"bytes"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// errVisited cancels a read that came back to a node already on its path. It
// is an error and not an empty set: a cycle means the answer is unknown here,
// and an empty set would read as a truth (I4).
var errVisited = errors.New("read already visited this node")

// path starts a read this node originates: we are already on it, so a peer that
// would route back here cancels instead.
func (n *Node) path() domain.Visited {
	return domain.Visited{n.Name()}
}

// zoneNearest names the node a read should be credited to. Unlike find() it
// never fails and may answer with a quarantined peer or with ourselves: a read
// must degrade to a stale copy, never to an error (K4).
func (n *Node) zoneNearest(collection, location string) (domain.Contact, error) {
	id, err := zoneKeyID(collection, location)
	if err != nil {
		return nil, err
	}
	return n.registered.Nearest(0, id), nil
}

func (n *Node) remote(c domain.Contact) bool {
	return c != nil && c.Name() != n.Name() && contactDialable(c)
}

// zoneFanout is Kademlia's α: how many peers a lookup may ask once the
// XOR-nearest one failed to answer. Large enough that one stale route heals on
// the same tick, small enough that a batch of cold locations cannot walk the
// whole mesh.
const zoneFanout = 3

// zoneCandidates returns the live peers ordered by XOR distance to the zone key
// — the order in which they are likely to own it — capped at max. Ordering by
// the key rather than by traversal order is what makes a small cap sufficient.
// Peers already on the read's path are dropped: asking them would only earn a
// cancellation.
func (n *Node) zoneCandidates(collection, location string, max int, via domain.Visited, skip ...domain.Contact) []domain.Contact {
	id, err := zoneKeyID(collection, location)
	if err != nil {
		return nil
	}

	seen := map[string]struct{}{n.Name(): {}}
	for _, name := range via {
		seen[name] = struct{}{}
	}
	for _, c := range skip {
		if c != nil {
			seen[c.Name()] = struct{}{}
		}
	}

	var out []domain.Contact
	for _, peers := range [][]domain.Contact{n.traverseRegistered(false), n.traverseAcknowledged(false)} {
		for _, peer := range peers {
			if !n.remote(peer) || n.suspended(peer.Name()) {
				continue
			}
			if _, ok := seen[peer.Name()]; ok {
				continue
			}
			seen[peer.Name()] = struct{}{}
			out = append(out, peer)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(xorDistance(out[i].ID(), id), xorDistance(out[j].ID(), id)) < 0
	})
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// serveStale answers with a local copy of a zone we do not own — a leftover
// from before a handoff. It credits the owner rather than ourselves so the
// client re-routes on its next call instead of pinning itself to us.
func (n *Node) serveStale(nearest domain.Contact, set *domain.Set) (domain.Contact, *domain.Set, error) {
	if n.remote(nearest) {
		return nearest, set, nil
	}
	return n, set, nil
}

// RoutingHint returns the XOR-nearest dialable peer for clientKey when that peer
// is not this node, so sticky clients adopt a closer ingress without polling.
func (n *Node) RoutingHint(clientKey []byte) domain.Contact {
	if len(clientKey) == 0 {
		return nil
	}
	nearest := n.registered.Nearest(0, clientKey)
	if !n.remote(nearest) {
		return nil
	}
	return nearest
}

// Get resolves collection/location. deep path-fills toward XOR-nearest, adding
// this node to via before forwarding so the walk stops when it closes on
// itself; refresh is a SoT-only pull that leaves the LRU untouched.
func (n *Node) Get(collection, location string, deep bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {
	if via.Has(n.Name()) {
		return nil, nil, errVisited
	}
	if refresh {
		return n.resolveRefresh(collection, location, via.With(n.Name()))
	}
	return n.resolveClient(collection, location, deep, via.With(n.Name()))
}

func (n *Node) resolveRefresh(collection, location string, via domain.Visited) (domain.Contact, *domain.Set, error) {
	nearest, err := n.zoneNearest(collection, location)
	if err != nil {
		return nil, nil, err
	}

	if c, exist := n.collections.Get(collection); exist {
		if set, ok := c.Get(location); ok && n.ownsLocation(collection, location) {
			return n, set, nil
		}
	}

	var pullErr error
	if n.remote(nearest) && !via.Has(nearest.Name()) {
		_, set, err := nearest.Get(collection, location, true, via, true)
		if err == nil && set != nil && set.Count() > 0 {
			return nearest, set, nil
		}
		pullErr = err
	}

	if set, peer := n.tryPeersGet(collection, location, via, true, nearest); set != nil {
		return peer, set, nil
	}
	return nearest, nil, pullErr
}

func (n *Node) resolveClient(collection, location string, deep bool, via domain.Visited) (domain.Contact, *domain.Set, error) {
	nearest, err := n.zoneNearest(collection, location)
	if err != nil {
		return nil, nil, err
	}

	// A non-owned local copy is a post-handoff leftover: it no longer receives
	// Update pushes, so it only serves as a fallback once path-fill has failed.
	var stale *domain.Set

	if c, exist := n.collections.Get(collection); exist {
		if set, ok := c.Get(location); ok {
			if n.ownsLocation(collection, location) {
				return n, set, nil
			}
			if !deep {
				return n.serveStale(nearest, set)
			}
			stale = set
		}
	}

	// Placeholders written by aggregate pulls are summaries, not child lists.
	if set, exist := n.cache.Get(collection, location); exist && set != nil && !set.IsSummaryPlaceholder() {
		return nearest, set, nil
	}

	if !deep {
		n.cache.SetHops(collection, location, nil, 0)
		return nearest, nil, nil
	}

	// len(via) counts this node, so it is the number of nodes between the
	// client and the answer: exactly the distance the cache TTL decays with.
	hops := len(via)

	if n.remote(nearest) && !via.Has(nearest.Name()) {
		_, set, err := nearest.Get(collection, location, true, via, false)
		if err == nil && set != nil && set.Count() > 0 {
			if n.settings.leafRedirect > 0 && set.Count() >= n.settings.leafRedirect {
				hops++
			}
			n.cache.SetHops(collection, location, set, hops)
			return nearest, set, nil
		}
	}

	if set, peer := n.tryPeersGet(collection, location, via, false, nearest); set != nil {
		n.cache.SetHops(collection, location, set, hops)
		return peer, set, nil
	}

	if stale != nil {
		return n.serveStale(nearest, stale)
	}
	return nearest, nil, nil
}

// tryPeersGet asks the peers nearest the zone key for a non-empty set: the XOR
// nearest is a hint, not the SoT, and PreferNear names often diverge from data
// prefixes. Refresh stays cheap — one peer at most, nothing while a transfer is
// running.
func (n *Node) tryPeersGet(collection, location string, via domain.Visited, refresh bool, skip domain.Contact) (*domain.Set, domain.Contact) {
	if refresh && n.TransferBusy() {
		return nil, nil
	}

	max := zoneFanout
	if refresh {
		max = 1
	}
	for _, peer := range n.zoneCandidates(collection, location, max, via, skip) {
		_, set, err := peer.Get(collection, location, true, via, refresh)
		if err == nil && set != nil && set.Count() > 0 {
			return set, peer
		}
	}
	return nil, nil
}

// ownsLocation reports whether an owned key covers collection/location, so a
// donor stays authoritative until Delegate completes.
func (n *Node) ownsLocation(collection, location string) bool {
	root := encoding.BASE64.Root()
	found := false
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(_ int, _ []byte, sets map[domain.Key]any) {
		if found {
			return
		}
		for key := range sets {
			if key.Collection != collection {
				continue
			}
			loc := key.Location
			if loc == root {
				loc = ""
			}
			if loc == "" || loc == location || strings.HasPrefix(location, loc) {
				found = true
				return
			}
		}
	})
	return found
}

// GetAggregates returns SoT summaries for locally owned locations only, and
// never forwards. Answering is therefore a claim of ownership: it is what makes
// a response authoritative for a parent stub, and what lets a peer detect a
// duplicate owner. See protocol.md §2.
func (n *Node) GetAggregates(collection string, locations []string) (map[string]*domain.Abelian, error) {
	c, ok := n.collections.Get(collection)
	if !ok {
		return nil, errors.New("no collection")
	}
	out := make(map[string]*domain.Abelian, len(locations))
	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc == "" || !n.ownsLocation(collection, loc) {
			continue
		}
		set, ok := c.Get(loc)
		if !ok || set == nil {
			continue
		}
		out[loc] = set.Abelian()
	}
	return out, nil
}

// batchParallelism caps the round trips a single /sets batch may have in
// flight. A cold batch is one path-fill per location, so resolving them in
// sequence makes latency linear in the batch size; resolving all of them at
// once would let one client open a connection per location on every peer.
const batchParallelism = 8

// GetMultiple resolves each location like Get and encodes the hits; misses are
// omitted from the body. No local collection is required — a sticky client may
// land on a node that never saw this collection and still reach the SoT by
// path-fill.
//
// refresh carries the same meaning as on Get, and it has to: a batch read is
// the only way an aggregate client reads, so dropping the flag here left it no
// way to ever see past this node's cache.
//
// When envelope is true the body is wrapped as an IXS1 frame that also lists
// owner redirects for locations that did not resolve locally (client-managed
// deep=false navigation). Legacy callers leave envelope false and receive the
// raw EncodeSets stream.
func (n *Node) GetMultiple(collection string, locations []string, precision int, properties []func(*domain.Abelian) int, deep bool, via domain.Visited, refresh bool, envelope bool) ([]byte, error) {
	resolved := make(map[string]*domain.Set, len(locations))
	var redirects []domain.SetsRedirect

	var mu sync.Mutex
	var wg sync.WaitGroup
	slots := make(chan struct{}, batchParallelism)

	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc == "" {
			continue
		}
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			contact, set, err := n.Get(collection, loc, deep, via, refresh)
			if err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if set != nil {
				resolved[loc] = set
				return
			}
			if !envelope || contact == nil || !n.remote(contact) {
				return
			}
			redirects = append(redirects, domain.SetsRedirect{
				Location: loc,
				Name:     contact.Name(),
				IP:       contact.IP(),
				Port:     contact.Port(),
			})
		}()
	}
	wg.Wait()

	body, err := domain.EncodeSets(resolved, locations, precision, properties)
	if err != nil {
		return nil, err
	}
	if !envelope {
		return body, nil
	}
	return domain.EncodeSetsEnvelope(body, redirects), nil
}
