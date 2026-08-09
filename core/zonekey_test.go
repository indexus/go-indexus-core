package core

import (
	"bytes"
	"testing"

	"github.com/indexus/go-indexus-core/encoding"
)

func sharedPrefixBits(a, b []byte) int {
	shared := 0
	for i := range a {
		diff := a[i] ^ b[i]
		if diff == 0 {
			shared += 8
			continue
		}
		for bit := 7; bit >= 0 && diff>>uint(bit)&1 == 0; bit-- {
			shared++
		}
		break
	}
	return shared
}

// Kademlia and Chord hash the key before routing it, which spreads load for
// free and destroys any relationship between neighbouring keys. zoneKeyID does
// not hash: it merges the location into the high bits and lets the collection
// fill the rest, so the shape of the location tree survives into the keyspace.
//
// That is what makes a subtree a contiguous XOR range, and a subtree handoff a
// single Delegate rather than a scatter of unrelated keys. It is also why the
// mesh cannot lean on hashing to balance itself and has to split on measured
// load instead.
func TestZoneKeysKeepTheLocationTreeAdjacent(t *testing.T) {
	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}
	other, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatalf("RandomName: %v", err)
	}

	key := func(collection, location string) []byte {
		t.Helper()
		id, err := zoneKeyID(collection, location)
		if err != nil {
			t.Fatalf("zoneKeyID(%s, %s): %v", collection, location, err)
		}
		return id
	}

	// A location character is six bits, so two zones sharing a two-character
	// prefix agree on at least the first twelve bits of their keys.
	const perChar = 6

	siblings := sharedPrefixBits(key(collection, "aab"), key(collection, "aac"))
	if siblings < 2*perChar {
		t.Fatalf("siblings share %d bits, want at least %d", siblings, 2*perChar)
	}

	child := sharedPrefixBits(key(collection, "aa"), key(collection, "aab"))
	if child < 2*perChar {
		t.Fatalf("a zone and its parent share %d bits, want at least %d", child, 2*perChar)
	}

	// Locality is relative: what matters is that near locations are nearer than
	// far ones, which is exactly what hashing would destroy.
	far := sharedPrefixBits(key(collection, "aab"), key(collection, "zab"))
	if siblings <= far {
		t.Fatalf("siblings share %d bits and unrelated zones %d; the location order did not survive into the keyspace",
			siblings, far)
	}

	// The collection fills the bits the location leaves, so the same location in
	// two collections stays in the same region of the tree and separates below
	// it — a node's slice is a slice of locations, not of collections.
	crossed := sharedPrefixBits(key(collection, "aab"), key(other, "aab"))
	if crossed < 3*perChar {
		t.Fatalf("the same location in two collections shares %d bits, want at least %d",
			crossed, 3*perChar)
	}

	nearer := bytes.Compare(
		xorDistance(key(collection, "aab"), key(collection, "aac")),
		xorDistance(key(collection, "aab"), key(collection, "zab")),
	)
	if nearer >= 0 {
		t.Fatal("a sibling zone is not XOR-nearer than an unrelated one")
	}
}
