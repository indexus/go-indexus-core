package core

import (
	"errors"
	"fmt"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Get(collection, location string, depth int) (domain.Contact, *domain.Set, error) {

	id, err := encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("key %s/%s: %w", collection, location, err)
	}

	nearest := n.registered.Nearest(0, id)

	if c, exist := n.collections.Get(collection); exist {
		set, ok := c.Get(location)
		if ok {

			if nearest == nil || nearest.Name() == n.Name() || !contactDialable(nearest) {
				return n, set, nil
			}
			return nearest, set, nil
		}
	}

	if set, exist := n.cache.Get(collection, location); exist && set != nil {
		return nearest, set, nil
	}

	if depth <= 0 {
		n.cache.SetHops(collection, location, nil, 0)
		return nearest, nil, nil
	}

	if nearest != nil && nearest.Name() != n.Name() {
		_, set, err := nearest.Get(collection, location, depth-1)
		if err == nil && set != nil && set.Count() > 0 {

			hops := 1
			if depth < 2 {
				hops = 2
			}
			if n.settings.leafRedirect > 0 && set.Count() >= n.settings.leafRedirect {
				hops++
			}
			n.cache.SetHops(collection, location, set, hops)
			return nearest, set, nil
		}
	}

	n.cache.SetHops(collection, location, nil, 0)
	return nearest, nil, nil
}

func (n *Node) GetMultiple(collection string, locations []string, precision int, properties []func(*domain.Abelian) int) ([]byte, error) {
	c, ok := n.collections.Get(collection)
	if !ok {
		return nil, errors.New("no collection")
	}

	return c.GetMultiple(locations, precision, properties)
}
