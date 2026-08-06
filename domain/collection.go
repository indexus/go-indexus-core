package domain

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"strings"
	"sync"
)

type Collections struct {
	mu   *sync.RWMutex
	data map[string]*Collection
}

func NewCollections() *Collections {
	return &Collections{
		mu:   &sync.RWMutex{},
		data: make(map[string]*Collection),
	}
}

func (c *Collections) Get(name string) (*Collection, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	collection, ok := c.data[name]
	return collection, ok
}

func (c *Collections) Set(collection *Collection) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.data[collection.Name()] = collection
}

// Ensure returns the named collection and creates it under the lock that looked
// it up. Two callers missing at the same time used to build one collection each
// and the later Set threw away everything the other had already stored.
func (c *Collections) Ensure(name, root string, base Encoder) *Collection {
	c.mu.Lock()
	defer c.mu.Unlock()

	collection, exist := c.data[name]
	if !exist {
		collection = NewCollection(name, root, base)
		c.data[name] = collection
	}
	return collection
}

func (c *Collections) Delete(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.data, name)
}

func (c *Collections) List() []*Collection {
	c.mu.RLock()
	defer c.mu.RUnlock()

	collections := make([]*Collection, 0)
	for _, collection := range c.data {
		collections = append(collections, collection)
	}
	return collections
}

type Collection struct {
	name       string
	base       Encoder
	sets       map[string]*Set
	owned      Ownership
	tombstones map[string]uint64 // "location:id" -> generation
	leaves     map[string]string // "location:id" -> setKey (O(1) findLeaf)
	mu         *sync.RWMutex
}

func NewCollection(name string, root string, base Encoder) *Collection {
	return &Collection{
		name:       name,
		base:       base,
		sets:       map[string]*Set{root: NewSet()},
		owned:      map[string]Delegation{root: {}},
		tombstones: make(map[string]uint64),
		leaves:     make(map[string]string),
		mu:         &sync.RWMutex{},
	}
}

func (c *Collection) Name() string {
	return c.name
}

func (c *Collection) Base() Encoder {
	return c.base
}

func (c *Collection) Allowing(location string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for child, parent := "", location; parent != ""; child, parent = parent, c.base.Parent(parent) {
		if delegation, owned := c.owned[parent]; owned {
			_, delegated := delegation[child]
			return !delegated
		}
	}
	return false
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

func (c *Collection) New(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.new(location)
}

// EnsureSet materialises a location that has no set yet. New replaces, so two
// callers racing on the same location left one of them adding to a set the
// other had already thrown away.
func (c *Collection) EnsureSet(location string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exist := c.sets[location]; exist {
		return
	}
	c.new(location)
}

func (c *Collection) new(location string) {
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
	c.mu.RLock()
	defer c.mu.RUnlock()

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
	// Encode is read-only over sets; RLock stalls writers for the whole encode.
	c.mu.RLock()
	defer c.mu.RUnlock()
	return EncodeSets(c.sets, locations, precision, properties)
}

// EncodeSets builds the /sets binary from an arbitrary location→Set map
// (local ownership, LRU path-fill, or both). Missing locations are skipped.
func EncodeSets(sets map[string]*Set, locations []string, precision int, properties []func(*Abelian) int) ([]byte, error) {
	if precision < 1 {
		return nil, nil
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
		set, exist := sets[location]
		if !exist || set == nil {
			continue
		}

		set.Traverse(func(key string, value *Abelian) {
			if idx := strings.IndexByte(key, ':'); idx >= 0 {
				key = key[:idx]
			}
			if len(key) > precision {
				key = key[:precision]
			}
			if key == "" {
				return
			}

			l := len(key) - 1
			if l < 0 || l >= precision {
				return
			}

			data[l].size++
			data[l].content = append(data[l].content, []byte(key)...)

			for idx, property := range properties {
				p := property(value)
				data[l].values[idx] = append(data[l].values[idx], p)
				if p > data[l].bits[idx] {
					data[l].bits[idx] = p
				}
			}
		})
	}

	bitsNeeded := func(n int) int {
		if n <= 0 {
			return 0
		}
		return bits.Len(uint(n))
	}

	var result []byte
	for i := 0; i < precision; i++ {
		if data[i].size == 0 {
			continue
		}

		header := []byte{byte(i)}
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

	entry := fmt.Sprintf("%s:%s", location, id)
	abelian := NewAbelian(1, metrics)

	// Idempotent / replace-with-delta when the leaf already exists.
	if setKey, previous, ok := c.findLeaf(entry); ok {
		if previous.IsEqual(abelian) {
			return Ownership{} // no-op success
		}
		c.sets[setKey].Put(entry, abelian)
		c.refreshAncestors(setKey)
		delete(c.tombstones, entry)
		return Ownership{}
	}

	// Re-add after delete: clear tombstone (gen advanced by ApplyTombstone/Remove).
	delete(c.tombstones, entry)

	added, areas := false, Ownership{}
	child, parent := "", location

	for len(parent) > 0 {
		child, parent = parent, c.base.Parent(parent)

		set, ok := c.sets[parent]
		if !ok {
			continue
		}

		if !added {
			if set.Add(entry, abelian, c.base.Length()) {
				set.Shrink(c.base, c.sets, parent, c.base.Length(), c.leaves)
			}
			if _, ok := c.leaves[entry]; !ok {
				c.leaves[entry] = parent
			}
		} else if set.Incr(child, abelian).Count() == delegation {
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

// Remove deletes a live leaf, subtracts its Abelian up the tree, and plants a tombstone.
// Returns (removed, gen, true) on success; (nil, 0, false) if the location is not owned.
// Already-tombstoned ids are a successful no-op.
func (c *Collection) Remove(location, id string) (*Abelian, uint64, bool) {
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
		return nil, 0, false
	}

	entry := fmt.Sprintf("%s:%s", location, id)
	if gen, exists := c.tombstones[entry]; exists {
		if _, _, ok := c.findLeaf(entry); !ok {
			return nil, gen, true // already deleted
		}
	}

	setKey, previous, ok := c.findLeaf(entry)
	if !ok {
		gen := c.tombstones[entry]
		if gen == 0 {
			gen = 1
		}
		c.tombstones[entry] = gen
		return nil, gen, true
	}

	c.sets[setKey].Delete(entry)
	delete(c.leaves, entry)
	c.refreshAncestors(setKey)

	gen := c.tombstones[entry] + 1
	if gen == 0 {
		gen = 1
	}
	c.tombstones[entry] = gen
	return previous, gen, true
}

// ApplyTombstone records a remote/transferred tombstone without requiring a live leaf.
func (c *Collection) ApplyTombstone(location, id string, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry := fmt.Sprintf("%s:%s", location, id)
	if setKey, _, ok := c.findLeaf(entry); ok {
		c.sets[setKey].Delete(entry)
		delete(c.leaves, entry)
		c.refreshAncestors(setKey)
	}
	if gen == 0 {
		gen = 1
	}
	if cur, ok := c.tombstones[entry]; !ok || gen > cur {
		c.tombstones[entry] = gen
	}
}

// TombstonesUnder returns tombstones whose location is under the given ownership root.
func (c *Collection) TombstonesUnder(root string) []*Item {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make([]*Item, 0)
	for entry, gen := range c.tombstones {
		arr := strings.SplitN(entry, ":", 2)
		if len(arr) != 2 {
			continue
		}
		loc, id := arr[0], arr[1]
		if root != c.base.Root() && loc != root && !strings.HasPrefix(loc, root) {
			continue
		}
		out = append(out, &Item{
			Collection: c.name,
			Location:   loc,
			Id:         id,
			Tombstone:  true,
			Gen:        gen,
		})
	}
	return out
}

// ClearTombstonesUnder drops tombstones under root (after a successful Transfer).
func (c *Collection) ClearTombstonesUnder(root string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for entry := range c.tombstones {
		arr := strings.SplitN(entry, ":", 2)
		if len(arr) != 2 {
			continue
		}
		loc := arr[0]
		if root == c.base.Root() || loc == root || strings.HasPrefix(loc, root) {
			delete(c.tombstones, entry)
		}
	}
}

// IsTombstoned reports whether location:id carries an active tombstone and no live leaf.
func (c *Collection) IsTombstoned(location, id string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry := fmt.Sprintf("%s:%s", location, id)
	if _, ok := c.tombstones[entry]; !ok {
		return false
	}
	_, _, live := c.findLeaf(entry)
	return !live
}

// findLeaf locates a count==1 entry via the leaves index. Caller must hold c.mu.
func (c *Collection) findLeaf(entry string) (setKey string, abelian *Abelian, ok bool) {
	setKey, indexed := c.leaves[entry]
	if !indexed {
		return "", nil, false
	}
	set, exists := c.sets[setKey]
	if !exists {
		delete(c.leaves, entry)
		return "", nil, false
	}
	ab, found := set.Get(entry)
	if !found || ab.Count() != 1 {
		delete(c.leaves, entry)
		return "", nil, false
	}
	return setKey, ab, true
}

// refreshAncestors recomputes aggregate entries from setKey up to the root.
func (c *Collection) refreshAncestors(setKey string) {
	child := setKey
	parent := c.base.Parent(setKey)
	for parent != "" {
		set, ok := c.sets[parent]
		if !ok {
			return
		}
		if childSet, ok := c.sets[child]; ok {
			set.Put(child, childSet.Abelian())
		}
		child, parent = parent, c.base.Parent(parent)
	}
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

			// A zone can be owned before its set exists: Add opens an area as
			// soon as the parent's running count reaches the delegation size,
			// while only Shrink materialises sets. Seed it, and leave the count
			// the parent already carries alone — recomputing the aggregate from
			// a set that has never held anything would erase it.
			child, materialised := c.sets[previous]
			if !materialised {
				child = NewSet()
				c.sets[previous] = child
			}
			if _, counted := c.sets[parent].Get(previous); materialised || !counted {
				c.sets[parent].Put(previous, child.Abelian())
			}

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
	}, func(parent, loc, id string, abelian *Abelian) {
		delete(c.leaves, fmt.Sprintf("%s:%s", loc, id))
		items = append(items, &Item{Collection: c.name, Location: loc, Id: id, Metrics: abelian.Metrics()})
	})

	// Carry tombstones so the receiving owner does not resurrect deleted ids.
	for entry, gen := range c.tombstones {
		arr := strings.SplitN(entry, ":", 2)
		if len(arr) != 2 {
			continue
		}
		loc, id := arr[0], arr[1]
		if location == c.base.Root() || loc == location || strings.HasPrefix(loc, location) {
			items = append(items, &Item{
				Collection: c.name,
				Location:   loc,
				Id:         id,
				Tombstone:  true,
				Gen:        gen,
			})
			delete(c.tombstones, entry)
		}
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

	set, exist := c.sets[parent]
	if !exist {
		return
	}

	var total *Abelian
	set.Traverse(func(key string, abelian *Abelian) {

		if total == nil {
			total = NewAbelian(abelian.Count(), abelian.Metrics())
		}

		// Items are keyed location:id, sub-sets by location alone. The count is
		// not a discriminator: deleting items can leave a sub-set holding one.
		if arr := strings.SplitN(key, ":", 2); len(arr) == 2 {
			processItem(parent, arr[0], arr[1], abelian)
			return
		}

		if c.browsable(parent, key) && c.sets[key] != nil {
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

	set, exist := c.sets[parent]
	if !exist {
		return
	}

	set.Traverse(func(key string, abelian *Abelian) {
		if arr := strings.SplitN(key, ":", 2); len(arr) == 2 {
			k := arr[0][:len(parent)]
			if parent != c.base.Root() {
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
	c.mu.RLock()
	defer c.mu.RUnlock()

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
