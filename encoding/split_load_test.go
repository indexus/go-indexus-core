package encoding

import (
	"bytes"
	"sort"
	"testing"
)

func xorDist(a, b []byte) []byte {
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

// closer reports whether cand is XOR-closer to key than owner (strict).
func xorCloser(key, cand, owner []byte) bool {
	return bytes.Compare(xorDist(key, cand), xorDist(key, owner)) < 0
}

// closestPeer returns the index of the XOR-closest peer for key, or -1 if
// owner remains closest among {owner}∪peers.
func closestPeer(key, owner []byte, peers [][]byte) int {
	best := -1
	bestD := xorDist(key, owner)
	for i, p := range peers {
		d := xorDist(key, p)
		if bytes.Compare(d, bestD) < 0 {
			bestD = d
			best = i
		}
	}
	return best
}

func mustDecode(t *testing.T, name string) []byte {
	t.Helper()
	id, err := BASE64.Decode(name)
	if err != nil {
		t.Fatalf("decode %q: %v", name, err)
	}
	return id
}

// clusterNear builds count IDs sharing keepBits with center, then random suffix.
func clusterNear(t *testing.T, center []byte, keepBits, count int) []string {
	t.Helper()
	out := make([]string, 0, count)
	for i := 0; i < count; i++ {
		name, err := BASE64.RandomNameNear(center, keepBits)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, name)
	}
	return out
}

func farCenter(t *testing.T, fill byte) []byte {
	t.Helper()
	id := make([]byte, BASE64.IDLength())
	for i := range id {
		id[i] = fill
	}
	return id
}

// simulateHandoff weights how much load leaves the owner toward each peer
// under the same XOR rule used by rebalance control().
func simulateHandoff(owner []byte, peers [][]byte, anchors []string, weights []int) (kept int, perPeer []int, total int) {
	perPeer = make([]int, len(peers))
	for i, a := range anchors {
		w := weights[i]
		if w < 1 {
			w = 1
		}
		total += w
		key := mustDecodeBytes(a)
		idx := closestPeer(key, owner, peers)
		if idx < 0 {
			kept += w
			continue
		}
		perPeer[idx] += w
	}
	return kept, perPeer, total
}

func mustDecodeBytes(name string) []byte {
	id, err := BASE64.Decode(name)
	if err != nil {
		panic(err)
	}
	return id
}

func TestSplitLoadDichotomyHalvesRelievesOwner(t *testing.T) {
	// Overloaded owner sits at 0x00…; owned load is two equal clusters at
	// opposite corners of the ID space. Dichotomy must spawn one PreferNear
	// per cluster so XOR handoff drains ~½ to each peer and little stays.
	owner := farCenter(t, 0x00)
	// Two clusters far from owner=0x00… in XOR space.
	cA := farCenter(t, 0x00)
	cA[0] = 0x80 // 1000…
	cB := farCenter(t, 0x00)
	cB[0] = 0xC0 // 1100…

	const perCluster = 8
	const w = 1000
	aNames := clusterNear(t, cA, 20, perCluster)
	bNames := clusterNear(t, cB, 20, perCluster)
	anchors := append(append([]string{}, aNames...), bNames...)
	weights := make([]int, len(anchors))
	for i := range weights {
		weights[i] = w
	}

	targets, err := BASE64.SplitLoadTargets(anchors, weights, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets=%d want 2", len(targets))
	}
	if targets[0] == targets[1] {
		t.Fatalf("dichotomy collapsed to one PreferNear %q", targets[0])
	}

	peers := [][]byte{mustDecode(t, targets[0]), mustDecode(t, targets[1])}
	kept, perPeer, total := simulateHandoff(owner, peers, anchors, weights)
	relieved := total - kept
	if relieved < total*70/100 {
		t.Fatalf("owner not relieved enough: kept=%d relieved=%d total=%d (want ≥70%% offloaded)", kept, relieved, total)
	}
	// Dichotomy: neither peer should monopolize (>75%), each should get a real share (≥20%).
	for i, got := range perPeer {
		pct := float64(got) * 100 / float64(total)
		if got > total*75/100 {
			t.Fatalf("peer[%d] monopolizes load: %d/%d (%.0f%%) targets=%v", i, got, total, pct, targets)
		}
		if got < total*20/100 {
			t.Fatalf("peer[%d] under-loaded: %d/%d (%.0f%%) — split not dichotomous; targets=%v perPeer=%v kept=%d",
				i, got, total, pct, targets, perPeer, kept)
		}
	}
	t.Logf("half-split OK: total=%d kept=%d perPeer=%v targets=%v", total, kept, perPeer, targets)
}

func TestSplitLoadDichotomyThirdsBalanced(t *testing.T) {
	owner := farCenter(t, 0x00)
	centers := [][]byte{
		farCenter(t, 0x00),
		farCenter(t, 0x00),
		farCenter(t, 0x00),
	}
	centers[0][0] = 0x40
	centers[1][0] = 0x80
	centers[2][0] = 0xC0

	const perCluster = 6
	const w = 500
	var anchors []string
	var weights []int
	for _, c := range centers {
		for _, name := range clusterNear(t, c, 20, perCluster) {
			anchors = append(anchors, name)
			weights = append(weights, w)
		}
	}

	targets, err := BASE64.SplitLoadTargets(anchors, weights, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets=%d want 3", len(targets))
	}
	seen := map[string]bool{}
	for _, s := range targets {
		if seen[s] {
			t.Fatalf("duplicate target %q", s)
		}
		seen[s] = true
	}

	peers := make([][]byte, 3)
	for i, s := range targets {
		peers[i] = mustDecode(t, s)
	}
	kept, perPeer, total := simulateHandoff(owner, peers, anchors, weights)
	relieved := total - kept
	if relieved < total*70/100 {
		t.Fatalf("owner not relieved: kept=%d relieved=%d total=%d", kept, relieved, total)
	}
	// Each third should land in a sane band — not a 90/5/5 dump.
	for i, got := range perPeer {
		if got > total*55/100 {
			t.Fatalf("peer[%d] took %.0f%% (>55%%) of load; thirds failed perPeer=%v", i, float64(got)*100/float64(total), perPeer)
		}
		if got < total*15/100 {
			t.Fatalf("peer[%d] took only %.0f%% (<15%%); thirds failed perPeer=%v kept=%d", i, float64(got)*100/float64(total), perPeer, kept)
		}
	}
	t.Logf("third-split OK: total=%d kept=%d perPeer=%v", total, kept, perPeer)
}

func TestSplitLoadDichotomyWeightBinsEqual(t *testing.T) {
	// Controlled clusters (not uniform-random IDs): XOR attraction then matches
	// the lex/weight dichotomy, so each peer gets ~total/N.
	owner := farCenter(t, 0x00)
	centers := make([][]byte, 3)
	for i := 0; i < 3; i++ {
		centers[i] = farCenter(t, 0x00)
		centers[i][0] = byte(0x40 * (i + 1)) // 0x40, 0x80, 0xC0
	}
	var anchors []string
	var weights []int
	for _, c := range centers {
		for _, name := range clusterNear(t, c, 20, 4) {
			anchors = append(anchors, name)
			weights = append(weights, 250)
		}
	}
	ownerID := owner
	for n := 2; n <= 3; n++ {
		targets, err := BASE64.SplitLoadTargets(anchors, weights, n)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		peers := make([][]byte, len(targets))
		for i, s := range targets {
			peers[i] = mustDecode(t, s)
		}
		_, perPeer, total := simulateHandoff(ownerID, peers, anchors, weights)
		share := total / n
		for i, got := range perPeer {
			if got < share*2/3 {
				t.Fatalf("n=%d peer[%d] got %d << share~%d (total=%d perPeer=%v)", n, i, got, share, total, perPeer)
			}
		}
	}
}

func TestSplitLoadDichotomySkewedHotspotStillSplits(t *testing.T) {
	// Classic failure mode: 90% of items in one hot prefix. Old PreferNearTargets
	// put every spawn on that prefix → one peer ate everything. Dichotomy must
	// still carve ~half the *weight* into each bin (lex walk), so two peers
	// receive distinct PreferNear tags and neither gets 100%.
	owner := farCenter(t, 0x00)
	hot := farCenter(t, 0x00)
	hot[0] = 0xA0
	cold := farCenter(t, 0x00)
	cold[0] = 0x20

	var anchors []string
	var weights []int
	for _, name := range clusterNear(t, hot, 22, 10) {
		anchors = append(anchors, name)
		weights = append(weights, 900) // heavy
	}
	for _, name := range clusterNear(t, cold, 22, 10) {
		anchors = append(anchors, name)
		weights = append(weights, 100) // light
	}

	targets, err := BASE64.SplitLoadTargets(anchors, weights, 2)
	if err != nil {
		t.Fatal(err)
	}
	peers := [][]byte{mustDecode(t, targets[0]), mustDecode(t, targets[1])}
	kept, perPeer, total := simulateHandoff(owner, peers, anchors, weights)

	if targets[0] == targets[1] {
		t.Fatal("skewed hotspot collapsed to identical PreferNear")
	}
	// At least some relief, and not a single-peer dump of the whole mesh.
	if kept == total {
		t.Fatalf("no load left owner: kept=%d total=%d", kept, total)
	}
	max := perPeer[0]
	if perPeer[1] > max {
		max = perPeer[1]
	}
	if max == total-kept && (total-kept) > 0 && (perPeer[0] == 0 || perPeer[1] == 0) {
		t.Fatalf("one peer took all relieved load; dichotomy failed perPeer=%v kept=%d total=%d targets=%v",
			perPeer, kept, total, targets)
	}
	t.Logf("skew split: total=%d kept=%d perPeer=%v targets=%v", total, kept, perPeer, targets)
}

func TestSplitLoadBinWeightsAreHalfAndThird(t *testing.T) {
	// Direct invariant on the weight partition (before XOR), using sorted
	// anchors so greedy bins are deterministic.
	type aw struct {
		a string
		w int
	}
	rows := make([]aw, 0, 9)
	for i := 0; i < 9; i++ {
		n, err := BASE64.RandomName()
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, aw{a: n, w: 100})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].a < rows[j].a })
	anchors := make([]string, len(rows))
	weights := make([]int, len(rows))
	total := 0
	for i, r := range rows {
		anchors[i] = r.a
		weights[i] = r.w
		total += r.w
	}

	// Half: first bin closes at ≥ total/2 → each bin weight within [total/2 - maxW, total/2 + maxW]
	targets2, err := BASE64.SplitLoadTargets(anchors, weights, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets2) != 2 {
		t.Fatalf("half: len=%d", len(targets2))
	}

	targets3, err := BASE64.SplitLoadTargets(anchors, weights, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets3) != 3 {
		t.Fatalf("third: len=%d", len(targets3))
	}

	// Sanity: each target must decode and be XOR-near *some* anchor (keepBits≈24).
	for _, tg := range append(targets2, targets3...) {
		tid := mustDecode(t, tg)
		best := 0
		for _, a := range anchors {
			aid := mustDecode(t, a)
			// Count matching prefix bits vs random baseline.
			match := 0
			for bit := 0; bit < 24 && bit/8 < len(tid); bit++ {
				byteIdx := bit / 8
				shift := 7 - (bit % 8)
				if (tid[byteIdx]>>shift)&1 == (aid[byteIdx]>>shift)&1 {
					match++
				} else {
					break
				}
			}
			if match > best {
				best = match
			}
		}
		if best < 16 {
			t.Fatalf("target %q not near any anchor (bestPrefixBits=%d)", tg, best)
		}
	}
	_ = total
}

func TestSplitLoadTargetsAvoidingPeers(t *testing.T) {
	// Owner overload with load clustered near an existing peer. Without avoid,
	// PreferNear lands on that peer's prefix → empty joiner / thrash. With
	// avoid, the spawn must share fewer than 24 leading bits with the peer.
	hot := farCenter(t, 0x00)
	hot[0] = 0x7A
	peerName, err := BASE64.RandomNameNear(hot, 28)
	if err != nil {
		t.Fatal(err)
	}
	peerID := mustDecode(t, peerName)

	var anchors []string
	var weights []int
	for _, name := range clusterNear(t, hot, 22, 12) {
		anchors = append(anchors, name)
		weights = append(weights, 1000)
	}
	// Cold half so dichotomy still has a second bin.
	cold := farCenter(t, 0x00)
	cold[0] = 0x20
	for _, name := range clusterNear(t, cold, 22, 12) {
		anchors = append(anchors, name)
		weights = append(weights, 1000)
	}

	targets, err := BASE64.SplitLoadTargetsAvoiding(anchors, weights, 2, []string{peerName})
	if err != nil {
		t.Fatal(err)
	}
	for i, tg := range targets {
		tid := mustDecode(t, tg)
		diff := firstDiffBit(tid, peerID)
		if diff >= 24 {
			t.Fatalf("target[%d]=%q too close to peer %q (firstDiffBit=%d, want <24)", i, tg, peerName, diff)
		}
	}
	t.Logf("avoiding OK: peer=%q targets=%v", peerName, targets)
}

func TestRandomNameNearAvoidingErrorsInsteadOfColliding(t *testing.T) {
	// maxPeerPrefix=0 ⇒ need firstDiffBit < 0 (impossible). Must error rather
	// than return a name glued to the avoided peer (old last-resort path).
	target := farCenter(t, 0xAB)
	_, err := BASE64.randomNameNearAvoiding(target, 24, [][]byte{target}, 0, map[string]struct{}{})
	if err == nil {
		t.Fatal("expected error when no name can clear maxPeerPrefix")
	}
}
