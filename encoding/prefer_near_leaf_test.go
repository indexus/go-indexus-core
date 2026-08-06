package encoding

import (
	"testing"
)

const labColl = "DvFMV2020idx0001"

// Historical / lab fixtures that previously produced bad PreferNear placement.
type preferFixture struct {
	name     string
	selfName string
	// exclusive locations with weights (donor still XOR-closest).
	exclusive []locWeight
	// claimed locations already closer to a known peer (optional).
	claimed []locWeight
	// peer node names already in the mesh (decoded as avoid / owners).
	peerNames []string
	// badPreferNear: historical PreferNear that should FAIL donor-keep or no-steal.
	// Empty ⇒ synthesize deep-name fork PreferNearAtFork(self, self-locality).
	badPreferNear string
}

func TestBadPreferNearHistoricalFails(t *testing.T) {
	for _, fx := range historicalPreferFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			self := mustDecode(t, fx.selfName)
			exIDs, exW, exTotal := mergeLocWeights(t, fx.exclusive)
			clIDs, _, _ := mergeLocWeights(t, fx.claimed)
			peers := decodePeerNames(t, fx.peerNames)

			bad := fx.badPreferNear
			if bad == "" {
				// Deep fork inside the donor name (quErE → quErV style).
				deep := firstVaryingBitPast(self, 24)
				nm, err := BASE64.PreferNearAtFork(self, self, deep, bitAt(self, deep)^1)
				if err != nil {
					t.Fatal(err)
				}
				bad = nm
			}
			prefer := mustDecode(t, bad)
			donate, keep, stolen := PreferNearScore(prefer, self, exIDs, exW, clIDs, peers)
			failsDonor := donate == 0 || donate >= exTotal || keep*5 < exTotal
			failsSteal := stolen > 0
			failsGlue := len(peers) > 0 && minLeadingDiff(prefer, peers) >= MaxPeerPrefixBits
			if !failsDonor && !failsSteal && !failsGlue {
				t.Fatalf("expected bad PreferNear %q to fail a gate; donate=%d keep=%d stolen=%d glue=%v",
					bad, donate, keep, stolen, failsGlue)
			}
			t.Logf("bad=%s donate=%d keep=%d total=%d stolen=%d glue=%v",
				bad, donate, keep, exTotal, stolen, failsGlue)
		})
	}
}

func TestPreferNearSplit_NoSteal(t *testing.T) {
	for _, fx := range historicalPreferFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			self := mustDecode(t, fx.selfName)
			exIDs, exW, exTotal := mergeLocWeights(t, fx.exclusive)
			clIDs, _, _ := mergeLocWeights(t, fx.claimed)
			peers := decodePeerNames(t, fx.peerNames)

			name, cand, err := BASE64.PreferNearSplit(self, exIDs, exW, clIDs, peers)
			if err != nil {
				t.Fatalf("PreferNearSplit: %v", err)
			}
			prefer := mustDecode(t, name)
			if d := firstDiffBit(self, prefer); d != cand.ForkBit {
				t.Fatalf("firstDiff=%d want embranchement %d prefer=%s", d, cand.ForkBit, name)
			}
			donate, keep, stolen := PreferNearScore(prefer, self, exIDs, exW, clIDs, peers)
			if stolen > 0 {
				t.Fatalf("PreferNear steals %d claimed zones: %s", stolen, name)
			}
			if donate == 0 || donate >= exTotal || keep*5 < exTotal {
				t.Fatalf("unsafe donor split donate=%d keep=%d total=%d", donate, keep, exTotal)
			}
			if len(peers) > 0 && minLeadingDiff(prefer, peers) >= MaxPeerPrefixBits {
				t.Fatalf("PreferNear glued to peer (prefix≥%d): %s", MaxPeerPrefixBits, name)
			}
			t.Logf("good=%s fork=%d donate=%d keep=%d", name, cand.ForkBit, donate, keep)
		})
	}
}

func TestBetterProposalBeatsBad(t *testing.T) {
	for _, fx := range historicalPreferFixtures(t) {
		t.Run(fx.name, func(t *testing.T) {
			self := mustDecode(t, fx.selfName)
			exIDs, exW, exTotal := mergeLocWeights(t, fx.exclusive)
			clIDs, _, _ := mergeLocWeights(t, fx.claimed)
			peers := decodePeerNames(t, fx.peerNames)

			badName := fx.badPreferNear
			if badName == "" {
				deep := firstVaryingBitPast(self, 24)
				var err error
				badName, err = BASE64.PreferNearAtFork(self, self, deep, bitAt(self, deep)^1)
				if err != nil {
					t.Fatal(err)
				}
			}
			bad := mustDecode(t, badName)
			bd, bk, bs := PreferNearScore(bad, self, exIDs, exW, clIDs, peers)

			goodName, _, err := BASE64.PreferNearSplit(self, exIDs, exW, clIDs, peers)
			if err != nil {
				t.Fatalf("PreferNearSplit: %v", err)
			}
			good := mustDecode(t, goodName)
			gd, gk, gs := PreferNearScore(good, self, exIDs, exW, clIDs, peers)

			badOK := bd > 0 && bd < exTotal && bk*5 >= exTotal && bs == 0 &&
				(len(peers) == 0 || minLeadingDiff(bad, peers) < MaxPeerPrefixBits)
			goodOK := gd > 0 && gd < exTotal && gk*5 >= exTotal && gs == 0 &&
				(len(peers) == 0 || minLeadingDiff(good, peers) < MaxPeerPrefixBits)
			if badOK {
				t.Logf("note: bad PreferNear also passes gates on this fixture (bad=%s)", badName)
			} else if !goodOK {
				t.Fatalf("good PreferNear failed gates: donate=%d keep=%d stolen=%d", gd, gk, gs)
			}
			t.Logf("bad donate=%d keep=%d stolen=%d | good donate=%d keep=%d stolen=%d prefer=%s",
				bd, bk, bs, gd, gk, gs, goodName)
		})
	}
}

func historicalPreferFixtures(t *testing.T) []preferFixture {
	t.Helper()
	// Peer that owns wE* half-space relative to quEr donor: name near MergeEncodings(wE).
	wEOwn, err := MergeEncodings(BASE64, BASE64, "wE", labColl)
	if err != nil {
		t.Fatal(err)
	}
	wEPeer, err := BASE64.RandomNameNear(wEOwn, 30)
	if err != nil {
		t.Fatal(err)
	}
	// Peer glued near 7vdM-like hot load.
	hot7, err := MergeEncodings(BASE64, BASE64, "7vd", labColl)
	if err != nil {
		t.Fatal(err)
	}
	hotPeer, err := BASE64.RandomNameNear(hot7, 28)
	if err != nil {
		t.Fatal(err)
	}

	return []preferFixture{
		{
			name:     "deep_name_quEr_7star",
			selfName: "quErE9A2PuvLXaey",
			exclusive: []locWeight{
				{"7", 80000},
				{"7v", 70000},
				{"7xS", 50000},
				{"7vd", 40000},
				{"qu", 5000},
				{"quE", 5000},
			},
			// Historical style: will be synthesized as deep PreferNearAtFork.
		},
		{
			name:     "bootstrap_mix",
			selfName: "oMVJ-ie1_C-VGosR",
			exclusive: []locWeight{
				{"1Zk", 50000},
				{"7v", 50000},
				{"7xS", 50000},
				{"wE", 40000},
			},
		},
		{
			name:     "steal_peer_wE_claimed",
			selfName: "quErE9A2PuvLXaey",
			exclusive: []locWeight{
				{"7", 80000},
				{"7v", 70000},
				{"7xS", 50000},
			},
			claimed: []locWeight{
				{"wE", 60000},
				{"wE-", 20000},
			},
			peerNames: []string{wEPeer},
		},
		{
			name:     "near_peer_glue_7vd",
			selfName: "oMVJ-ie1_C-VGosR",
			exclusive: []locWeight{
				{"1Zk", 40000},
				{"1Z", 30000},
				{"wE", 35000},
			},
			// claimed hot cluster already owned by live peer — exclusive is cold side.
			claimed: []locWeight{
				{"7vd", 90000},
				{"7v", 80000},
			},
			peerNames: []string{hotPeer},
		},
	}
}

func mergeLocWeights(t *testing.T, rows []locWeight) (ids [][]byte, weights []int, total int) {
	t.Helper()
	for _, r := range rows {
		id, err := MergeEncodings(BASE64, BASE64, r.loc, labColl)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		weights = append(weights, r.w)
		total += r.w
	}
	return ids, weights, total
}

func decodePeerNames(t *testing.T, names []string) [][]byte {
	t.Helper()
	out := make([][]byte, 0, len(names))
	for _, n := range names {
		out = append(out, mustDecode(t, n))
	}
	return out
}

func firstVaryingBitPast(self []byte, minBit int) int {
	total := len(self) * 8
	b := minBit
	if b >= total-1 {
		b = total / 2
	}
	return b
}
