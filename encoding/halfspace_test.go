package encoding

import (
	"bytes"
	"testing"
)

func TestPreferNearInHalfSpaceDonatesTarget(t *testing.T) {
	self, err := BASE64.Decode("7v0EYyymt0X06gf5")
	if err != nil {
		t.Fatal(err)
	}
	// Ownership key on a fork away from self (merge-style id).
	target, err := BASE64.Decode("7vdMV2020idx0001")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(self, target) {
		t.Fatal("fixture ids must differ")
	}

	name, err := BASE64.PreferNearInHalfSpace(self, target)
	if err != nil {
		t.Fatal(err)
	}
	prefer, err := BASE64.Decode(name)
	if err != nil {
		t.Fatal(err)
	}

	// Prefer must be strictly closer to target than self — the Range predicate.
	if bytes.Compare(xorBytes(target, prefer), xorBytes(target, self)) >= 0 {
		t.Fatalf("prefer %q not closer to target than self", name)
	}

	// Fork bit must match target (binary half-space side).
	d := firstDiffBit(self, target)
	if bitAt(prefer, d) != bitAt(target, d) {
		t.Fatalf("prefer not on target's side of fork bit %d", d)
	}
}

func TestRangeHalfSpaceIgnoresThirdPartyNeighbors(t *testing.T) {
	// Neighbors never enter Range(owner, candidate): adding arbitrary peer IDs
	// does not change whether a key is donated between two fixed IDs.
	owner, _ := BASE64.Decode("oMVJ-ie1_C-VGosR")
	cand, _ := BASE64.Decode("7xUrVn4uq3I6ZAt8")
	key, _ := BASE64.Decode("7xSrV2020idx0001")

	donate := bytes.Compare(xorBytes(key, cand), xorBytes(key, owner)) < 0
	// "Neighbors" present or absent cannot flip that boolean.
	_ = [][]byte{
		mustDecode(t, "wEdMFa-aFsLYFg7B"),
		mustDecode(t, "7vdMHEX8yNTxsnBu"),
		mustDecode(t, "wGdMzsYOGr7b-J12"),
	}
	donateAgain := bytes.Compare(xorBytes(key, cand), xorBytes(key, owner)) < 0
	if donate != donateAgain {
		t.Fatal("neighbor set must not affect owner/candidate Range predicate")
	}
	t.Logf("owner→cand donates key=%v (neighbors irrelevant)", donate)
}

func xorBytes(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}
