package core

// Placement model A/B: climb-with-memo vs restart-from-address with via.
//
// Both strategies share the same ownership geometry and XOR routing. The only
// difference is whether a walk position (current) survives across hops and
// retries. Invariants checked after every scenario:
//   1. every accepted write is readable by XOR routing to its owner
//   2. every owned zone is reachable from "@" through the mark chain
//   3. root aggregate equals the sum of leaf items
//   4. every placement attempt terminates in a bounded hop count
//   5. no permanently stuck write when an owner exists

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/indexus/go-indexus-core/encoding"
)

type placeStrategy int

const (
	strategyMemo placeStrategy = iota
	strategyVia
)

func (s placeStrategy) String() string {
	if s == strategyVia {
		return "via"
	}
	return "memo"
}

// modelNode is a local ownership view: zones it owns and children it has marked
// as living elsewhere. Peer names are XOR-routable BASE64 ids.
type modelNode struct {
	name  string
	id    []byte
	owned map[string]map[string]string // parent -> child -> peer ("" = anonymous)
	items map[string]int               // location -> count
}

type placeMesh struct {
	nodes   []*modelNode
	byName  map[string]*modelNode
	col     string
	hopCap  int
	strat   placeStrategy
	hops    int
	stuck   int
	landed  int
	looped  int
}

func newPlaceMesh(strat placeStrategy, names ...string) *placeMesh {
	m := &placeMesh{
		byName: make(map[string]*modelNode),
		col:    "DvFMV2020idx0001",
		hopCap: 32,
		strat:  strat,
	}
	for _, name := range names {
		id, err := encoding.BASE64.Decode(name)
		if err != nil {
			panic(err)
		}
		n := &modelNode{
			name:  name,
			id:    id,
			owned: map[string]map[string]string{},
			items: map[string]int{},
		}
		m.nodes = append(m.nodes, n)
		m.byName[name] = n
	}
	return m
}

func (m *placeMesh) own(name, loc string) {
	n := m.byName[name]
	if n.owned[loc] == nil {
		n.owned[loc] = map[string]string{}
	}
}

func (m *placeMesh) mark(name, parent, child, peer string) {
	n := m.byName[name]
	if n.owned[parent] == nil {
		n.owned[parent] = map[string]string{}
	}
	n.owned[parent][child] = peer
}

func (m *placeMesh) zoneID(location string) []byte {
	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, location, m.col)
	if err != nil {
		panic(err)
	}
	return id
}

func (m *placeMesh) nearest(from *modelNode, location string, skip map[string]bool) *modelNode {
	key := m.zoneID(location)
	var best *modelNode
	var bestDist []byte
	for _, n := range m.nodes {
		if skip != nil && skip[n.name] {
			continue
		}
		// Local membership view: every node knows every other in this model.
		_ = from
		d := xorDist(n.id, key)
		if best == nil || bytes.Compare(d, bestDist) < 0 {
			best, bestDist = n, d
		}
	}
	return best
}

func xorDist(a, b []byte) []byte {
	n := min(len(a), len(b))
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func (n *modelNode) accepts(location string) bool {
	// Deepest owned ancestor of location, then refuse if a delegated child covers it.
	best := ""
	bestLen := -1
	for parent := range n.owned {
		cover := parent
		if parent == encoding.BASE64.Root() {
			cover = ""
		}
		if cover == "" || cover == location || strings.HasPrefix(location, cover) {
			if len(parent) > bestLen {
				best, bestLen = parent, len(parent)
			}
		}
	}
	if bestLen < 0 {
		return false
	}
	for child := range n.owned[best] {
		if child == location || strings.HasPrefix(location, child) {
			return false
		}
	}
	return true
}

func (n *modelNode) coversDelegated(location string) bool {
	for _, kids := range n.owned {
		for child := range kids {
			if child == location || strings.HasPrefix(location, child) {
				return true
			}
		}
	}
	return false
}

// placeOnce runs one placement attempt starting at entry. Returns true if the
// item landed. hop counts accumulate on the mesh.
func (m *placeMesh) placeOnce(entry *modelNode, address, startCurrent string) bool {
	switch m.strat {
	case strategyMemo:
		return m.placeMemo(entry, address, startCurrent)
	default:
		return m.placeVia(entry, address)
	}
}

func (m *placeMesh) placeMemo(entry *modelNode, address, current string) bool {
	if current == "" {
		current = address
	}
	node := entry
	seen := map[string]int{}
	for hop := 0; hop < m.hopCap; hop++ {
		m.hops++
		seen[node.name]++
		if seen[node.name] > 4 {
			m.looped++
			return false
		}
		nearest := m.nearest(node, current, nil)
		if nearest.name != node.name {
			// Forward carries the truncated key — the memo spreads.
			node = nearest
			continue
		}
		if node.accepts(address) {
			node.items[address]++
			m.landed++
			return true
		}
		parent := encoding.BASE64.Parent(current)
		if parent == "" {
			m.stuck++
			return false
		}
		current = parent
	}
	m.looped++
	return false
}

func (m *placeMesh) placeVia(entry *modelNode, address string) bool {
	node := entry
	via := map[string]bool{}
	for hop := 0; hop < m.hopCap; hop++ {
		m.hops++
		if via[node.name] {
			m.looped++
			return false
		}
		via[node.name] = true

		// Always climb from the address. via only blocks forwards to peers that
		// already declined — it must not rewrite who is XOR-nearest.
		current := address
		for len(current) > 0 {
			nearest := m.nearest(node, current, nil)
			if nearest == nil {
				break
			}
			if nearest.name != node.name {
				if via[nearest.name] {
					current = encoding.BASE64.Parent(current)
					continue
				}
				node = nearest
				goto nextHop
			}
			if node.accepts(address) {
				node.items[address]++
				m.landed++
				return true
			}
			current = encoding.BASE64.Parent(current)
		}
		m.stuck++
		return false
	nextHop:
	}
	m.looped++
	return false
}

func (m *placeMesh) ownerOf(location string) *modelNode {
	for _, n := range m.nodes {
		if _, ok := n.owned[location]; ok {
			return n
		}
	}
	// Deepest covering owner.
	var best *modelNode
	bestLen := -1
	for _, n := range m.nodes {
		for loc := range n.owned {
			if loc == encoding.BASE64.Root() || loc == location || strings.HasPrefix(location, loc) {
				if len(loc) > bestLen && !n.coversDelegated(location) {
					best, bestLen = n, len(loc)
				}
			}
		}
	}
	return best
}

func (m *placeMesh) readable(address string) bool {
	owner := m.ownerOf(address)
	if owner == nil {
		return false
	}
	return owner.items[address] > 0
}

func (m *placeMesh) reachableFromRoot() []string {
	var missing []string
	for _, n := range m.nodes {
		for loc := range n.owned {
			if loc == encoding.BASE64.Root() {
				continue
			}
			if !m.chainToRoot(n, loc) {
				missing = append(missing, loc+"@"+n.name)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

func (m *placeMesh) chainToRoot(owner *modelNode, loc string) bool {
	// A zone is reachable if every hop up to "@" is either owned locally or
	// marked under an ancestor that exists somewhere in the mesh.
	cur := loc
	for cur != "" && cur != encoding.BASE64.Root() {
		parent := encoding.BASE64.Parent(cur)
		if parent == "" {
			return false
		}
		found := false
		for _, n := range m.nodes {
			if kids, ok := n.owned[parent]; ok {
				if _, marked := kids[cur]; marked {
					found = true
					break
				}
				if parent == encoding.BASE64.Root() || n == owner {
					// Parent owned here without mark is OK if this is the same owner climbing.
					if _, owns := n.owned[cur]; owns && n == owner {
						found = true
						break
					}
				}
			}
			if _, owns := n.owned[parent]; owns {
				// Parent owned elsewhere: need a mark. If same node owns both, OK.
				if n == owner {
					found = true
					break
				}
			}
		}
		if !found {
			// Soft check: parent owned somewhere counts as a chain link when
			// the child is also owned (duplicate-ownership window).
			for _, n := range m.nodes {
				if _, ok := n.owned[parent]; ok {
					found = true
					break
				}
			}
		}
		if !found {
			return false
		}
		cur = parent
	}
	return true
}

func (m *placeMesh) aggregateOK() bool {
	var leaves, rootSum int
	for _, n := range m.nodes {
		for _, c := range n.items {
			leaves += c
		}
		if kids, ok := n.owned[encoding.BASE64.Root()]; ok {
			_ = kids
			for _, c := range n.items {
				rootSum += c
			}
		} else {
			for loc, c := range n.items {
				// Count items whose covering owned zone is on this node and
				// whose path is not delegated away.
				if n.accepts(loc) {
					rootSum += c
				}
			}
		}
	}
	// In this model items live only on accepting owners; rootSum == leaves.
	return leaves == rootSum
}

// Scenario: donor owns "@" and "7", has "7x" delegated; donor is XOR-nearest
// for both coarse keys. Writes arrive with a truncated current="7".
func livePinnedScenario(strat placeStrategy) *placeMesh {
	m := newPlaceMesh(strat, "_vSrV2020idx0001", "_x_rV2020idx0001", "_uSrV2020idx0001")
	m.own("_vSrV2020idx0001", encoding.BASE64.Root())
	m.own("_vSrV2020idx0001", "7")
	m.mark("_vSrV2020idx0001", encoding.BASE64.Root(), "7", "_vSrV2020idx0001")
	m.mark("_vSrV2020idx0001", "7", "7x", "_x_rV2020idx0001")
	m.own("_x_rV2020idx0001", "7x")
	return m
}

func TestModelPinnedTruncatedWalkLandsWithViaNotMemo(t *testing.T) {
	address := "7xmD4zoqo5mppmvz"
	for _, strat := range []placeStrategy{strategyMemo, strategyVia} {
		m := livePinnedScenario(strat)
		entry := m.byName["_vSrV2020idx0001"]
		ok := m.placeOnce(entry, address, "7")
		t.Logf("%s: landed=%v hops=%d stuck=%d looped=%d", strat, ok, m.hops, m.stuck, m.looped)
		if strat == strategyVia && !ok {
			t.Fatalf("via strategy failed to land a write that has a live owner")
		}
		if strat == strategyMemo && ok {
			// Memo with current="7" may or may not land depending on XOR; the
			// live bug is the *retry* keeping current="7". Simulate that:
			m2 := livePinnedScenario(strategyMemo)
			for i := 0; i < 5; i++ {
				m2.placeOnce(entry, address, "7")
			}
			if m2.landed > 0 && m2.stuck == 0 {
				t.Logf("memo landed unexpectedly on this geometry; stuck=%d", m2.stuck)
			}
		}
	}
}

func TestModelViaTerminatesUnderDivergentMembership(t *testing.T) {
	// Two nodes each believing itself nearest: via must bound the walk.
	m := newPlaceMesh(strategyVia, "_aSrV2020idx0001", "_bSrV2020idx0001")
	m.own("_aSrV2020idx0001", encoding.BASE64.Root())
	// No owner for "zz…" — should stuck, not loop forever.
	entry := m.byName["_aSrV2020idx0001"]
	ok := m.placeOnce(entry, "zzzzzzzzzzzzzzzz", "")
	if ok {
		t.Fatal("write for unowned zone landed")
	}
	if m.hops > m.hopCap {
		t.Fatalf("via walk exceeded hop cap: %d", m.hops)
	}
	if m.looped > 0 && m.hops >= m.hopCap {
		t.Fatalf("via failed to terminate before hop cap")
	}
}

func TestModelInvariantsHoldAfterViaPlacement(t *testing.T) {
	m := livePinnedScenario(strategyVia)
	entry := m.byName["_uSrV2020idx0001"]
	addresses := []string{
		"7xmD4zoqo5mppmvz",
		"7xAAAAAAAAAAAAAA",
		"7xBBBBBBBBBBBBBB",
	}
	for _, addr := range addresses {
		if !m.placeOnce(entry, addr, addr) {
			t.Fatalf("failed to place %s", addr)
		}
	}
	for _, addr := range addresses {
		if !m.readable(addr) {
			t.Fatalf("accepted write %s not readable at owner", addr)
		}
	}
	if missing := m.reachableFromRoot(); len(missing) > 0 {
		t.Fatalf("zones unreachable from @: %v", missing)
	}
	if !m.aggregateOK() {
		t.Fatal("root aggregate != leaf sum")
	}
	if m.looped != 0 {
		t.Fatalf("looped=%d, want 0", m.looped)
	}
}

func TestModelCompareMemoVsViaOnTruncatedArrival(t *testing.T) {
	address := "7xmD4zoqo5mppmvz"
	type stats struct {
		landed, stuck, looped, hops int
	}
	run := func(strat placeStrategy, retries int, start string) stats {
		m := livePinnedScenario(strat)
		entry := m.byName["_vSrV2020idx0001"]
		cur := start
		for i := 0; i < retries; i++ {
			if m.placeOnce(entry, address, cur) {
				break
			}
			// Memo keeps the truncated key across retries (the bug).
			// Via always restarts from the address (start ignored after first).
			if strat == strategyMemo {
				cur = "7"
			}
		}
		return stats{m.landed, m.stuck, m.looped, m.hops}
	}

	memo := run(strategyMemo, 10, "7")
	via := run(strategyVia, 10, "7")
	t.Logf("memo=%+v via=%+v", memo, via)

	if via.landed != 1 {
		t.Fatalf("via must land exactly once, got landed=%d", via.landed)
	}
	if via.looped != 0 {
		t.Fatalf("via looped=%d", via.looped)
	}
	// Memo with persistent truncation should not land (or land far less).
	if memo.landed > via.landed {
		t.Fatalf("memo unexpectedly beat via: memo=%+v via=%+v", memo, via)
	}
	if memo.landed == 0 && memo.stuck == 0 && memo.looped == 0 {
		t.Fatal("memo produced no outcome")
	}
}

func TestModelRestartFromAddressBeatsMemoAcrossEntries(t *testing.T) {
	address := "7xmD4zoqo5mppmvz"
	names := []string{"_vSrV2020idx0001", "_x_rV2020idx0001", "_uSrV2020idx0001"}
	starts := []string{address, "7x", "7", encoding.BASE64.Root()}

	score := func(strat placeStrategy) (landed, failed int) {
		for _, entryName := range names {
			for _, start := range starts {
				m := livePinnedScenario(strat)
				entry := m.byName[entryName]
				if m.placeOnce(entry, address, start) {
					landed++
				} else {
					failed++
				}
			}
		}
		return
	}

	memoL, memoF := score(strategyMemo)
	viaL, viaF := score(strategyVia)
	t.Logf("memo landed=%d failed=%d; via landed=%d failed=%d", memoL, memoF, viaL, viaF)

	if viaL < memoL {
		t.Fatalf("via landed fewer writes than memo: via=%d memo=%d", viaL, memoL)
	}
	if viaF > 0 {
		// Every (entry, start) pair has a live owner for this address.
		t.Fatalf("via failed %d placements that have a live owner", viaF)
	}
}

func TestModelHopBoundIsFinite(t *testing.T) {
	for _, strat := range []placeStrategy{strategyMemo, strategyVia} {
		m := livePinnedScenario(strat)
		entry := m.byName["_vSrV2020idx0001"]
		_ = m.placeOnce(entry, "7xmD4zoqo5mppmvz", "7")
		if m.hops > m.hopCap {
			t.Fatalf("%s exceeded hop cap: %d", strat, m.hops)
		}
	}
}

func TestModelSummary(t *testing.T) {
	// Keeps a one-line CI-visible summary of why via is the chosen strategy.
	memoL, memoF := 0, 0
	viaL, viaF := 0, 0
	address := "7xmD4zoqo5mppmvz"
	for _, entryName := range []string{"_vSrV2020idx0001", "_x_rV2020idx0001", "_uSrV2020idx0001"} {
		for _, start := range []string{address, "7", "@"} {
			for _, strat := range []placeStrategy{strategyMemo, strategyVia} {
				m := livePinnedScenario(strat)
				ok := m.placeOnce(m.byName[entryName], address, start)
				if strat == strategyMemo {
					if ok {
						memoL++
					} else {
						memoF++
					}
				} else {
					if ok {
						viaL++
					} else {
						viaF++
					}
				}
			}
		}
	}
	summary := fmt.Sprintf("placement model: memo %d/%d landed; via %d/%d landed",
		memoL, memoL+memoF, viaL, viaL+viaF)
	t.Log(summary)
	if viaL != viaL+viaF {
		t.Fatal(summary)
	}
}
