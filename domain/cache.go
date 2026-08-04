package domain

import (
	"container/list"
	"math"
	"strings"
	"sync"
	"time"
)

type cacheEntry struct {
	set  *Set
	hops int // 0 = owner-adjacent / direct; higher = farther from owner
	last time.Time
	coll string
	loc  string
	elem *list.Element
}

type Cache struct {
	mu          *sync.Mutex
	collections map[string]map[string]*cacheEntry
	lru         *list.List // front = most recently used
	total       int
	maxEntries  int
	touchWindow time.Duration
}

func NewCache() *Cache {
	return &Cache{
		mu:          &sync.Mutex{},
		collections: make(map[string]map[string]*cacheEntry),
		lru:         list.New(),
		maxEntries:  0, // unbounded until SetMax
	}
}

func (c *Cache) SetMax(max int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxEntries = max
	c.evictLocked()
}

// SetTouchWindow overrides how long an entry counts as recently read. Zero
// keeps the default: twice the base TTL, never below 5s.
func (c *Cache) SetTouchWindow(window time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touchWindow = window
}

// Refresh drops expired entries (TTL shrinks with hops) and returns keys to re-pull.
// Nil placeholders and untouched cold entries are omitted (pull-on-read).
func (c *Cache) Refresh(base time.Duration, beta float64) map[string]map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	if beta < 0 {
		beta = 0
	}
	result := make(map[string]map[string]any)
	touchWindow := c.touchWindow
	if touchWindow <= 0 {
		touchWindow = base * 2
		if touchWindow < 5*time.Second {
			touchWindow = 5 * time.Second
		}
	}

	for collection, sets := range c.collections {
		for location, entry := range sets {
			if entry.set == nil {
				// Cold miss placeholder: do not actively refresh, just forget it
				// once nothing has asked for that location in a while.
				if time.Since(entry.last) > touchWindow {
					c.removeLocked(collection, location, entry)
				}
				continue
			}
			ttl := hopTTL(base, beta, entry.hops)
			if entry.set.Expired(ttl) {
				c.removeLocked(collection, location, entry)
				continue
			}
			// Pull-on-read: only refresh copies that saw recent traffic.
			if time.Since(entry.last) > touchWindow {
				continue
			}
			if _, ok := result[collection]; !ok {
				result[collection] = make(map[string]any)
			}
			result[collection][location] = nil
		}
		if sets, ok := c.collections[collection]; ok && len(sets) == 0 {
			delete(c.collections, collection)
		}
	}

	return result
}

func hopTTL(base time.Duration, beta float64, hops int) time.Duration {
	if hops <= 0 || beta == 0 {
		return base
	}
	div := math.Pow(1+beta, float64(hops))
	ttl := time.Duration(float64(base) / div)
	if ttl < time.Second {
		return time.Second
	}
	return ttl
}

func (c *Cache) Get(collection, location string) (*Set, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sets, exist := c.collections[collection]
	if !exist {
		return nil, false
	}

	entry, exist := sets[location]
	if !exist {
		return nil, false
	}

	c.touchLocked(entry)
	// A placeholder entry has no set: the location is known, not cached.
	if entry.set != nil {
		entry.set.Reset()
	}
	return entry.set, true
}

// Hops returns the stored hop distance for a cache entry (0 if missing).
func (c *Cache) Hops(collection, location string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	sets, exist := c.collections[collection]
	if !exist {
		return 0
	}
	entry, exist := sets[location]
	if !exist {
		return 0
	}
	return entry.hops
}

func (c *Cache) Set(collection, location string, set *Set) {
	c.SetHops(collection, location, set, 0)
}

func (c *Cache) SetHops(collection, location string, set *Set, hops int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exist := c.collections[collection]; !exist {
		c.collections[collection] = make(map[string]*cacheEntry)
	}

	// Keeping the existing entry is only right when it holds the same set the
	// caller is offering; anything else, including a placeholder either way, is a
	// new entry.
	current, exist := c.collections[collection][location]
	if !exist || set == nil || current.set == nil || current.set.Count() != set.Count() {
		if exist {
			c.removeLocked(collection, location, current)
		}
		if _, ok := c.collections[collection]; !ok {
			c.collections[collection] = make(map[string]*cacheEntry)
		}
		entry := &cacheEntry{set: set, hops: hops, last: time.Now(), coll: collection, loc: location}
		entry.elem = c.lru.PushFront(entry)
		c.collections[collection][location] = entry
		c.total++
		c.evictLocked()
		return
	}

	current.hops = hops
	c.touchLocked(current)
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

// ForgetUnder drops cached sets at location and under it in the same
// collection. Call after a successful zone Transfer so the donor does not
// keep heap-heavy Set copies of data it no longer owns.
func (c *Cache) ForgetUnder(collection, location string) {
	if c == nil || collection == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	sets, ok := c.collections[collection]
	if !ok {
		return
	}
	for loc, entry := range sets {
		if location == "" || loc == location || strings.HasPrefix(loc, location) {
			c.removeLocked(collection, loc, entry)
		}
	}
}

func (c *Cache) touchLocked(entry *cacheEntry) {
	entry.last = time.Now()
	if entry.elem != nil {
		c.lru.MoveToFront(entry.elem)
	}
}

func (c *Cache) removeLocked(coll, loc string, entry *cacheEntry) {
	if entry != nil && entry.elem != nil {
		c.lru.Remove(entry.elem)
		entry.elem = nil
	}
	if sets, ok := c.collections[coll]; ok {
		delete(sets, loc)
		if len(sets) == 0 {
			delete(c.collections, coll)
		}
	}
	if c.total > 0 {
		c.total--
	}
}

func (c *Cache) evictLocked() {
	if c.maxEntries <= 0 {
		return
	}
	for c.total > c.maxEntries {
		back := c.lru.Back()
		if back == nil {
			return
		}
		entry := back.Value.(*cacheEntry)
		c.removeLocked(entry.coll, entry.loc, entry)
	}
}
