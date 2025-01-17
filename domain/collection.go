package domain

import (
	"encoding/binary"
	"fmt"
	"math/bits"
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

// GetMultiple retrieves and encodes data from the collection.
func (c *Collections) GetMultiple(col string, locations []string) []byte {
	var result []byte
	collection, exist := c.data[col]
	if !exist {
		return result
	}

	precision := 6
	properties := [4](func(*Abelian) int){
		func(a *Abelian) int {
			return a.count
		},
		func(a *Abelian) int {
			return int(a.metrics[2])
		},
		func(a *Abelian) int {
			return int(1_000_000 * a.metrics[3])
		},
		func(a *Abelian) int {
			return int(1_000_000 * a.metrics[4])
		},
	}

	type t struct {
		size    int
		bits    []int
		values  [][]int
		content []byte
	}

	data := make([]t, precision)
	for idx := range data {
		data[idx] = t{
			size:    0,
			bits:    make([]int, len(properties)),
			values:  make([][]int, 0),
			content: make([]byte, 0),
		}

		for v := 0; v < len(properties); v++ {
			data[idx].values = append(data[idx].values, make([]int, 0))
		}
	}

	for _, location := range locations {
		set, exist := collection.sets[location]
		if !exist {
			continue
		}

		for key, value := range set.list {
			idxColon := strings.IndexByte(key, ':')
			if idxColon >= 0 {
				key = key[:precision]
			}

			l := len(key) - 1

			data[l].size++
			data[l].content = append(data[l].content, []byte(key)...)

			for idx, property := range properties {
				p := property(value)
				data[l].values[idx] = append(data[l].values[idx], p)
				if p > data[l].bits[idx] {
					data[l].bits[idx] = p
				}
			}
		}
	}

	bitsNeeded := func(n int) int {
		if n <= 0 {
			return 0
		}
		return bits.Len(uint(n))
	}

	for i := 0; i < precision; i++ {
		if data[i].size == 0 {
			continue
		}

		header := []byte{}
		header = append(header, byte(i))

		// Encode size as four bytes (Big Endian)
		sizeBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(sizeBytes, uint32(data[i].size))
		header = append(header, sizeBytes...)

		content := data[i].content

		for j := 0; j < len(properties); j++ {
			bitCount := bitsNeeded(data[i].bits[j])
			header = append(header, byte(bitCount))
			content = append(content, encodeBits(data[i].values[j], bitCount)...)
		}

		result = append(result, header...)
		result = append(result, content...)
	}

	return result
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

func (c *Collection) Add(location, id string, metrics []float64, setLength, delegation int) Ownership {
	c.mu.Lock()
	defer c.mu.Unlock()

	added, areas := false, Ownership{}
	entry := fmt.Sprintf("%s:%s", location, id)
	abelian := NewAbelian(1, metrics)

	child, parent := "", location

	for len(parent) > 0 {
		child, parent = parent, Parent(parent)

		set, ok := c.sets[parent]
		if !ok {
			continue
		}

		if !added && set.Add(entry, abelian, setLength) {
			set.Shrink(c.sets, parent, setLength)
		} else if added && set.Incr(child, abelian).Count() == delegation {
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

func (c *Collection) Update(location, sublocation string, abelian *Abelian) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set, exist := c.sets[location]
	if !exist {
		return
	}

	previous, exist := set.Get(sublocation)
	if exist && abelian.IsEqual(previous) {
		return
	}

	set.Put(sublocation, abelian)

	delta := abelian.Clone()
	delta.Substract(previous)

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

			c.sets[parent].Put(previous, c.sets[previous].Abelian())

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
	c.traverse(location, func(set string, abelian *Abelian) {
		delete(c.sets, set)
	}, func(parent, location, id string, abelian *Abelian) {
		items = append(items, &Item{Collection: c.name, Location: location, Id: id, Metrics: abelian.Metrics()})
	})

	_, exist := c.owned[Parent(location)]
	if exist {
		c.owned[Parent(location)][location] = nil
	}

	delete(c.owned, location)

	return items, len(c.owned) == 0
}

func (c *Collection) Traverse(parent string, processSet func(string, *Abelian), processItem func(string, string, string, *Abelian)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.traverse(parent, processSet, processItem)
}

func (c *Collection) traverse(parent string, processSet func(string, *Abelian), processItem func(string, string, string, *Abelian)) {

	var total *Abelian
	c.sets[parent].Traverse(func(key string, abelian *Abelian) {

		if total == nil {
			total = NewAbelian(abelian.Count(), abelian.Metrics())
		}

		if abelian.Count() == 1 {
			arr := strings.Split(key, ":")
			processItem(parent, arr[0], arr[1], abelian)
			return
		}

		if c.browsable(parent, key) {
			c.traverse(key, processSet, processItem)
		}
	})

	processSet(parent, total)
}

func (c *Collection) Clean(parent string, processSet func(string, *Abelian), processItem func(string, string, string, *Abelian)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.clean(parent, processSet, processItem)
}

func (c *Collection) clean(parent string, processSet func(string, *Abelian), processItem func(string, string, string, *Abelian)) {

	c.sets[parent].Traverse(func(key string, abelian *Abelian) {
		if abelian.Count() == 1 {
			arr := strings.Split(key, ":")

			k := arr[0][:len(parent)]
			if parent != Root() {
				k = arr[0][:len(parent)+1]
			}

			if !c.browsable(parent, k) {
				processItem(parent, arr[0], arr[1], abelian)
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
