package domain

import (
	"sync"
	"time"
)

type Cache struct {
	mu          *sync.Mutex
	collections map[string]map[string]*Set
}

func NewCache() *Cache {
	return &Cache{
		mu:          &sync.Mutex{},
		collections: make(map[string]map[string]*Set),
	}
}

func (c *Cache) Refresh(expiration time.Duration) map[string]map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make(map[string]map[string]any)

	for collection, sets := range c.collections {
		result[collection] = make(map[string]any)
		for location, set := range sets {
			if set != nil && set.Expired(expiration) {
				delete(sets, location)
				continue
			}
			result[collection][location] = nil
		}
		if len(sets) == 0 {
			delete(c.collections, collection)
		}
	}

	return result
}

func (c *Cache) Get(collection, location string) (*Set, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	sets, exist := c.collections[collection]
	if !exist {
		return nil, false
	}

	set, exist := sets[location]
	if exist && set != nil {
		set.Reset()
	}

	return set, exist
}

func (c *Cache) Set(collection, location string, set *Set) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exist := c.collections[collection]; !exist {
		c.collections[collection] = make(map[string]*Set)
	}

	if current, exist := c.collections[collection][location]; !exist || current == nil || set == nil || current.Count() != set.Count() {
		c.collections[collection][location] = set
	}
}
