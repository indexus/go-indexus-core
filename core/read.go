package core

import (
	"errors"
	"fmt"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// DefaultDeepHops caps recursive path-fill when the client asks deep=true
// without an explicit hop (edge ingress).
const DefaultDeepHops = 8

// Get resolves collection/location. deep=false is local/LRU only (soft
// redirect). deep=true path-fills via XOR-nearest while hop > 0.
func (n *Node) Get(collection, location string, deep bool, hop int) (domain.Contact, *domain.Set, error) {
	if !deep {
		hop = 0
	}
	return n.resolveSet(collection, location, deep, hop)
}

func (n *Node) resolveSet(collection, location string, deep bool, hop int) (domain.Contact, *domain.Set, error) {
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
			// While we still own this location, remain authoritative even if
			// XOR nearest is already the receiver (prep / dual copy).
			if n.ownsLocation(collection, location) {
				return n, set, nil
			}
			if nearest == nil || nearest.Name() == n.Name() || !contactDialable(nearest) {
				return n, set, nil
			}
			return nearest, set, nil
		}
	}

	if set, exist := n.cache.Get(collection, location); exist && set != nil {
		return nearest, set, nil
	}

	if !deep || hop <= 0 {
		n.cache.SetHops(collection, location, nil, 0)
		return nearest, nil, nil
	}

	if nearest != nil && nearest.Name() != n.Name() && contactDialable(nearest) {
		_, set, err := nearest.Get(collection, location, true, hop-1)
		if err == nil && set != nil && set.Count() > 0 {
			cacheHops := 1
			if hop <= 1 {
				cacheHops = 2
			}
			if n.settings.leafRedirect > 0 && set.Count() >= n.settings.leafRedirect {
				cacheHops++
			}
			n.cache.SetHops(collection, location, set, cacheHops)
			return nearest, set, nil
		}
	}

	n.cache.SetHops(collection, location, nil, 0)
	return nearest, nil, nil
}

// ownsLocation reports whether this node still lists an owned key that covers
// collection/location (root or prefix). Used by Get to stay authoritative
// during S3 mirror / cutover before Delegate.
func (n *Node) ownsLocation(collection, location string) bool {
	if n == nil {
		return false
	}
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

// GetMultiple encodes locations after resolving each via the same path-fill as
// Get (local → LRU → recurse when deep). Misses are omitted from the binary.
func (n *Node) GetMultiple(collection string, locations []string, precision int, properties []func(*domain.Abelian) int, deep bool, hop int) ([]byte, error) {
	if _, ok := n.collections.Get(collection); !ok {
		return nil, errors.New("no collection")
	}
	if !deep {
		hop = 0
	}

	resolved := make(map[string]*domain.Set, len(locations))
	for _, loc := range locations {
		loc = strings.TrimSpace(loc)
		if loc == "" {
			continue
		}
		_, set, err := n.resolveSet(collection, loc, deep, hop)
		if err != nil || set == nil {
			continue
		}
		resolved[loc] = set
	}
	return domain.EncodeSets(resolved, locations, precision, properties)
}
