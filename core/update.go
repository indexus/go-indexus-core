package core

import (
	"log/slog"
	"math/rand"
	"time"
)

func (n *Node) Update() error {

	if n.settings.cacheMax > 0 {
		n.cache.SetMax(n.settings.cacheMax)
	}

	refresh := n.cache.Refresh(n.settings.expiration, n.settings.cacheBeta)

	for _, collection := range n.collections.List() {
		for location := range collection.Refresh() {
			if _, exist := refresh[collection.Name()]; !exist {
				refresh[collection.Name()] = make(map[string]any)
			}
			refresh[collection.Name()][location] = nil
		}
	}

	type key struct {
		collection string
		location   string
	}
	keys := make([]key, 0)
	for collection, sets := range refresh {
		for location := range sets {
			keys = append(keys, key{collection, location})
		}
	}
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })

	for _, k := range keys {
		collection, location := k.collection, k.location

		if n.settings.updateEta > 0 && rand.Float64() < n.settings.updateEta {
			continue
		}

		contact, err := n.find(collection, location)
		if err != nil {
			slog.Debug("cache refresh has no route", "collection", collection, "location", location, "err", err)
			continue
		}
		if contact == nil || contact.Name() == n.Name() {
			continue
		}

		_, set, err := contact.Get(collection, location, 0)
		if err != nil {
			slog.Debug("cache refresh unanswered", "peer", contact.Name(), "collection", collection, "location", location, "err", err)
			continue
		}
		if set == nil {
			continue
		}

		hops := n.cache.Hops(collection, location)
		if hops < 1 {
			hops = 1
		}
		n.cache.SetHops(collection, location, set, hops)

		if c, exist := n.collections.Get(collection); exist {
			c.Update(location, set.Abelian())
		}

		time.Sleep(time.Duration(rand.Intn(5)) * time.Millisecond)
	}
	return nil
}
