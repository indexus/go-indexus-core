package domain

import (
	"math"
	"sync"
	"time"
)

type cacheEntry struct {
	set  *Set
	hops int // 0 = owner-adjacent / direct; higher = farther from owner
	last time.Time
}

type Cache struct {
	mu          *sync.Mutex
	collections map[string]map[string]*cacheEntry
	maxEntries  int
	touchWindow time.Duration
}

func NewCache() *Cache {
	return &Cache{
		mu:          &sync.Mutex{},
		collections: make(map[string]map[string]*cacheEntry),
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
			if entry == nil || entry.set == nil {
				// Cold miss placeholder: do not actively refresh.
				if entry != nil && time.Since(entry.last) > touchWindow {
					delete(sets, location)
				}
				continue
			}
			ttl := hopTTL(base, beta, entry.hops)
			if entry.set.Expired(ttl) {
				delete(sets, location)
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
		if len(sets) == 0 {
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
	if entry == nil {
		return nil, true
	}
	entry.last = time.Now()
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
	if !exist || entry == nil {
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

	current, exist := c.collections[collection][location]
	if !exist || current == nil || set == nil || current.set == nil || current.set.Count() != set.Count() {
		c.collections[collection][location] = &cacheEntry{set: set, hops: hops, last: time.Now()}
		c.evictLocked()
		return
	}
	current.hops = hops
	current.last = time.Now()
}

func (c *Cache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, sets := range c.collections {
		n += len(sets)
	}
	return n
}

func (c *Cache) evictLocked() {
	if c.maxEntries <= 0 {
		return
	}
	for {
		total := 0
		var oldestColl, oldestLoc string
		var oldest time.Time
		first := true
		for coll, sets := range c.collections {
			for loc, entry := range sets {
				total++
				t := time.Time{}
				if entry != nil {
					t = entry.last
				}
				if first || t.Before(oldest) {
					first = false
					oldest = t
					oldestColl, oldestLoc = coll, loc
				}
			}
		}
		if total <= c.maxEntries {
			return
		}
		delete(c.collections[oldestColl], oldestLoc)
		if len(c.collections[oldestColl]) == 0 {
			delete(c.collections, oldestColl)
		}
	}
}
