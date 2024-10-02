package domain

import (
	"fmt"
	"strings"
	"sync"
)

type Collections struct {
	mu   *sync.Mutex
	data map[string]*Collection
}

func NewCollections() *Collections {
	return &Collections{
		mu:   &sync.Mutex{},
		data: make(map[string]*Collection),
	}
}

func (c *Collections) Get(name string) (*Collection, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	collection, ok := c.data[name]
	return collection, ok
}

func (c *Collections) Set(collection *Collection) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[collection.Name()] = collection
}

func (c *Collections) Delete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.data, name)
}

func (c *Collections) List() []*Collection {
	c.mu.Lock()
	defer c.mu.Unlock()

	collections := make([]*Collection, 0)
	for _, collection := range c.data {
		collections = append(collections, collection)
	}
	return collections
}

type Collection struct {
	name  string
	sets  map[string]*Set
	owned Ownership
	mu    *sync.Mutex
}

func NewCollection(name string, root string) *Collection {
	return &Collection{
		name:  name,
		sets:  map[string]*Set{root: NewSet()},
		owned: map[string]Delegation{root: {}},
		mu:    &sync.Mutex{},
	}
}

func (c *Collection) Name() string {
	return c.name
}

func (c *Collection) Allowing(location string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for child, parent := "", location; parent != ""; child, parent = parent, Parent(parent) {
		if delegation, owned := c.owned[parent]; owned {
			_, delegated := delegation[child]
			return !delegated
		}
	}
	return false
}

func (c *Collection) Browse(processOwnership func(string), processDelegation func(string, string)) {

	for ownership, delegations := range c.owned {
		processOwnership(ownership)
		for delegation := range delegations {
			processDelegation(ownership, delegation)
		}
	}
}

func (c *Collection) New(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sets[location] = NewSet()
}

func (c *Collection) Get(location string) (*Set, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set, ok := c.sets[location]
	return set, ok
}

func (c *Collection) List() map[string]*Set {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.sets
}

func (c *Collection) Add(location, id string, setLength, delegation int) Ownership {
	c.mu.Lock()
	defer c.mu.Unlock()

	added, areas := false, Ownership{}
	entry := fmt.Sprintf("%s:%s", location, id)

	child, parent := "", location

	for len(parent) > 0 {
		child, parent = parent, Parent(parent)

		set, ok := c.sets[parent]
		if !ok {
			continue
		}

		if !added && set.Add(entry, setLength) {
			set.Shrink(c.sets, parent, setLength)
		} else if added && set.Incr(child, 1) == delegation {
			areas[child] = Delegation{}
		}
		added = true
	}

	for area := range areas {
		parent := Parent(area)
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

	return areas
}

func (c *Collection) Update(location, sublocation string, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set, exist := c.sets[location]
	if !exist {
		return
	}

	previous, _ := set.Get(sublocation)
	delta := count - previous
	if delta == 0 {
		return
	}

	set.Put(sublocation, count)

	parent, child := location, sublocation
	for {
		parent, child = Parent(parent), parent

		set, ok := c.sets[parent]
		if !ok {
			return
		}

		set.Incr(child, delta)
	}
}

func (c *Collection) Own(location string, delegation map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if current, exist := c.owned[location]; exist {
		for delegated := range current {
			delegation[delegated] = nil
		}
	}
	c.owned[location] = delegation
}

func (c *Collection) Complete(root string) Ownership {
	c.mu.Lock()
	defer c.mu.Unlock()

	areas := Ownership{root: {}}
	for key := range c.owned {

		if key == root || (root != Root() && !strings.Contains(key, root)) {
			continue
		}

		parent, previous := key, ""
		for parent != root {

			parent, previous = Parent(parent), parent

			_, exist := c.sets[parent]
			if !exist {
				c.sets[parent] = NewSet()
			}

			c.sets[parent].Put(previous, c.sets[previous].Count())

			if _, exist := areas[parent]; !exist {
				areas[parent] = Delegation{}
			}
			areas[parent][previous] = nil
		}
	}
	return areas
}

func (c *Collection) Delegate(location string) ([]*Item, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	items := make([]*Item, 0)
	c.traverse(location, func(set string, count int) {
		delete(c.sets, set)
	}, func(parent, location, id string) {
		items = append(items, &Item{Collection: c.name, Location: location, Id: id})
	})

	_, exist := c.owned[Parent(location)]
	if exist {
		c.owned[Parent(location)][location] = nil
	}

	delete(c.owned, location)

	return items, len(c.owned) == 0
}

func (c *Collection) Traverse(parent string, processSet func(string, int), processItem func(string, string, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.traverse(parent, processSet, processItem)
}

func (c *Collection) traverse(parent string, processSet func(string, int), processItem func(string, string, string)) {

	total := 0
	c.sets[parent].Traverse(func(key string, count int) {

		total += count
		if count == 1 {
			arr := strings.Split(key, ":")
			processItem(parent, arr[0], arr[1])
			return
		}

		if c.browsable(parent, key) {
			c.traverse(key, processSet, processItem)
		}
	})

	processSet(parent, total)
}

func (c *Collection) Clean(parent string, processSet func(string, int), processItem func(string, string, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clean(parent, processSet, processItem)
}

func (c *Collection) clean(parent string, processSet func(string, int), processItem func(string, string, string)) {

	total := 0
	c.sets[parent].Traverse(func(key string, count int) {
		total += count

		if count == 1 {
			arr := strings.Split(key, ":")

			k := arr[0][:len(parent)]
			if parent != Root() {
				k = arr[0][:len(parent)+1]
			}

			if !c.browsable(parent, k) {
				processItem(parent, arr[0], arr[1])
			}
			return
		}

		if !c.browsable(parent, key) && c.owned[key] == nil && c.sets[key] != nil {
			c.traverse(key, processSet, processItem)
		}
	})
}

func (c *Collection) Browsable(parent string, key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.browsable(parent, key)
}

func (c *Collection) browsable(parent string, key string) bool {
	if _, exist := c.owned[parent]; !exist {
		return true
	}
	if _, exist := c.owned[parent][key]; exist {
		return false
	}
	return true
}

func (c *Collection) Refresh() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()

	result := make(map[string]any)

	for _, ownership := range c.owned {
		for delegation := range ownership {
			if _, exist := c.owned[delegation]; !exist {
				result[delegation] = nil
			}
		}
	}

	return result
}
