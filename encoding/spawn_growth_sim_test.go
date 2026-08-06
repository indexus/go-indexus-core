package encoding

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"
)

// Growth + successive-split simulator: items arrive into fixed micro-zones;
// when a node's exclusive load exceeds limit, it spawns via exclusive_gap and
// XOR rebalance moves zones. Tracks per-node item counts across every step.

const (
	growItemLimit   = 100_000 // INDEXUS_ITEMS_LIMIT-style
	growMaxNodes    = 8
	growMaxSplits   = 12
	growInjectSteps = 40
)

type growZone struct {
	key    []byte
	name   string
	items  int
	region int // which synthetic region fed growth
}

type growNode struct {
	id   []byte
	name string
	born int // split index when created (0 = genesis)
}

type growSnap struct {
	Step       int              `json:"step"`
	Kind       string           `json:"kind"` // inject | split
	TotalItems int              `json:"total_items"`
	Nodes      int              `json:"nodes"`
	PerNode    map[string]int   `json:"per_node"`
	Moved      map[string]int   `json:"moved,omitempty"` // to → items received this split
	Spawned    string           `json:"spawned,omitempty"`
	Requester  string           `json:"requester,omitempty"`
	PreferNear string           `json:"prefer_near,omitempty"`
	MaxNode    string           `json:"max_node"`
	MaxItems   int              `json:"max_items"`
	Imbalance  float64          `json:"imbalance"` // max/mean
}

type growReport struct {
	Strategy   string     `json:"strategy"`
	FinalNodes []string   `json:"final_nodes"`
	FinalLoad  map[string]int `json:"final_load"`
	Snaps      []growSnap `json:"snaps"`
	Splits     []growSnap `json:"splits"`
	Summary    string     `json:"summary"`
}

func TestSuccessiveSplitGrowth(t *testing.T) {
	report := runGrowthSim(t, "exclusive_gap")
	b, _ := json.MarshalIndent(report, "", "  ")
	t.Log("\n" + string(b))

	// Sanity: ended with multiple nodes and no single node holding everything.
	if len(report.FinalNodes) < 3 {
		t.Fatalf("expected ≥3 nodes after growth, got %d", len(report.FinalNodes))
	}
	total := 0
	max := 0
	for _, v := range report.FinalLoad {
		total += v
		if v > max {
			max = v
		}
	}
	if total < growItemLimit {
		t.Fatalf("expected total items ≥ limit, got %d", total)
	}
	if float64(max) > 0.65*float64(total) {
		t.Fatalf("final monopoly: max=%d / total=%d (%.0f%%)", max, total, 100*float64(max)/float64(total))
	}
	if len(report.Splits) < 2 {
		t.Fatalf("expected multiple splits, got %d", len(report.Splits))
	}
}

func runGrowthSim(t *testing.T, strategy string) growReport {
	t.Helper()

	// Genesis owner + 4 growth regions in XOR space (like A/F dichotomy cases).
	owner := farCenter(t, 0x00)
	regions := make([][]byte, 4)
	for i := 0; i < 4; i++ {
		regions[i] = farCenter(t, 0x00)
		regions[i][0] = byte(0x40 * (i + 1)) // 0x40, 0x80, 0xC0, 0x00+offset — wait 0x100 wraps
	}
	regions[3][0] = 0x20 // fourth region

	// Micro-zones: 8 per region, start near-empty.
	zones := make([]*growZone, 0, 32)
	for r, center := range regions {
		for _, n := range clusterNear(t, center, 18, 8) {
			id, _ := BASE64.Decode(n)
			zones = append(zones, &growZone{key: id, name: n, items: 50, region: r})
		}
	}

	nodes := []*growNode{{id: owner, name: BASE64.Encode(owner), born: 0}}

	snaps := make([]growSnap, 0, growInjectSteps+growMaxSplits)
	splits := make([]growSnap, 0, growMaxSplits)
	step := 0

	record := func(kind string, moved map[string]int, spawned, requester, prefer string) {
		per := assignLoads(zones, nodes)
		total, maxN, maxI, imb := loadStats(per)
		s := growSnap{
			Step: step, Kind: kind, TotalItems: total, Nodes: len(nodes),
			PerNode: cloneMap(per), Moved: moved, Spawned: spawned,
			Requester: requester, PreferNear: prefer,
			MaxNode: maxN, MaxItems: maxI, Imbalance: imb,
		}
		snaps = append(snaps, s)
		if kind == "split" {
			splits = append(splits, s)
		}
	}

	record("inject", nil, "", "", "")

	// Growth schedule: accelerate into hottest region over time, with occasional
	// secondary region bursts (skew then multi-hot — matches real mesh).
	for inject := 0; inject < growInjectSteps && len(splits) < growMaxSplits; inject++ {
		step++
		batch := 8_000 + inject*1_500 // increasing arrival rate
		primary := inject % 4
		secondary := (inject + 2) % 4
		for _, z := range zones {
			share := 0
			if z.region == primary {
				share = batch * 70 / 100 / 8
			} else if z.region == secondary {
				share = batch * 20 / 100 / 8
			} else {
				share = batch * 10 / 100 / 16
			}
			if share < 1 {
				share = 1
			}
			z.items += share
		}
		record("inject", nil, "", "", "")

		// Repeatedly split while someone is over limit (cascade like autoscale).
		for guard := 0; guard < 4 && len(nodes) < growMaxNodes && len(splits) < growMaxSplits; guard++ {
			per := assignLoads(zones, nodes)
			overIdx := -1
			overLoad := 0
			for i, n := range nodes {
				if per[n.name] > growItemLimit && per[n.name] > overLoad {
					overLoad = per[n.name]
					overIdx = i
				}
			}
			if overIdx < 0 {
				break
			}
			requester := nodes[overIdx]
			known := peerIDsExcept(nodes, overIdx)
			anchors, weights := exclusiveAnchors(requester.id, zones, nodes)
			if len(anchors) == 0 {
				t.Logf("step %d: %s over limit but no exclusive load — skip spawn", step, requester.name)
				break
			}
			var spawnedIDs [][]byte
			switch strategy {
			case "exclusive_gap":
				spawnedIDs, _ = placeExclusiveGap(requester.id, anchors, weights, known, 1)
			case "hot_prefer_near":
				spawnedIDs, _ = placeHotPreferNear(requester.id, anchors, weights, known, 1)
			default:
				spawnedIDs, _ = placeLoadSplitAvoid(requester.id, anchors, weights, known, 1)
			}
			if len(spawnedIDs) == 0 {
				t.Logf("step %d: placement returned empty for %s", step, requester.name)
				break
			}
			newID := spawnedIDs[0]
			newName := BASE64.Encode(newID)
			// Avoid exact ID collision.
			for _, n := range nodes {
				if n.name == newName {
					newName = newName + "x"
					break
				}
			}
			before := assignLoads(zones, nodes)
			nodes = append(nodes, &growNode{id: newID, name: newName, born: len(splits) + 1})
			after := assignLoads(zones, nodes)
			moved := map[string]int{}
			for name, v := range after {
				delta := v - before[name]
				if delta > 0 {
					moved[name] = delta
				}
			}
			step++
			prefer := newName
			record("split", moved, newName, requester.name, prefer)
			t.Logf("split#%d requester=%s… load=%d → spawn=%s… moved=%v",
				len(splits), trunc(requester.name), overLoad, trunc(newName), moved)
		}
	}

	final := assignLoads(zones, nodes)
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.name
	}
	total, _, maxI, imb := loadStats(final)
	summary := fmt.Sprintf(
		"%d nodes, %d items, %d splits, max=%d (%.0f%%), imbalance max/mean=%.2f",
		len(nodes), total, len(splits), maxI, 100*float64(maxI)/float64(total), imb,
	)
	return growReport{
		Strategy:   strategy,
		FinalNodes: names,
		FinalLoad:  final,
		Snaps:      snaps,
		Splits:     splits,
		Summary:    summary,
	}
}

func assignLoads(zones []*growZone, nodes []*growNode) map[string]int {
	out := make(map[string]int, len(nodes))
	for _, n := range nodes {
		out[n.name] = 0
	}
	ids := make([][]byte, len(nodes))
	for i, n := range nodes {
		ids[i] = n.id
	}
	for _, z := range zones {
		best := 0
		bestD := xorDist(z.key, ids[0])
		for i := 1; i < len(ids); i++ {
			d := xorDist(z.key, ids[i])
			if bytesLess(d, bestD) {
				bestD = d
				best = i
			}
		}
		out[nodes[best].name] += z.items
	}
	return out
}

func exclusiveAnchors(owner []byte, zones []*growZone, nodes []*growNode) ([]string, []int) {
	known := make([][]byte, 0, len(nodes))
	for _, n := range nodes {
		if bytesEqual(n.id, owner) {
			continue
		}
		known = append(known, n.id)
	}
	var anchors []string
	var weights []int
	for _, z := range zones {
		if closestPeer(z.key, owner, known) >= 0 {
			continue
		}
		// Owner currently closest → exclusive.
		anchors = append(anchors, z.name)
		w := z.items
		if w < 1 {
			w = 1
		}
		weights = append(weights, w)
	}
	return anchors, weights
}

func peerIDsExcept(nodes []*growNode, idx int) [][]byte {
	out := make([][]byte, 0, len(nodes)-1)
	for i, n := range nodes {
		if i == idx {
			continue
		}
		out = append(out, n.id)
	}
	return out
}

func loadStats(per map[string]int) (total int, maxN string, maxI int, imbalance float64) {
	if len(per) == 0 {
		return 0, "", 0, 0
	}
	for n, v := range per {
		total += v
		if v > maxI {
			maxI = v
			maxN = n
		}
	}
	mean := float64(total) / float64(len(per))
	if mean > 0 {
		imbalance = float64(maxI) / mean
	}
	return total, maxN, maxI, imbalance
}

func cloneMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func bytesLess(a, b []byte) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func trunc(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:8] + "…"
}

// Also compare hot vs exclusive on the same growth trace for the canvas.
func TestSuccessiveSplitGrowthCompare(t *testing.T) {
	ex := runGrowthSim(t, "exclusive_gap")
	hot := runGrowthSim(t, "hot_prefer_near")

	type cmpRow struct {
		Strategy string `json:"strategy"`
		Nodes    int    `json:"nodes"`
		Splits   int    `json:"splits"`
		MaxPct   float64 `json:"max_pct"`
		Imbalance float64 `json:"imbalance"`
		EmptySpawns int `json:"empty_spawns"` // splits where spawned got <5% of moved total
	}
	summarize := func(r growReport) cmpRow {
		total := 0
		max := 0
		for _, v := range r.FinalLoad {
			total += v
			if v > max {
				max = v
			}
		}
		empty := 0
		for _, s := range r.Splits {
			got := s.Moved[s.Spawned]
			movedSum := 0
			for _, v := range s.Moved {
				movedSum += v
			}
			if movedSum > 0 && float64(got) < 0.05*float64(movedSum) {
				empty++
			}
			if movedSum == 0 {
				empty++
			}
		}
		maxPct := 0.0
		if total > 0 {
			maxPct = 100 * float64(max) / float64(total)
		}
		_, _, _, imb := loadStats(r.FinalLoad)
		return cmpRow{
			Strategy: r.Strategy, Nodes: len(r.FinalNodes), Splits: len(r.Splits),
			MaxPct: maxPct, Imbalance: imb, EmptySpawns: empty,
		}
	}
	out := []cmpRow{summarize(ex), summarize(hot)}
	b, _ := json.MarshalIndent(out, "", "  ")
	t.Log("\nCOMPARE\n" + string(b))

	// Emit compact trajectory for exclusive_gap (canvas feed).
	type traj struct {
		Step int            `json:"step"`
		Kind string         `json:"kind"`
		Loads map[string]int `json:"loads"`
		Labels []string     `json:"labels"`
	}
	// Stable short labels n0, n1, …
	labelOf := map[string]string{}
	order := append([]string{}, ex.FinalNodes...)
	sort.Strings(order)
	// Prefer birth order from snaps.
	seen := []string{}
	for _, s := range ex.Snaps {
		for name := range s.PerNode {
			if _, ok := labelOf[name]; ok {
				continue
			}
			labelOf[name] = fmt.Sprintf("n%d", len(seen))
			seen = append(seen, name)
		}
	}
	trajs := make([]traj, 0, len(ex.Snaps))
	for _, s := range ex.Snaps {
		loads := map[string]int{}
		for name, v := range s.PerNode {
			loads[labelOf[name]] = v
		}
		trajs = append(trajs, traj{Step: s.Step, Kind: s.Kind, Loads: loads, Labels: seen})
	}
	splitDetail := make([]map[string]any, 0, len(ex.Splits))
	for i, s := range ex.Splits {
		movedLabeled := map[string]int{}
		for name, v := range s.Moved {
			movedLabeled[labelOf[name]] = v
		}
		splitDetail = append(splitDetail, map[string]any{
			"split":     i + 1,
			"step":      s.Step,
			"requester": labelOf[s.Requester],
			"spawned":   labelOf[s.Spawned],
			"moved":     movedLabeled,
			"per_node": func() map[string]int {
				m := map[string]int{}
				for name, v := range s.PerNode {
					m[labelOf[name]] = v
				}
				return m
			}(),
			"total": s.TotalItems,
			"imbalance": s.Imbalance,
		})
	}
	payload := map[string]any{
		"compare": out,
		"summary": ex.Summary,
		"trajectory": trajs,
		"splits": splitDetail,
		"final": func() map[string]int {
			m := map[string]int{}
			for name, v := range ex.FinalLoad {
				m[labelOf[name]] = v
			}
			return m
		}(),
	}
	pb, _ := json.MarshalIndent(payload, "", "  ")
	t.Log("\nCANVAS_PAYLOAD\n" + string(pb))
}
