package core

import (
	"log/slog"
	"math/rand"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

const parentSyncDebounce = 2 * time.Second

type refreshKey struct {
	collection string
	location   string
}

type updateStats struct {
	delegPulls, delegPullOK            int
	candidates, skipEta, self, skipCap int
	pulls, pullOK, batchRPC            int
	transferBusy                       bool
}

func (s *updateStats) log(started time.Time) {
	slog.Info("update tick",
		"deleg_pulls", s.delegPulls,
		"deleg_pull_ok", s.delegPullOK,
		"cache_candidates", s.candidates,
		"cache_skip_eta", s.skipEta,
		"cache_self", s.self,
		"cache_skip_cap", s.skipCap,
		"cache_pulls", s.pulls,
		"cache_pull_ok", s.pullOK,
		"batch_rpc", s.batchRPC,
		"transfer_busy", s.transferBusy,
		"dur", time.Since(started).String(),
	)
}

func (n *Node) Update() error {
	started := time.Now()
	stats := &updateStats{transferBusy: n.TransferBusy()}
	defer stats.log(started)

	if n.settings.cacheMax > 0 {
		n.cache.SetMax(n.settings.cacheMax)
	}

	// Convergent repair: children announce themselves to parent holders; named
	// marks are verified. This replaces silence-based reclaim and discovery.
	n.repair()
	n.reconcileDelegatedParents(stats)

	if stats.transferBusy {
		return nil
	}

	var keys []refreshKey
	for collection, sets := range n.cache.Refresh(n.settings.expiration, n.settings.cacheBeta) {
		for location := range sets {
			keys = append(keys, refreshKey{collection, location})
		}
	}
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	stats.candidates = len(keys)

	maxPulls := n.settings.UpdateMaxPulls()
	batch := n.settings.UpdateBatch()
	selected := 0

	type peerPull struct {
		contact domain.Contact
		keys    []refreshKey
	}
	byPeer := make(map[string]*peerPull)
	var ordered []*peerPull

	for _, k := range keys {
		if n.settings.updateEta > 0 && rand.Float64() < n.settings.updateEta {
			stats.skipEta++
			continue
		}

		contact, err := n.find(k.collection, k.location)
		if err != nil {
			slog.Debug("cache refresh has no route", "collection", k.collection, "location", k.location, "err", err)
			continue
		}
		if !n.remote(contact) {
			stats.self++
			continue
		}
		if selected >= maxPulls {
			stats.skipCap++
			continue
		}
		selected++

		if batch {
			p := byPeer[contact.Name()]
			if p == nil {
				p = &peerPull{contact: contact}
				byPeer[contact.Name()] = p
				ordered = append(ordered, p)
			}
			p.keys = append(p.keys, k)
			continue
		}

		stats.pulls++
		_, set, err := contact.Get(k.collection, k.location, true, n.path(), true)
		if err != nil {
			slog.Debug("cache refresh unanswered", "peer", contact.Name(), "collection", k.collection, "location", k.location, "err", err)
			continue
		}
		if set != nil {
			stats.pullOK++
			n.applyPulledSet(k.collection, k.location, set)
		}
		time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
	}

	for _, p := range ordered {
		// One peer, but possibly several collections: /aggregates takes one.
		byCollection := make(map[string][]refreshKey)
		for _, k := range p.keys {
			byCollection[k.collection] = append(byCollection[k.collection], k)
		}
		for collection, ks := range byCollection {
			locations := make([]string, len(ks))
			for i, k := range ks {
				locations[i] = k.location
			}
			stats.pulls += len(locations)
			stats.batchRPC++

			aggs := n.askAggregates(p.contact, collection, locations)
			if len(aggs) == 0 {
				for _, k := range ks {
					if _, set, err := p.contact.Get(collection, k.location, true, n.path(), true); err == nil && set != nil {
						stats.pullOK++
						n.applyPulledSet(collection, k.location, set)
					}
				}
				continue
			}
			for location, ab := range aggs {
				if ab != nil {
					stats.pullOK++
					n.applyPulledAbelian(collection, location, ab)
				}
			}
		}
		time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
	}

	return nil
}

// applyPulledSet warms the cache from a /set read. A /set response can come
// from a cache or a path-fill, so it is not an authority on the zone and never
// writes a parent stub.
func (n *Node) applyPulledSet(collection, location string, set *domain.Set) {
	if set == nil {
		return
	}
	hops := n.cache.Hops(collection, location)
	if hops < 1 {
		hops = 1
	}
	n.cache.SetHops(collection, location, set, hops)
}

// applyPulledAbelian applies a /aggregates response: the peer answered for a
// location it owns, so it is the authority and may write the parent stub.
func (n *Node) applyPulledAbelian(collection, location string, ab *domain.Abelian) {
	if ab == nil {
		return
	}
	n.cache.SetHops(collection, location, domain.NewSetFromAbelian(ab), 1)
	if c, exist := n.collections.Get(collection); exist {
		c.SetChildSummary(location, ab)
	}
}

// askAggregates is the /aggregates RPC: it returns what peer claims to own
// among locations, and nothing at all when peer cannot answer. No caller
// distinguishes "refused" from "owns none of it" — both mean look elsewhere.
func (n *Node) askAggregates(peer domain.Contact, collection string, locations []string) map[string]*domain.Abelian {
	a, ok := peer.(interface {
		GetAggregates(collection string, locations []string) (map[string]*domain.Abelian, error)
	})
	if !ok {
		return nil
	}
	aggs, err := a.GetAggregates(collection, locations)
	if err != nil {
		return nil
	}
	return aggs
}

// reconcileDelegatedParents pulls abelian summaries for every known delegated
// child (U3). Child discovery and silence-based reclaim are retired: repair()
// pushes Claim and verifies named marks instead.
func (n *Node) reconcileDelegatedParents(stats *updateStats) {
	for _, collection := range n.collections.List() {
		for location := range collection.Refresh() {
			stats.delegPulls++
			if n.pullDelegated(collection.Name(), location, nil) == pullOK {
				stats.delegPullOK++
			}
		}
	}
}

// Children is the other side of that RPC: the local inventory of direct
// children under parent, merging owned stubs, delegated redirects and plain set
// keys. Ownership metadata lags behind /set after a path-fill, so the set keys
// are what keeps discovery from seeing an empty zone.
func (n *Node) Children(collection, parent string) (map[string]*domain.ChildEntry, error) {
	c, ok := n.collections.Get(collection)
	if !ok {
		return map[string]*domain.ChildEntry{}, nil
	}

	out := make(map[string]*domain.ChildEntry)
	for loc, ab := range c.OwnedChildren(parent) {
		out[loc] = &domain.ChildEntry{Abelian: ab}
	}

	set, _ := c.Get(parent)
	stub := func(child string) *domain.ChildEntry {
		entry := &domain.ChildEntry{Abelian: domain.NewAbelian(0, nil)}
		if set != nil {
			if ab, ok := set.Get(child); ok && ab != nil {
				entry.Abelian = ab.Clone()
			}
		}
		if contact, err := n.find(collection, child); err == nil && n.remote(contact) {
			entry.RedirectName = contact.Name()
			entry.RedirectIP = contact.IP()
			entry.RedirectPort = contact.Port()
			entry.RedirectIPs = contact.IPs()
		}
		return entry
	}

	for _, child := range c.DelegatedChildren(parent) {
		if _, exists := out[child]; !exists {
			out[child] = stub(child)
		}
	}

	if set != nil {
		for key := range set.List() {
			// Item rows are "location:id", not zone children.
			if strings.ContainsRune(key, ':') || !domain.IsDirectChild(c.Base(), parent, key) {
				continue
			}
			if _, exists := out[key]; !exists {
				out[key] = stub(key)
			}
		}
	}

	return out, nil
}

// pullDelegatedAggregate refreshes the parent stub of a delegated child from
// its owner. /aggregates answers for owned locations only, so the first peer
// that answers is the authority. prefer, when set, is tried first: right after
// a handoff the receiver is the owner while XOR still points elsewhere.
func (n *Node) pullDelegatedAggregate(collection, location string, prefer domain.Contact) bool {
	return n.pullDelegated(collection, location, prefer) == pullOK
}

// pullOutcome separates a debounced pull from a completed pull with no answer.
// Neither outcome changes authority; only an explicit membership event may
// reclaim a named mark.
type pullOutcome int

const (
	pullOK pullOutcome = iota
	pullDebounced
	pullNoOwner
)

func (n *Node) pullDelegated(collection, location string, prefer domain.Contact) pullOutcome {
	if !n.allowParentSync(collection, location) {
		return pullDebounced
	}

	peers := n.zoneCandidates(collection, location, zoneFanout, nil, prefer)
	if n.remote(prefer) {
		peers = append([]domain.Contact{prefer}, peers...)
	}

	for _, peer := range peers {
		if ab := n.askAggregates(peer, collection, []string{location})[location]; ab != nil {
			n.applyPulledAbelian(collection, location, ab)
			n.markParentSync(collection, location)
			return pullOK
		}
	}
	if len(peers) == 0 {
		return pullDebounced
	}
	return pullNoOwner
}

func (n *Node) allowParentSync(collection, location string) bool {
	n.parentSyncMu.Lock()
	defer n.parentSyncMu.Unlock()
	at, ok := n.parentSyncAt[domain.Key{Collection: collection, Location: location}]
	return !ok || time.Since(at) >= parentSyncDebounce
}

func (n *Node) markParentSync(collection, location string) {
	n.parentSyncMu.Lock()
	defer n.parentSyncMu.Unlock()
	n.parentSyncAt[domain.Key{Collection: collection, Location: location}] = time.Now()
}
