package encoding

import (
	"testing"
)

type locWeight struct {
	loc string
	w   int
}

func TestBestLoadSplitForkBalancesItems(t *testing.T) {
	self, err := BASE64.Decode("oMVJ-ie1_C-VGosR")
	if err != nil {
		t.Fatal(err)
	}
	rows := []locWeight{
		{"1Zk", 50000},
		{"7v", 50000},
		{"7xS", 50000},
		{"wE", 40000},
	}
	ownIDs, weights, total := mergeRows(t, rows)
	fork, side, donate, keep, ok := BestLoadSplitFork(self, ownIDs, weights)
	if !ok {
		t.Fatal("expected a balanced fork")
	}
	if donate == 0 || donate >= total {
		t.Fatalf("donate=%d total=%d — must not empty donor", donate, total)
	}
	if keep*5 < total {
		t.Fatalf("keep=%d total=%d — donor kept <20%%", keep, total)
	}
	half := total / 2
	errAmt := donate - half
	if errAmt < 0 {
		errAmt = -errAmt
	}
	if errAmt*100/total > 35 {
		t.Fatalf("donate=%d half=%d too far from ½ (fork=%d side=%d)", donate, half, fork, side)
	}

	name, err := BASE64.PreferNearAtFork(self, ownIDs[0], fork, side)
	if err != nil {
		t.Fatal(err)
	}
	prefer, _ := BASE64.Decode(name)
	got := xorDonateWeight(ownIDs, weights, prefer, self)
	if got != donate {
		t.Fatalf("XOR donate=%d want fork estimate %d prefer=%s", got, donate, name)
	}
	t.Logf("fork=%d side=%d donate=%d keep=%d prefer=%s", fork, side, donate, keep, name)
}

// Hot node named quEr* holding mostly 7* zones: a deep fork inside the name
// (quErE vs quErV) used to look keep-heavy while XOR emptied the donor.
func TestBestLoadSplitForkHotNameDoesNotEmptyDonor(t *testing.T) {
	self, err := BASE64.Decode("quErE9A2PuvLXaey")
	if err != nil {
		t.Fatal(err)
	}
	rows := []locWeight{
		{"7", 80000},
		{"7v", 70000},
		{"7xS", 50000},
		{"7vd", 40000},
		{"qu", 5000},
		{"quE", 5000},
	}
	ownIDs, weights, total := mergeRows(t, rows)
	fork, side, donate, keep, ok := BestLoadSplitFork(self, ownIDs, weights)
	if !ok {
		t.Log("no safe fork (acceptable)")
		return
	}
	if keep*5 < total || donate == 0 || donate >= total {
		t.Fatalf("unsafe fork=%d side=%d donate=%d keep=%d total=%d", fork, side, donate, keep, total)
	}
	name, err := BASE64.PreferNearAtFork(self, heaviestOnSide(ownIDs, weights, fork, side), fork, side)
	if err != nil {
		t.Fatal(err)
	}
	prefer, _ := BASE64.Decode(name)
	got := xorDonateWeight(ownIDs, weights, prefer, self)
	if got != donate {
		t.Fatalf("XOR donate=%d != estimate %d prefer=%s fork=%d", got, donate, name, fork)
	}
	if got == 0 || got >= total {
		t.Fatalf("PreferNear would empty/donate nothing: %d/%d prefer=%s", got, total, name)
	}
	t.Logf("fork=%d donate=%d keep=%d prefer=%s", fork, donate, keep, name)
}

func mergeRows(t *testing.T, rows []locWeight) (ownIDs [][]byte, weights []int, total int) {
	t.Helper()
	const coll = "DvFMV2020idx0001"
	for _, r := range rows {
		id, err := MergeEncodings(BASE64, BASE64, r.loc, coll)
		if err != nil {
			t.Fatal(err)
		}
		ownIDs = append(ownIDs, id)
		weights = append(weights, r.w)
		total += r.w
	}
	return ownIDs, weights, total
}

func heaviestOnSide(ownIDs [][]byte, weights []int, fork int, side byte) []byte {
	best, bestW := 0, -1
	for i, id := range ownIDs {
		if bitAt(id, fork) == side && weights[i] > bestW {
			best, bestW = i, weights[i]
		}
	}
	return ownIDs[best]
}

func xorDonateWeight(ownIDs [][]byte, weights []int, prefer, self []byte) int {
	got := 0
	for i, id := range ownIDs {
		if xorCmp(id, prefer, self) < 0 {
			got += weights[i]
		}
	}
	return got
}

func xorCmp(key, a, b []byte) int {
	n := len(key)
	if len(a) < n {
		n = len(a)
	}
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		xa, xb := key[i]^a[i], key[i]^b[i]
		if xa < xb {
			return -1
		}
		if xa > xb {
			return 1
		}
	}
	return 0
}
