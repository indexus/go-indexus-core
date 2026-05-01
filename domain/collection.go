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
	base  Encoder
	sets  map[string]*Set
	owned Ownership
	mu    *sync.Mutex
}

func NewCollection(name string, root string, base Encoder) *Collection {
	return &Collection{
		name:  name,
		base:  base,
		sets:  map[string]*Set{root: NewSet()},
		owned: map[string]Delegation{root: {}},
		mu:    &sync.Mutex{},
	}
}

func (c *Collection) Name() string {
	return c.name
}

func (c *Collection) Base() Encoder {
	return c.base
}

func (c *Collection) Allowing(location string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for child, parent := "", location; parent != ""; child, parent = parent, c.base.Parent(parent) {
		if delegation, owned := c.owned[parent]; owned {
			_, delegated := delegation[child]
			return !delegated
		}
	}
	return false
}

// OwnerOf returns the ownership key (prefix in c.owned) under which location
// is stored when Allowing(location) is true. Same walk as Allowing.
func (c *Collection) OwnerOf(location string) (owner string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for child, parent := "", location; parent != ""; child, parent = parent, c.base.Parent(parent) {
		if delegation, owned := c.owned[parent]; owned {
			_, delegated := delegation[child]
			if !delegated {
				return parent, true
			}
			return "", false
		}
	}
	return "", false
}

func (c *Collection) Browse(processOwnership func(string), processDelegation func(string, string)) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for ownership, delegations := range c.owned {
		processOwnership(ownership)
		for delegation := range delegations {
			processDelegation(ownership, delegation)
		}
	}
}

// RestoreOwned ensures location is registered in c.owned (with empty delegation
// if missing), creates a Set at location if missing, and materialises the
// parent chain of sets up to the base root so traversals and aggregations work
// after restore. Unlike Add/Complete, it never registers intermediate parents
// as owned: each parent's ownership is governed exclusively by its own
// `ownership|...` snapshot command. It also does NOT eagerly aggregate child
// abelians into the parent (the child sets are still empty at this point);
// aggregates are populated lazily by set.Incr as items are replayed.
func (c *Collection) RestoreOwned(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exist := c.sets[location]; !exist {
		c.sets[location] = NewSet()
	}
	if _, exist := c.owned[location]; !exist {
		c.owned[location] = Delegation{}
	}

	for prev := location; ; {
		parent := c.base.Parent(prev)
		if parent == "" {
			return
		}
		if _, exist := c.sets[parent]; !exist {
			c.sets[parent] = NewSet()
		}
		prev = parent
	}
}

// AppendDelegation records a structural delegation from `child`'s base parent
// to `child`. Non-destructive: it never removes anything from c.owned. Used
// only during snapshot replay to rebuild c.owned[parent][child] = nil pairs.
func (c *Collection) AppendDelegation(child string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	parent := c.base.Parent(child)
	if parent == "" {
		return
	}
	if d, ok := c.owned[parent]; ok {
		d[child] = nil
	}
}

func (c *Collection) New(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.sets[location] = NewSet()

	parent := c.base.Parent(location)

	if parent == "" {
		return
	}

	set, ok := c.sets[parent]
	if !ok {
		return
	}

	set.Put(location, c.sets[location].abelian())
}

func (c *Collection) Get(location string) (*Set, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set, ok := c.sets[location]
	return set, ok
}

// EncodeBits encodes a list of values into a compact byte slice using the specified number of bits per value.
func encodeBits(values []int, bitsPerValue int) []byte {
	var buffer uint64
	var bufferBits uint
	result := make([]byte, 0, (len(values)*bitsPerValue+7)/8)

	for _, value := range values {
		buffer = (buffer << bitsPerValue) | uint64(value)
		bufferBits += uint(bitsPerValue)

		for bufferBits >= 8 {
			byteValue := byte(buffer >> (bufferBits - 8))
			result = append(result, byteValue)
			bufferBits -= 8
			buffer &= (1 << bufferBits) - 1 // Mask remaining bits
		}
	}

	// Flush remaining bits
	for bufferBits > 0 {
		if bufferBits >= 8 {
			byteValue := byte(buffer >> (bufferBits - 8))
			result = append(result, byteValue)
			bufferBits -= 8
			buffer &= (1 << bufferBits) - 1
		} else {
			byteValue := byte(buffer << (8 - bufferBits))
			result = append(result, byteValue)
			bufferBits = 0
		}
	}

	return result
}

func (c *Collection) GetMultiple(locations []string, precision int, properties []func(*Abelian) int) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var result []byte

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
		set, exist := c.sets[location]
		if !exist {
			continue
		}

		set.mu.Lock()
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
		set.mu.Unlock()
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

	return result, nil
}

func (c *Collection) List() map[string]*Set {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string]*Set, len(c.sets))
	for k, v := range c.sets {
		out[k] = v
	}
	return out
}

func (c *Collection) Add(location string, id string, metrics []float64, delegation int) Ownership {
	c.mu.Lock()
	defer c.mu.Unlock()

	delegated := true
	for child, parent := "", location; parent != ""; child, parent = parent, c.base.Parent(parent) {
		if delegation, owned := c.owned[parent]; owned {
			_, delegated = delegation[child]
			break
		}
	}

	if delegated {
		return nil
	}

	added, areas := false, Ownership{}
	entry := fmt.Sprintf("%s:%s", location, id)
	abelian := NewAbelian(1, metrics)

	child, parent := "", location

	for len(parent) > 0 {
		child, parent = parent, c.base.Parent(parent)

		set, ok := c.sets[parent]
		if !ok {
			continue
		}

		if !added && set.Add(entry, abelian, c.base.Length()) {
			set.Shrink(c.base, c.sets, parent, c.base.Length())
		} else if added && set.Incr(child, abelian).Count() == delegation {
			areas[child] = Delegation{}
		}
		added = true
	}

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

	return areas
}

func (c *Collection) Update(sublocation string, abelian *Abelian) {
	c.mu.Lock()
	defer c.mu.Unlock()

	location := c.base.Parent(sublocation)

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
	if previous != nil {
		delta.Substract(previous)
	}

	parent, child := location, sublocation
	for {
		parent, child = c.base.Parent(parent), parent

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

		if key == root || (root != c.base.Root() && strings.Index(key, root) != 0) {
			continue
		}

		parent, previous := key, ""
		for parent != root {

			parent, previous = c.base.Parent(parent), parent

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
	// Restore replays delegation markers before shard items may have created
	// this location's *Set; skip traverse instead of panicking on nil.
	if set, ok := c.sets[location]; ok && set != nil {
		c.traverse(location, func(set string, abelian *Abelian) {
			delete(c.sets, set)
		}, func(parent, location, id string, abelian *Abelian) {
			items = append(items, &Item{Collection: c.name, Location: location, Id: id, Metrics: abelian.Metrics()})
		})
	}

	parent := c.base.Parent(location)

	_, exist := c.owned[parent]
	if exist {
		c.owned[parent][location] = nil
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
	set, ok := c.sets[parent]
	if !ok || set == nil {
		return
	}

	var total *Abelian
	set.Traverse(func(key string, abelian *Abelian) {

		if total == nil {
			total = NewAbelian(abelian.Count(), abelian.Metrics())
		}

		if abelian.Count() == 1 {
			arr := strings.Split(key, ":")
			processItem(parent, arr[0], arr[1], abelian)
			return
		}

		if key == parent {
			return
		}
		if c.sets[key] == nil {
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
	set, ok := c.sets[parent]
	if !ok || set == nil {
		return
	}

	set.Traverse(func(key string, abelian *Abelian) {
		if abelian.Count() == 1 {
			arr := strings.Split(key, ":")

			k := arr[0][:len(parent)]
			if parent != c.base.Root() {
				k = arr[0][:len(parent)+1]
			}

			if !c.browsable(parent, k) {
				processItem(parent, arr[0], arr[1], abelian)
			}
			return
		}

		if key == parent {
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
