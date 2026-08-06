package encoding

import (
	"testing"
)

// TestPreferNearSplitMultiNoSteal: multi-peer Range simulation on
// MergeEncodings fixtures. Bad deep-name PreferNear may empty/steal; PreferNearSplit
// must relieve the donor without stealing claimed peer load.
func TestPreferNearSplitMultiNoSteal(t *testing.T) {
	const trials = 16
	for trial := 0; trial < trials; trial++ {
		t.Run("trial", func(t *testing.T) {
			self := mustDecode(t, "quErE9A2PuvLXaey")
			wEOwn, err := MergeEncodings(BASE64, BASE64, "wE", labColl)
			if err != nil {
				t.Fatal(err)
			}
			wEPeerName, err := BASE64.RandomNameNear(wEOwn, 28+trial%4)
			if err != nil {
				t.Fatal(err)
			}
			wEPeer := mustDecode(t, wEPeerName)

			exclusive := []locWeight{
				{"7", 80000},
				{"7v", 70000},
				{"7xS", 50000},
				{"7vd", 40000},
			}
			claimed := []locWeight{
				{"wE", 60000},
				{"wE-", 25000},
			}
			exIDs, exW, exTotal := mergeLocWeights(t, exclusive)
			clIDs, clW, _ := mergeLocWeights(t, claimed)
			peers := [][]byte{wEPeer}

			// --- bad: deep fork inside donor name ---
			deep := firstVaryingBitPast(self, 24)
			badName, err := BASE64.PreferNearAtFork(self, self, deep, bitAt(self, deep)^1)
			if err != nil {
				t.Fatal(err)
			}
			bad := mustDecode(t, badName)
			bd, bk, bs := PreferNearScore(bad, self, exIDs, exW, clIDs, peers)
			badFails := bd == 0 || bd >= exTotal || bk*5 < exTotal || bs > 0 ||
				minLeadingDiff(bad, peers) >= MaxPeerPrefixBits
			t.Logf("bad=%s donate=%d keep=%d stolen=%d fails=%v", badName, bd, bk, bs, badFails)

			// --- good: PreferNearSplit ---
			goodName, cand, err := BASE64.PreferNearSplit(self, exIDs, exW, clIDs, peers)
			if err != nil {
				t.Fatalf("PreferNearSplit: %v", err)
			}
			good := mustDecode(t, goodName)
			gd, gk, gs := PreferNearScore(good, self, exIDs, exW, clIDs, peers)
			if gs > 0 {
				t.Fatalf("PreferNearSplit steals claimed zones=%d prefer=%s", gs, goodName)
			}
			if gd == 0 || gd >= exTotal || gk*5 < exTotal {
				t.Fatalf("PreferNearSplit empties/under-donates donor donate=%d keep=%d total=%d", gd, gk, exTotal)
			}
			if minLeadingDiff(good, peers) >= MaxPeerPrefixBits {
				t.Fatalf("PreferNearSplit glued to peer prefer=%s", goodName)
			}

			// Multi-node Range: among exclusive+claimed anchors, only donor load
			// may move to C; peer-owned claimed must stay with wEPeer (or not C).
			anchors := make([]string, 0, len(exIDs)+len(clIDs))
			weights := make([]int, 0, len(exIDs)+len(clIDs))
			for i, id := range exIDs {
				anchors = append(anchors, BASE64.Encode(id))
				weights = append(weights, exW[i])
			}
			for i, id := range clIDs {
				anchors = append(anchors, BASE64.Encode(id))
				weights = append(weights, clW[i])
			}
			// Competitors: known peer + joiner. Handoff from donor.
			_, per, total := simulateHandoff(self, [][]byte{wEPeer, good}, anchors, weights)
			if len(per) != 2 {
				t.Fatalf("perPeer len=%d", len(per))
			}
			toPeer, toJoin := per[0], per[1]
			// Joiner should receive exclusive relief, not all claimed.
			if toJoin == 0 {
				t.Fatalf("joiner got 0 load (empty) prefer=%s fork=%d", goodName, cand.ForkBit)
			}
			if toJoin >= total {
				t.Fatalf("joiner took everything %d/%d", toJoin, total)
			}
			// Claimed mass that was closer to wEPeer than self must not flip to joiner.
			stolenW := 0
			for i, id := range clIDs {
				if closestPeer(id, self, peers) < 0 {
					continue // not actually claimed by peer vs self
				}
				if xorCloser(id, good, wEPeer) {
					stolenW += clW[i]
				}
			}
			if stolenW > 0 {
				t.Fatalf("joiner stole claimed weight=%d from peer (toPeer=%d toJoin=%d)", stolenW, toPeer, toJoin)
			}
			t.Logf("good=%s fork=%d toPeer=%d toJoin=%d donorKeepScore=%d", goodName, cand.ForkBit, toPeer, toJoin, gk)
		})
	}
}

// TestPreferNearSplitVsDeepName: multi seeds, PreferNearSplit never empties donor.
func TestPreferNearSplitVsDeepName(t *testing.T) {
	self := mustDecode(t, "oMVJ-ie1_C-VGosR")
	rows := []locWeight{
		{"1Zk", 50000},
		{"7v", 50000},
		{"7xS", 50000},
		{"wE", 40000},
	}
	exIDs, exW, exTotal := mergeLocWeights(t, rows)
	for i := 0; i < 12; i++ {
		name, _, err := BASE64.PreferNearSplit(self, exIDs, exW, nil, nil)
		if err != nil {
			t.Fatalf("iter %d: %v", i, err)
		}
		prefer := mustDecode(t, name)
		donate, keep, _ := PreferNearScore(prefer, self, exIDs, exW, nil, nil)
		if donate == 0 || donate >= exTotal || keep*5 < exTotal {
			t.Fatalf("iter %d unsafe donate=%d keep=%d prefer=%s", i, donate, keep, name)
		}
	}
}
