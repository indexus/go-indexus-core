package core

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

// A model of the membership layer alone, built on the real trie: the same
// Extract that answers /neighbors, the same ExtractK that widens it, the same
// sampleFanout that sizes the random draw. Only the transport is modelled
// away, which is what makes it cheap enough to run hundreds of meshes and
// quote probabilities rather than anecdotes.
//
// One round is one Observe/Refresh pair on every node at once:
//   - routing is rebuilt as the one-per-bucket extract around self (clean),
//   - each routing peer is asked for Neighbors(self) and answers out of its
//     own table (G),
//   - with sampling on, log2(known) peers drawn without replacement each name
//     one uniformly random member of their own table (M5).
//
// `TestAChainOfNodesReachesFullKnowledgeOfTheMesh` runs the same dynamics on
// real nodes over the real RPCs and is what keeps this model honest.
type membershipModel struct {
	ids    [][]byte
	known  []map[int]bool
	rng    *rand.Rand
	k      int
	sample bool
}

// star is how the mesh actually forms: every node is spawned with the
// bootstrap's contact, and the bootstrap learns each of them as they ping it.
func newStarModel(rng *rand.Rand, size, k int, sample bool) *membershipModel {
	m := &membershipModel{
		ids:    make([][]byte, size),
		known:  make([]map[int]bool, size),
		rng:    rng,
		k:      k,
		sample: sample,
	}
	for v := range m.ids {
		// 32 bits rather than the production 96: the bucket structure is
		// identical and collisions are impossible at these sizes, and it makes
		// the trie walk three times cheaper per trial.
		id := make([]byte, 4)
		rng.Read(id)
		m.ids[v] = id
		m.known[v] = map[int]bool{}
	}
	for v := 1; v < size; v++ {
		m.known[v][0] = true
		m.known[0][v] = true
	}
	return m
}

func (m *membershipModel) table(v int) *domain.BST[int] {
	bst := domain.NewBST[int]()
	for peer := range m.known[v] {
		// Offset by one: the trie's zero value has to mean "empty slot".
		bst.Insert(0, m.ids[peer], peer+1)
	}
	return bst
}

func (m *membershipModel) neighbors(holder *domain.BST[int], target int, k int) []int {
	var buckets [160][]int
	holder.ExtractK(0, m.ids[target], k, &buckets)

	out := make([]int, 0, 160)
	for _, bucket := range buckets {
		for _, peer := range bucket {
			out = append(out, peer-1)
		}
	}
	return out
}

func (m *membershipModel) peersOf(v int) []int {
	peers := make([]int, 0, len(m.known[v]))
	for peer := range m.known[v] {
		peers = append(peers, peer)
	}
	sort.Ints(peers)
	return peers
}

func (m *membershipModel) round() bool {
	tables := make([]*domain.BST[int], len(m.ids))
	for v := range m.ids {
		tables[v] = m.table(v)
	}

	learned := make([][]int, len(m.ids))
	for v := range m.ids {
		// clean(): routing keeps one contact per bucket around our own ID.
		for _, p := range m.neighbors(tables[v], v, 1) {
			learned[v] = append(learned[v], m.neighbors(tables[p], v, m.k)...)
		}

		if !m.sample {
			continue
		}
		peers := m.peersOf(v)
		fanout := sampleFanout(len(peers))
		for i := 0; i < fanout; i++ {
			j := i + m.rng.Intn(len(peers)-i)
			peers[i], peers[j] = peers[j], peers[i]

			// Node.Random: a uniform draw from the answering peer's own table,
			// never naming the asker back.
			candidates := m.peersOf(peers[i])
			filtered := candidates[:0]
			for _, c := range candidates {
				if c != v {
					filtered = append(filtered, c)
				}
			}
			if len(filtered) > 0 {
				learned[v] = append(learned[v], filtered[m.rng.Intn(len(filtered))])
			}
		}
	}

	changed := false
	for v, list := range learned {
		for _, peer := range list {
			if peer != v && !m.known[v][peer] {
				m.known[v][peer] = true
				changed = true
			}
		}
	}
	return changed
}

// run returns the round at which every node knew every other (0 if never) and
// the worst per-node coverage reached.
func (m *membershipModel) run(maxRounds int) (int, float64) {
	for round := 1; round <= maxRounds; round++ {
		changed := m.round()
		if m.coverage() == 1 {
			return round, 1
		}
		// Gossip on its own is deterministic, so a round that changes nothing
		// is the fixed point exactly and there is nothing left to wait for.
		// Sampling is not: near the end a node is missing one peer out of
		// sixty and a barren round is ordinary luck, so it runs the budget out
		// rather than mistake patience for a plateau.
		if !changed && !m.sample {
			break
		}
	}
	return 0, m.coverage()
}

func (m *membershipModel) coverage() float64 {
	size := len(m.ids)
	worst := 1.0
	for v := range m.ids {
		if got := float64(len(m.known[v])) / float64(size-1); got < worst {
			worst = got
		}
	}
	return worst
}

type outcome struct {
	full     int
	trials   int
	rounds   []int
	coverage []float64
}

// byRound is P(every node knows every other by round r).
func (o outcome) byRound(r int) float64 {
	reached := 0
	for _, round := range o.rounds {
		if round <= r {
			reached++
		}
	}
	return float64(reached) / float64(o.trials)
}

func (o outcome) summary() string {
	line := fmt.Sprintf("full %5.1f%%", float64(o.full)/float64(o.trials)*100)
	if o.full > 0 {
		sort.Ints(o.rounds)
		line += fmt.Sprintf("  median %3d  max %3d", o.rounds[len(o.rounds)/2], o.rounds[len(o.rounds)-1])
	} else {
		line += "                     "
	}
	if len(o.coverage) > 0 {
		line += fmt.Sprintf("  the rest reach %.3f", mean(o.coverage))
	}
	return line
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	total := 0.0
	for _, x := range xs {
		total += x
	}
	return total / float64(len(xs))
}

func measure(seed int64, size, k int, sample bool, trials, maxRounds int) outcome {
	rng := rand.New(rand.NewSource(seed))
	out := outcome{trials: trials}
	for i := 0; i < trials; i++ {
		round, cover := newStarModel(rng, size, k, sample).run(maxRounds)
		if round > 0 {
			out.full++
			out.rounds = append(out.rounds, round)
			continue
		}
		out.coverage = append(out.coverage, cover)
	}
	return out
}

// The ceiling of the gossip channel, measured rather than argued. Once every
// peer knows the whole mesh, every answer to Neighbors(v) is the same set — the
// k-per-bucket extract around v taken from all of it — so this is the most a
// node can ever learn that way, however many peers it asks and however long it
// waits.
//
// Bucket 0 always holds about half the mesh and yields k of them, so the
// ceiling falls as the mesh grows for any fixed k. No amount of widening
// removes that; only a channel that does not sort by distance does.
func TestGossipChannelCeiling(t *testing.T) {
	rng := rand.New(rand.NewSource(20260808))
	ceilings := map[int]float64{}

	for _, size := range []int{16, 64, 256, 1024} {
		ids := make([][]byte, size)
		mesh := domain.NewBST[int]()
		for v := range ids {
			id := make([]byte, 4)
			rng.Read(id)
			ids[v] = id
			mesh.Insert(0, id, v+1)
		}

		line := fmt.Sprintf("N=%4d", size)
		for _, k := range []int{1, 8, 20, 64} {
			worst := 1.0
			for v := range ids {
				var buckets [160][]int
				mesh.ExtractK(0, ids[v], k, &buckets)
				reach := 0
				for _, bucket := range buckets {
					reach += len(bucket)
				}
				if got := float64(reach) / float64(size-1); got < worst {
					worst = got
				}
			}
			line += fmt.Sprintf("   k=%2d %.3f", k, worst)
			if k == gossipBucketWidth {
				ceilings[size] = worst
			}
		}
		t.Log(line)
	}

	// The width we ship has to carry a small mesh outright, or the gossip
	// answer is not doing the work the measurements picked it for.
	if got := ceilings[16]; got < 1 {
		t.Fatalf("k=%d reaches only %.3f of a 16-node mesh; the width no longer covers a small mesh",
			gossipBucketWidth, got)
	}

	// And it has to stay short of a large one, or the comment on
	// gossipBucketWidth is wrong and M5 would look optional.
	if got := ceilings[1024]; got >= 1 {
		t.Fatalf("k=%d reaches %.3f of a 1024-node mesh; widening alone would suffice and M5's rationale needs revisiting",
			gossipBucketWidth, got)
	}
}

// The headline numbers. Gossip alone is a fixed point well short of the mesh
// and no number of rounds moves it; the random sample reaches everything.
func TestMembershipConvergenceProbabilities(t *testing.T) {
	// A measuring instrument, not a regression guard: it sweeps mesh sizes and
	// bucket widths for about a minute to produce the numbers the design was
	// chosen from. `TestGossipChannelCeiling` guards the conclusion cheaply.
	if os.Getenv("INDEXUS_MEASURE") == "" {
		t.Skip("set INDEXUS_MEASURE=1 to run the membership convergence sweep (~50 s)")
	}

	const (
		trials    = 25
		maxRounds = 150
	)

	for _, size := range []int{16, 64} {
		for _, k := range []int{1, 8, 20} {
			for _, sample := range []bool{false, true} {
				got := measure(int64(size*1000+k), size, k, sample, trials, maxRounds)
				label := "gossip only     "
				if sample {
					label = "gossip + sample "
				}
				t.Logf("N=%3d k=%2d %s %s", size, k, label, got.summary())
			}
		}

		// The probability curve for the shipped configuration.
		got := measure(int64(size), size, 1, true, trials, maxRounds)
		curve := fmt.Sprintf("N=%3d k= 1 gossip + sample  P(full by round)", size)
		for _, r := range []int{5, 10, 25, 50, 100, 150} {
			curve += fmt.Sprintf("  %d:%.2f", r, got.byRound(r))
		}
		t.Log(curve)
	}
}
