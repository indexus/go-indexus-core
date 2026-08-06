package encoding

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"testing"
)

// Placement strategies compared under local knowledge only (owned anchors +
// known peer IDs). Inspired by:
//   - Kademlia XOR tree (Maymounkov/Mazières)
//   - CAN join = split with neighbor awareness (Ratnasamy)
//   - Power-of-d / cut longest interval (Byers; Naor–Wieder)
//   - Chord virtual-server load move (Rao et al., IPTPS'03)

type placeFn func(owner []byte, anchors []string, weights []int, knownPeers [][]byte, n int) ([][]byte, string)

func placeHotPreferNear(_ []byte, anchors []string, weights []int, _ [][]byte, n int) ([][]byte, string) {
	hot := heaviestAnchor(anchors, weights)
	id, _ := BASE64.Decode(hot)
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		name, err := BASE64.RandomNameNear(id, 24)
		if err != nil {
			continue
		}
		pid, _ := BASE64.Decode(name)
		out = append(out, pid)
	}
	return out, "hot_prefer_near"
}

func placeLoadSplit(_ []byte, anchors []string, weights []int, _ [][]byte, n int) ([][]byte, string) {
	names, err := BASE64.SplitLoadTargets(anchors, weights, n)
	if err != nil {
		return nil, "load_split"
	}
	return decodeNames(names), "load_split"
}

func placeLoadSplitAvoid(_ []byte, anchors []string, weights []int, known [][]byte, n int) ([][]byte, string) {
	avoid := encodeIDs(known)
	names, err := BASE64.SplitLoadTargetsAvoiding(anchors, weights, n, avoid)
	if err != nil {
		return nil, "load_split_avoid"
	}
	return decodeNames(names), "load_split_avoid"
}

// placeGapMidpoint: load-split bins, then for each bin sample candidates near
// the median and pick the one maximizing min XOR-distance to known peers
// (largest "empty branch" in the local XOR view — Kademlia empty-subtree idea
// + Byers longest-interval cut, adapted to XOR).
func placeGapMidpoint(_ []byte, anchors []string, weights []int, known [][]byte, n int) ([][]byte, string) {
	names, err := BASE64.SplitLoadTargets(anchors, weights, n)
	if err != nil {
		return nil, "gap_midpoint"
	}
	out := make([][]byte, 0, n)
	for _, nm := range names {
		med, err := BASE64.Decode(nm)
		if err != nil {
			continue
		}
		// The SplitLoadTargets name is already near the median; re-sample around
		// the *anchor* median with varying keepBits and keep farthest from peers.
		anchor := nm
		// Use the encoded name's bits as center; try several keepBits.
		best := med
		bestScore := -1
		for keep := 24; keep >= 8; keep -= 4 {
			for try := 0; try < 12; try++ {
				candName, err := BASE64.RandomNameNear(med, keep)
				if err != nil {
					continue
				}
				cand, _ := BASE64.Decode(candName)
				score := minXORInt(cand, known)
				if score > bestScore {
					bestScore = score
					best = cand
				}
			}
		}
		_ = anchor
		out = append(out, best)
	}
	return out, "gap_midpoint"
}

// placeExclusiveGap keeps only anchors for which the overloaded owner is still
// XOR-closest among known peers (local Voronoi cell — CAN/Kademlia idea), then
// load-splits and gap-places. If the hot shard is already owned by a peer,
// spawning near it cannot help; we target residual exclusive load instead.
func placeExclusiveGap(owner []byte, anchors []string, weights []int, known [][]byte, n int) ([][]byte, string) {
	var exA []string
	var exW []int
	for i, a := range anchors {
		key, err := BASE64.Decode(a)
		if err != nil {
			continue
		}
		if closestPeer(key, owner, known) >= 0 {
			continue // a known peer is already closer — not our exclusive load
		}
		exA = append(exA, a)
		exW = append(exW, weights[i])
	}
	if len(exA) == 0 {
		// Nothing exclusive left: spawning cannot relieve; return empty so caller
		// can skip scale-up (rebalance / existing peers should absorb).
		return nil, "exclusive_gap"
	}
	avoid := encodeIDs(append([][]byte{owner}, known...))
	names, err := BASE64.SplitLoadTargetsAvoiding(exA, exW, n, avoid)
	if err != nil {
		return nil, "exclusive_gap"
	}
	return decodeNames(names), "exclusive_gap"
}

// placePreferNearSplit: PreferNearSplit on exclusive anchors; reject steal/glue.
func placePreferNearSplit(owner []byte, anchors []string, weights []int, known [][]byte, n int) ([][]byte, string) {
	var exID [][]byte
	var exW []int
	var clID [][]byte
	for i, a := range anchors {
		key, err := BASE64.Decode(a)
		if err != nil {
			continue
		}
		if closestPeer(key, owner, known) >= 0 {
			clID = append(clID, key)
			continue
		}
		w := 1
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		exID = append(exID, key)
		exW = append(exW, w)
	}
	if len(exID) == 0 {
		return nil, "prefer_near_split"
	}
	avoid := append([][]byte{}, known...)
	out := make([][]byte, 0, n)
	seen := map[string]struct{}{}
	for len(out) < n {
		name, _, err := BASE64.PreferNearSplit(owner, exID, exW, clID, avoid)
		if err != nil {
			break
		}
		if _, dup := seen[name]; dup {
			break
		}
		seen[name] = struct{}{}
		id, err := BASE64.Decode(name)
		if err != nil {
			break
		}
		out = append(out, id)
		avoid = append(avoid, id)
	}
	return out, "prefer_near_split"
}

// placePowerOfTwo: two independent load-split draws; keep the set that
// relieves more owner load without colliding (firstDiff ≥ 16) with known peers.
func placePowerOfTwo(owner []byte, anchors []string, weights []int, known [][]byte, n int) ([][]byte, string) {
	type cand struct {
		peers [][]byte
		score float64
	}
	best := cand{score: -1}
	for draw := 0; draw < 2; draw++ {
		names, err := BASE64.SplitLoadTargetsAvoiding(anchors, weights, n, encodeIDs(known))
		if err != nil {
			continue
		}
		peers := decodeNames(names)
		// Score by load that lands on *new* peers, not known ones.
		all := append(append([][]byte{}, known...), peers...)
		_, perAll, total := simulateHandoff(owner, all, anchors, weights)
		perPeer := perAll[len(known):]
		relieved := 0
		for _, s := range perPeer {
			relieved += s
		}
		collisions := 0
		for _, p := range peers {
			if minLeadingDiff(p, known) >= 16 {
				collisions++
			}
		}
		// Higher relieved is good; collisions are catastrophic (empty joiner).
		score := float64(relieved) - float64(collisions)*float64(total)
		// Prefer more balanced peer shares.
		if len(perPeer) > 0 && relieved > 0 {
			maxShare := 0
			for _, s := range perPeer {
				if s > maxShare {
					maxShare = s
				}
			}
			score -= float64(maxShare) * 0.1
		}
		if score > best.score {
			best = cand{peers: peers, score: score}
		}
	}
	return best.peers, "power_of_two"
}

func heaviestAnchor(anchors []string, weights []int) string {
	best, bw := anchors[0], -1
	for i, a := range anchors {
		w := weights[i]
		if w > bw {
			bw, best = w, a
		}
	}
	return best
}

func decodeNames(names []string) [][]byte {
	out := make([][]byte, 0, len(names))
	for _, n := range names {
		id, err := BASE64.Decode(n)
		if err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

func encodeIDs(ids [][]byte) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, BASE64.Encode(id))
	}
	return out
}

func minXORInt(id []byte, peers [][]byte) int {
	if len(peers) == 0 {
		return math.MaxInt32
	}
	best := math.MaxInt32
	for _, p := range peers {
		d := xorDist(id, p)
		// Interpret first 4 bytes as big-endian distance proxy.
		v := 0
		for i := 0; i < 4 && i < len(d); i++ {
			v = (v << 8) | int(d[i])
		}
		if v < best {
			best = v
		}
	}
	return best
}

type simScenario struct {
	name       string
	build      func(t *testing.T) (owner []byte, anchors []string, weights []int, peers [][]byte)
	spawnN     int
	literature string
}

type simProps struct {
	Strategy     string
	Scenario     string
	RelievedPct  float64
	EmptyJoiner  bool // all spawn peers get <5% of total
	Collisions   int  // spawn IDs sharing ≥16 prefix bits with a known peer
	MaxPeerShare float64
	MinPeerShare float64
	Balanced     bool // each peer in [15%, 75%] of total when N≥2
}

func evalPlacement(owner []byte, anchors []string, weights []int, known, spawned [][]byte) simProps {
	// Real mesh: known peers already compete for XOR ownership. A PreferNear
	// glued to a live peer yields an empty joiner even if "relieved from owner".
	competitors := append([][]byte{}, known...)
	competitors = append(competitors, spawned...)
	kept, perAll, total := simulateHandoff(owner, competitors, anchors, weights)
	// Map perAll back: first len(known) are known, rest are spawned.
	perSpawned := perAll[len(known):]
	relievedToSpawn := 0
	for _, s := range perSpawned {
		relievedToSpawn += s
	}
	// Owner relief that actually landed on *new* nodes (not stolen by known peers).
	rp := 0.0
	if total > 0 {
		rp = 100 * float64(relievedToSpawn) / float64(total)
	}
	_ = kept
	coll := 0
	for _, s := range spawned {
		if minLeadingDiff(s, known) >= 16 {
			coll++
		}
	}
	maxS, minS := 0, math.MaxInt32
	for _, s := range perSpawned {
		if s > maxS {
			maxS = s
		}
		if s < minS {
			minS = s
		}
	}
	if len(perSpawned) == 0 {
		minS = 0
	}
	maxPct, minPct := 0.0, 0.0
	if total > 0 {
		maxPct = 100 * float64(maxS) / float64(total)
		minPct = 100 * float64(minS) / float64(total)
	}
	empty := true
	for _, s := range perSpawned {
		if float64(s) >= 0.05*float64(total) {
			empty = false
			break
		}
	}
	if len(perSpawned) == 0 {
		empty = true
	}
	balanced := len(perSpawned) >= 2 && total > 0
	for _, s := range perSpawned {
		pct := 100 * float64(s) / float64(total)
		if pct < 15 || pct > 75 {
			balanced = false
		}
	}
	return simProps{
		RelievedPct:  rp,
		EmptyJoiner:  empty,
		Collisions:   coll,
		MaxPeerShare: maxPct,
		MinPeerShare: minPct,
		Balanced:     balanced,
	}
}

func TestSpawnPlacementStrategiesMonteCarlo(t *testing.T) {
	scenarios := []simScenario{
		{
			name:       "A_opposite_clusters",
			literature: "Ideal XOR dichotomy — Kademlia empty branches both sides of owner",
			spawnN:     2,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00)
				cA, cB := farCenter(t, 0x00), farCenter(t, 0x00)
				cA[0], cB[0] = 0x80, 0xC0
				var anchors []string
				var weights []int
				for _, c := range [][]byte{cA, cB} {
					for _, n := range clusterNear(t, c, 20, 8) {
						anchors = append(anchors, n)
						weights = append(weights, 1000)
					}
				}
				// One far peer (bootstrap-like), not on hot prefixes.
				far := farCenter(t, 0x00)
				far[0] = 0x10
				return owner, anchors, weights, [][]byte{far}
			},
		},
		{
			name:       "B_hotspot_on_existing_peer",
			literature: "Screenshot thrash: PreferNear lands on live 7v0* peer → empty joiner",
			spawnN:     1,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00) // owner at 0x00…
				hot := farCenter(t, 0x00)
				hot[0] = 0x7A
				peer, _ := BASE64.Decode(mustNear(t, hot, 28))
				// Residual exclusive load near owner — rebalance cannot give
				// hot to a new spawn (peer already owns it), but cold can move.
				cold := farCenter(t, 0x00)
				cold[0] = 0x08
				var anchors []string
				var weights []int
				for _, n := range clusterNear(t, hot, 22, 12) {
					anchors = append(anchors, n)
					weights = append(weights, 900)
				}
				for _, n := range clusterNear(t, cold, 22, 12) {
					anchors = append(anchors, n)
					weights = append(weights, 100)
				}
				return owner, anchors, weights, [][]byte{peer}
			},
		},
		{
			name:       "C_uniform_zones",
			literature: "Consistent hashing baseline — uniform keys, local peers sparse",
			spawnN:     2,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00)
				var anchors []string
				var weights []int
				for i := 0; i < 24; i++ {
					n, err := BASE64.RandomName()
					if err != nil {
						t.Fatal(err)
					}
					anchors = append(anchors, n)
					weights = append(weights, 100)
				}
				sort.Strings(anchors)
				p1 := farCenter(t, 0x00)
				p1[0] = 0x55
				p2 := farCenter(t, 0x00)
				p2[0] = 0xAA
				return owner, anchors, weights, [][]byte{p1, p2}
			},
		},
		{
			name:       "D_skew_90_10",
			literature: "Rao IPTPS'03 — skewed object sizes; virtual server must move hot shard",
			spawnN:     2,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00)
				hot := farCenter(t, 0x00)
				hot[0] = 0xA0
				cold := farCenter(t, 0x00)
				cold[0] = 0x20
				var anchors []string
				var weights []int
				for _, n := range clusterNear(t, hot, 22, 10) {
					anchors = append(anchors, n)
					weights = append(weights, 900)
				}
				for _, n := range clusterNear(t, cold, 22, 10) {
					anchors = append(anchors, n)
					weights = append(weights, 100)
				}
				far := farCenter(t, 0x00)
				far[0] = 0x08
				return owner, anchors, weights, [][]byte{far}
			},
		},
		{
			name:       "E_crowded_hot_neighborhood",
			literature: "Byers power-of-d: many peers already fill hot XOR ball",
			spawnN:     1,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00)
				hot := farCenter(t, 0x00)
				hot[0] = 0x7A
				var peers [][]byte
				for i := 0; i < 4; i++ {
					n, _ := BASE64.Decode(mustNear(t, hot, 18+i%4))
					peers = append(peers, n)
				}
				cold := farCenter(t, 0x00)
				cold[0] = 0x20
				var anchors []string
				var weights []int
				for _, n := range clusterNear(t, hot, 20, 10) {
					anchors = append(anchors, n)
					weights = append(weights, 800)
				}
				for _, n := range clusterNear(t, cold, 20, 10) {
					anchors = append(anchors, n)
					weights = append(weights, 200)
				}
				return owner, anchors, weights, peers
			},
		},
		{
			name:       "F_thirds_three_clusters",
			literature: "Multi-spawn ⅓ split — Indexus N=3 dichotomy",
			spawnN:     3,
			build: func(t *testing.T) ([]byte, []string, []int, [][]byte) {
				owner := farCenter(t, 0x00)
				centers := make([][]byte, 3)
				for i := 0; i < 3; i++ {
					centers[i] = farCenter(t, 0x00)
					centers[i][0] = byte(0x40 * (i + 1))
				}
				var anchors []string
				var weights []int
				for _, c := range centers {
					for _, n := range clusterNear(t, c, 20, 6) {
						anchors = append(anchors, n)
						weights = append(weights, 500)
					}
				}
				far := farCenter(t, 0x00)
				far[0] = 0x08
				return owner, anchors, weights, [][]byte{far}
			},
		},
	}

	strategies := []placeFn{
		placeHotPreferNear,
		placeLoadSplit,
		placeLoadSplitAvoid,
		placeGapMidpoint,
		placePowerOfTwo,
		placeExclusiveGap,
		placePreferNearSplit,
	}

	const trials = 24
	type aggKey struct{ scen, strat string }
	type agg struct {
		n, empty, collTrials, balanced int
		relSum, maxSum, minSum         float64
		collSum                        int
	}
	stats := map[aggKey]*agg{}

	for _, sc := range scenarios {
		for trial := 0; trial < trials; trial++ {
			owner, anchors, weights, known := sc.build(t)
			for _, fn := range strategies {
				peers, strat := fn(owner, anchors, weights, known, sc.spawnN)
				if len(peers) == 0 {
					continue
				}
				p := evalPlacement(owner, anchors, weights, known, peers)
				k := aggKey{sc.name, strat}
				a := stats[k]
				if a == nil {
					a = &agg{}
					stats[k] = a
				}
				a.n++
				a.relSum += p.RelievedPct
				a.maxSum += p.MaxPeerShare
				a.minSum += p.MinPeerShare
				a.collSum += p.Collisions
				if p.EmptyJoiner {
					a.empty++
				}
				if p.Collisions > 0 {
					a.collTrials++
				}
				if p.Balanced {
					a.balanced++
				}
			}
		}
	}

	// Stable print for canvas ingestion / human review.
	order := []string{
		"A_opposite_clusters", "B_hotspot_on_existing_peer", "C_uniform_zones",
		"D_skew_90_10", "E_crowded_hot_neighborhood", "F_thirds_three_clusters",
	}
	strats := []string{"hot_prefer_near", "load_split", "load_split_avoid", "gap_midpoint", "power_of_two", "exclusive_gap", "prefer_near_split"}

	t.Log("SCENARIO\tSTRATEGY\trel%_avg\tempty%\tcoll%\tbalanced%\tmaxShare%\tminShare%")
	var rows []string
	for _, sn := range order {
		for _, st := range strats {
			a := stats[aggKey{sn, st}]
			if a == nil || a.n == 0 {
				continue
			}
			line := fmt.Sprintf("%s\t%s\t%.1f\t%.0f\t%.0f\t%.0f\t%.1f\t%.1f",
				sn, st,
				a.relSum/float64(a.n),
				100*float64(a.empty)/float64(a.n),
				100*float64(a.collTrials)/float64(a.n),
				100*float64(a.balanced)/float64(a.n),
				a.maxSum/float64(a.n),
				a.minSum/float64(a.n),
			)
			t.Log(line)
			rows = append(rows, line)
		}
	}

	// Soft assertions: on thrash scenario B, prefer_near_split / exclusive_gap must
	// beat hot on collisions (PreferNear glued to live peer).
	bHot := stats[aggKey{"B_hotspot_on_existing_peer", "hot_prefer_near"}]
	bLeaf := stats[aggKey{"B_hotspot_on_existing_peer", "prefer_near_split"}]
	bEx := stats[aggKey{"B_hotspot_on_existing_peer", "exclusive_gap"}]
	if bHot == nil || bLeaf == nil {
		t.Fatal("missing B scenario stats")
	}
	hotColl := float64(bHot.collTrials) / float64(bHot.n)
	leafColl := float64(bLeaf.collTrials) / float64(bLeaf.n)
	if leafColl >= hotColl && hotColl > 0.5 {
		t.Fatalf("prefer_near_split should reduce collisions on hotspot-near-peer: hot=%.0f%% leaf=%.0f%%",
			100*hotColl, 100*leafColl)
	}
	if bEx != nil {
		exColl := float64(bEx.collTrials) / float64(bEx.n)
		if exColl >= hotColl && hotColl > 0.5 {
			t.Fatalf("exclusive_gap should reduce collisions on hotspot-near-peer: hot=%.0f%% exclusive=%.0f%%",
				100*hotColl, 100*exColl)
		}
	}

	// Opposite clusters: load_split should relieve well and stay balanced often.
	aSplit := stats[aggKey{"A_opposite_clusters", "load_split"}]
	if aSplit == nil || aSplit.relSum/float64(aSplit.n) < 60 {
		t.Fatalf("opposite clusters should relieve ≥60%% with load_split, got %.1f", aSplit.relSum/float64(aSplit.n))
	}

	// prefer_near_split must not empty joiners on opposite clusters.
	aLeaf := stats[aggKey{"A_opposite_clusters", "prefer_near_split"}]
	if aLeaf == nil || aLeaf.empty > aLeaf.n/2 {
		t.Fatalf("prefer_near_split emptied too often on opposite clusters: empty=%d/%d", aLeaf.empty, aLeaf.n)
	}

	_ = rows
	_ = bytes.Compare
}

func mustNear(t *testing.T, center []byte, keep int) string {
	t.Helper()
	n, err := BASE64.RandomNameNear(center, keep)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
