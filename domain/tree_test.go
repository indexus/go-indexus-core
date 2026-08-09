package domain

import (
	"bytes"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// Kademlia properties of the routing trie (docs/protocol.md §P/§R).
//
// The whole placement and rebalance story rests on three trie operations:
//   - Nearest is the true XOR minimum — find() routes every key to exactly
//     one owner, and all nodes agree on who that is.
//   - Extract fills k-bucket slots — routing[i] is the best contact sharing
//     exactly i prefix bits, so gossip reaches every distance class.
//   - Range yields exactly the candidate's XOR half-space — control() donates
//     a zone iff the candidate is strictly closer, so two correct nodes never
//     both claim a key and rebalance terminates.
//
// These tests check the trie against brute-force XOR arithmetic on random
// datasets, seeds fixed for reproducibility.

const kadIDLen = 8 // bytes; 64 bits < the 160-slot routing array

func kadIDs(seed int64, count int) [][]byte {
	rnd := rand.New(rand.NewSource(seed))
	seen := make(map[string]struct{}, count)
	ids := make([][]byte, 0, count)
	for len(ids) < count {
		id := make([]byte, kadIDLen)
		rnd.Read(id)
		if _, dup := seen[string(id)]; dup {
			continue
		}
		seen[string(id)] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func xorOf(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i]
	}
	return out
}

// prefixBits is the index of the first differing bit (== shared prefix
// length); len*8 when equal.

// prefixBits is the index of the first differing bit (== shared prefix
// length); len*8 when equal.
func prefixBits(a, b []byte) int {
	for i := 0; i < len(a); i++ {
		if x := a[i] ^ b[i]; x != 0 {
			n := i * 8
			for mask := byte(0x80); mask != 0 && x&mask == 0; mask >>= 1 {
				n++
			}
			return n
		}
	}
	return len(a) * 8
}

func bitAt(id []byte, i int) byte {
	return id[i/8] >> (7 - i%8) & 1
}

// Nearest must return the XOR minimum over the whole trie — the routing
// decision every node makes independently in find(). Distinct ids never tie
// (XOR against a fixed target is a bijection), so the answer is unique.

// Nearest must return the XOR minimum over the whole trie — the routing
// decision every node makes independently in find(). Distinct ids never tie
// (XOR against a fixed target is a bijection), so the answer is unique.
func TestNearestIsXorMinimum(t *testing.T) {
	ids := kadIDs(1, 256)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	targets := kadIDs(2, 64)
	targets = append(targets, ids[:32]...) // members: distance 0 must win

	for _, target := range targets {
		want := ids[0]
		for _, id := range ids[1:] {
			if bytes.Compare(xorOf(id, target), xorOf(want, target)) < 0 {
				want = id
			}
		}
		got := bst.Nearest(0, target)
		if !bytes.Equal(got, want) {
			t.Fatalf("Nearest(%x) = %x, brute-force XOR minimum is %x", target, got, want)
		}
	}
}

// Extract must fill routing[i] with the XOR-nearest id sharing exactly i
// prefix bits with the target — one best contact per k-bucket — and leave
// the slot empty iff that distance class has no member.

// Extract must fill routing[i] with the XOR-nearest id sharing exactly i
// prefix bits with the target — one best contact per k-bucket — and leave
// the slot empty iff that distance class has no member.
func TestExtractFillsEachBucketWithItsNearest(t *testing.T) {
	ids := kadIDs(3, 256)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	for _, target := range kadIDs(4, 16) {
		var routing [160][]byte
		bst.Extract(0, target, &routing)

		buckets := make(map[int][][]byte)
		for _, id := range ids {
			p := prefixBits(id, target)
			buckets[p] = append(buckets[p], id)
		}

		for i := 0; i < kadIDLen*8; i++ {
			members := buckets[i]
			if routing[i] == nil {
				if len(members) > 0 {
					t.Fatalf("bucket %d has %d members but Extract left it empty", i, len(members))
				}
				continue
			}
			if got := prefixBits(routing[i], target); got != i {
				t.Fatalf("routing[%d] shares %d prefix bits with the target, want exactly %d", i, got, i)
			}
			best := members[0]
			for _, id := range members[1:] {
				if bytes.Compare(xorOf(id, target), xorOf(best, target)) < 0 {
					best = id
				}
			}
			if !bytes.Equal(routing[i], best) {
				t.Fatalf("routing[%d] = %x, bucket's XOR-nearest is %x", i, routing[i], best)
			}
		}
	}
}

// ExtractK is what a peer gossips back. Each bucket must carry its k nearest
// members in distance order, not an arbitrary k: a node learning the mesh from
// these answers gets the near half of every band it cannot see, and a lookup
// reading only the head of each bucket still gets the same contact Extract
// would have given it.
func TestExtractKFillsEachBucketWithItsKNearest(t *testing.T) {
	ids := kadIDs(7, 256)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	for _, k := range []int{1, 2, 8, 20} {
		for _, target := range kadIDs(9, 8) {
			var routing [160][][]byte
			bst.ExtractK(0, target, k, &routing)

			buckets := make(map[int][][]byte)
			for _, id := range ids {
				p := prefixBits(id, target)
				buckets[p] = append(buckets[p], id)
			}

			for i := 0; i < kadIDLen*8; i++ {
				members := buckets[i]
				got := routing[i]

				want := len(members)
				if want > k {
					want = k
				}
				if len(got) != want {
					t.Fatalf("k=%d bucket %d holds %d contacts, want %d (bucket has %d members)",
						k, i, len(got), want, len(members))
				}

				sort.Slice(members, func(a, b int) bool {
					return bytes.Compare(xorOf(members[a], target), xorOf(members[b], target)) < 0
				})
				for rank, id := range got {
					if prefixBits(id, target) != i {
						t.Fatalf("k=%d routing[%d][%d] shares %d prefix bits, want exactly %d",
							k, i, rank, prefixBits(id, target), i)
					}
					if !bytes.Equal(id, members[rank]) {
						t.Fatalf("k=%d routing[%d][%d] = %x, the %d-th nearest is %x",
							k, i, rank, id, rank, members[rank])
					}
				}
			}
		}
	}
}

// The head of each widened bucket has to be exactly what the narrow Extract
// returns, or a lookup reading ExtractK would route differently from one
// reading Extract.
func TestExtractKAgreesWithExtractOnTheFirstContact(t *testing.T) {
	ids := kadIDs(11, 512)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	for _, target := range kadIDs(13, 16) {
		var narrow [160][]byte
		bst.Extract(0, target, &narrow)

		var wide [160][][]byte
		bst.ExtractK(0, target, 20, &wide)

		for i := range narrow {
			if narrow[i] == nil {
				if len(wide[i]) != 0 {
					t.Fatalf("bucket %d is empty for Extract and holds %d for ExtractK", i, len(wide[i]))
				}
				continue
			}
			if len(wide[i]) == 0 || !bytes.Equal(wide[i][0], narrow[i]) {
				t.Fatalf("bucket %d head disagrees: Extract %x, ExtractK %v", i, narrow[i], wide[i])
			}
		}
	}
}

// Range(owner, candidate) must yield exactly the ids strictly XOR-closer to
// the candidate — the half-space control() donates. Anything extra hands a
// zone to a farther node (ping-pong on the next pass); anything missing
// strands a zone on the wrong owner forever.
func TestRangeIsExactlyTheCandidateHalfSpace(t *testing.T) {
	ids := kadIDs(5, 256)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	pairs := kadIDs(6, 32)
	for p := 0; p+1 < len(pairs); p += 2 {
		owner, candidate := pairs[p], pairs[p+1]

		got := make(map[string]struct{})
		bst.Range(0, owner, candidate, make([]byte, kadIDLen), func(_ int, value []byte, _ []byte) {
			got[string(append([]byte(nil), value...))] = struct{}{}
		})

		for _, id := range ids {
			closer := bytes.Compare(xorOf(id, candidate), xorOf(id, owner)) < 0
			_, yielded := got[string(id)]
			if closer != yielded {
				t.Fatalf("owner=%x candidate=%x id=%x: closer-to-candidate=%v but yielded=%v",
					owner, candidate, id, closer, yielded)
			}
		}
		if want := countCloser(ids, owner, candidate); len(got) != want {
			t.Fatalf("Range yielded %d ids, half-space holds %d", len(got), want)
		}
	}
}

func countCloser(ids [][]byte, owner, candidate []byte) int {
	n := 0
	for _, id := range ids {
		if bytes.Compare(xorOf(id, candidate), xorOf(id, owner)) < 0 {
			n++
		}
	}
	return n
}

// A node must never donate to itself: owner == candidate has an empty
// half-space, so Range yields nothing.

// A node must never donate to itself: owner == candidate has an empty
// half-space, so Range yields nothing.
func TestRangeSelfIsEmpty(t *testing.T) {
	ids := kadIDs(7, 64)
	bst := NewBST[[]byte]()
	for _, id := range ids {
		bst.Insert(0, id, id)
	}

	self := ids[10]
	calls := 0
	bst.Range(0, self, self, make([]byte, kadIDLen), func(int, []byte, []byte) {
		calls++
	})
	if calls != 0 {
		t.Fatalf("Range(self, self) yielded %d ids, a node would donate its own zones to itself", calls)
	}
}

// Ownership routing must be zero-or-one: for any key, the set of nodes is
// partitioned so exactly one node is nearest, and Range agreements are
// antisymmetric — if A would donate k to B, B would not donate k to A.

// Ownership routing must be zero-or-one: for any key, the set of nodes is
// partitioned so exactly one node is nearest, and Range agreements are
// antisymmetric — if A would donate k to B, B would not donate k to A.
func TestRangeIsAntisymmetric(t *testing.T) {
	keys := kadIDs(8, 128)
	nodes := kadIDs(9, 8)

	for _, key := range keys {
		bst := NewBST[[]byte]()
		bst.Insert(0, key, key)

		donors := 0
		for _, a := range nodes {
			for _, b := range nodes {
				if bytes.Equal(a, b) {
					continue
				}
				aToB, bToA := false, false
				bst.Range(0, a, b, make([]byte, kadIDLen), func(int, []byte, []byte) { aToB = true })
				bst.Range(0, b, a, make([]byte, kadIDLen), func(int, []byte, []byte) { bToA = true })
				if aToB && bToA {
					t.Fatalf("key %x: %x and %x would donate to each other — transfer ping-pong", key, a, b)
				}
				if aToB {
					donors++
				}
			}
		}
		// Every node except the XOR-nearest donates toward it (directly or
		// transitively); at minimum someone donates unless a single node set.
		if donors == 0 {
			t.Fatalf("key %x: no donation edge among %d distinct nodes", key, len(nodes))
		}
	}
}

func TestBSTConcurrentAccess(t *testing.T) {
	const (
		writers = 8
		readers = 8
		ops     = 200
	)

	bst := NewBST[int]()

	keys := make([][]byte, writers*ops)
	for i := range keys {
		keys[i] = []byte{byte(i), byte(i >> 8), byte(i >> 16), byte(i >> 24)}
	}

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				bst.Insert(0, keys[w*ops+i], w*ops+i)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := make([]byte, 4)
			for i := 0; i < ops; i++ {
				_, _ = bst.Get(0, keys[i])
				_ = bst.Nearest(0, keys[i])
				bst.Traverse(0, id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}

// Reset must be observed atomically: readers may see a full or an empty tree,
// never a half-replaced one.

// Reset must be observed atomically: readers may see a full or an empty tree,
// never a half-replaced one.
func TestBSTConcurrentReset(t *testing.T) {
	const ops = 500

	bst := NewBST[int]()
	id := make([]byte, 4)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			bst.Insert(0, []byte{byte(i), 0, 0, 0}, i)
			bst.Reset()
		}
	}()

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_ = bst.Nearest(0, id)
				bst.Traverse(0, id, func(int, []byte, int) {})
			}
		}()
	}

	wg.Wait()
}
