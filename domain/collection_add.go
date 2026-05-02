package domain

// isDelegated reports whether the insert path for location is suppressed
// because it falls under an active delegation chain. Caller must hold c.mu.
func (c *Collection) isDelegated(location string) bool {
	delegated := true
	for child, parent := "", location; parent != ""; child, parent = parent, c.base.Parent(parent) {
		if delMap, owned := c.owned[parent]; owned {
			_, delegated = delMap[child]
			break
		}
	}
	return delegated
}

// walkAndAdd walks ancestor sets, performs idempotent insert at the deepest
// shard, then Incr propagation for delegation splits. Caller must hold c.mu.
func (c *Collection) walkAndAdd(location, entry string, abelian *Abelian, delegation int) Ownership {
	added, areas := false, Ownership{}
	child, parent := "", location
	for len(parent) > 0 {
		child, parent = parent, c.base.Parent(parent)
		set, ok := c.sets[parent]
		if !ok {
			continue
		}
		if !added {
			// Idempotent insert at the deepest existing shard. If the
			// entry already exists (same location:id pair was added
			// before, e.g. because a forward timed out at the sender
			// while actually being processed by the receiver and then
			// got retried), we MUST NOT propagate Incrs to the parent
			// chain — they would double-count this single physical
			// item against the @ aggregate. Return a non-nil (but
			// empty) Ownership so n.add treats the call as a successful
			// no-op rather than a delegated rejection that would walk
			// the parent chain back up and re-queue the item forever.
			fresh, full := set.AddIfAbsent(entry, abelian, c.base.Length())
			if !fresh {
				return areas
			}
			if full {
				set.Shrink(c.base, c.sets, parent, c.base.Length())
			}
		} else if set.Incr(child, abelian).Count() == delegation {
			areas[child] = Delegation{}
		}
		added = true
	}
	return areas
}

// propagateAreas links new delegation areas into existing ownership maps.
// Caller must hold c.mu.
func (c *Collection) propagateAreas(areas Ownership) {
	for area := range areas {
		parent := c.base.Parent(area)
		if parent == "" {
			continue
		}
		if _, exist := c.owned[parent]; exist {
			c.owned[parent][area] = nil
		}
		if _, exist := areas[parent]; exist {
			areas[parent][area] = nil
		}
	}
}
