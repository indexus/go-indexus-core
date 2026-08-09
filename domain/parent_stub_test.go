package domain

import (
	"fmt"
	"testing"
)

// A parent stub must follow the current owner of its child, whatever counters
// either side happens to hold. The regression this pins: zone summaries used to
// be ordered by a node-local epoch that travelled in no handoff payload, so a
// mature donor handing a zone to a fresh recipient froze the parent on the
// pre-handoff value forever. See protocol.md §2.
func TestParentStubFollowsOwnerAcrossHandoff(t *testing.T) {
	parent := NewCollection("col", "@", fakeBase{length: 26})
	parent.EnsureSet("a")
	parent.Own("a", Delegation{})

	parent.SetChildSummary("ab", NewAbelian(1024, []float64{1024}))
	if got := stubCount(t, parent, "a", "ab"); got != 1024 {
		t.Fatalf("setup: parent stub = %d, want 1024", got)
	}

	// The zone moved to a new owner, which now serves the truth.
	parent.SetChildSummary("ab", NewAbelian(90000, []float64{90000}))
	if got := stubCount(t, parent, "a", "ab"); got != 90000 {
		t.Fatalf("parent stub = %d, want 90000: it did not follow the new owner", got)
	}

	// A handoff can legitimately shrink a zone (half of it split away).
	parent.SetChildSummary("ab", NewAbelian(45000, []float64{45000}))
	if got := stubCount(t, parent, "a", "ab"); got != 45000 {
		t.Fatalf("parent stub = %d, want 45000: a legitimate shrink was rejected", got)
	}
}

func stubCount(t *testing.T, c *Collection, parent, child string) int {
	t.Helper()
	set, ok := c.Get(parent)
	if !ok || set == nil {
		t.Fatalf("no set at %q", parent)
	}
	ab, ok := set.Get(child)
	if !ok || ab == nil {
		return 0
	}
	return ab.Count()
}

// Parent zones after child split/handoff hold Abelian *stubs* (cache toward
// leaf truth). Leaf owners are the item SoT. These tests pin gaps between
// that model and Count/Traverse/Browse (Status.zones).

func fillUntilSplitLoc(t *testing.T, c *Collection, loc string, delegation int) (child string, n int) {
	t.Helper()
	for i := 0; i < delegation*40; i++ {
		areas := c.Add(loc, fmt.Sprintf("id-%d", i), []float64{1}, delegation)
		if areas == nil {
			t.Fatalf("add %d refused", i)
		}
		n = i + 1
		for area := range areas {
			if area != c.base.Root() && area != loc {
				return area, n
			}
		}
	}
	t.Fatal("no child split")
	return "", 0
}

func leafCountAt(c *Collection, loc string) int {
	n := 0
	c.Traverse(loc, func(string, *Abelian) {}, func(string, string, string, *Abelian) { n++ })
	return n
}

func TestParentStubRemainsWhileLocalChildOwned(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.EnsureSet("@")
	c.Own("@", Delegation{})
	const delegation = 4

	child, added := fillUntilSplitLoc(t, c, "aa", delegation)
	c.Own(child, Delegation{})

	parent := c.base.Parent(child)
	if parent == "" {
		parent = "@"
	}

	delegated := false
	c.Browse(func(string) {}, func(own, d string, _ Handoff) {
		if own == parent && d == child {
			delegated = true
		}
	})
	if !delegated {
		t.Fatalf("parent %q does not list child %q as delegated", parent, child)
	}

	parentLeaves := leafCountAt(c, parent)
	childLeaves := leafCountAt(c, child)
	seen := map[string]struct{}{}
	c.Traverse(child, func(string, *Abelian) {}, func(_, loc, id string, _ *Abelian) {
		seen[loc+":"+id] = struct{}{}
	})
	dup := 0
	c.Traverse(parent, func(string, *Abelian) {}, func(_, loc, id string, _ *Abelian) {
		if _, ok := seen[loc+":"+id]; ok {
			dup++
		}
	})
	if dup != 0 {
		t.Fatalf("Traverse(parent) yields %d leaves also under child — Items would double-count", dup)
	}

	pset, ok := c.Get(parent)
	if !ok {
		t.Fatalf("parent set %q missing", parent)
	}
	stub, ok := pset.Get(child)
	if !ok || stub == nil {
		t.Fatalf("parent set has no Abelian stub for local child %q", child)
	}
	abelianTotal := pset.Abelian().Count()
	t.Logf("added=%d residualParent=%d childLeaves=%d stub=%d parentAbelian=%d",
		added, parentLeaves, childLeaves, stub.Count(), abelianTotal)
	if abelianTotal <= parentLeaves && stub.Count() > 0 {
		t.Fatalf("expected parent Abelian (%d) > residual leaves (%d) when child stub present", abelianTotal, parentLeaves)
	}
}

func TestParentAfterDelegateIsStubNotSoT(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.EnsureSet("@")
	c.Own("@", Delegation{})
	const delegation = 4

	child, _ := fillUntilSplitLoc(t, c, "aa", delegation)
	c.Own(child, Delegation{})
	beforeChild := leafCountAt(c, child)
	if beforeChild < 1 {
		t.Fatal("child empty before Delegate")
	}

	items, _ := c.Delegate(child)
	if len(items) < 1 {
		t.Fatal("Delegate returned no items")
	}
	c.MarkDelegatedTo(c.base.Parent(child), child, "receiver")
	if c.Allowing(child) {
		t.Fatal("child still Allowing after Delegate")
	}
	if leafCountAt(c, child) != 0 {
		t.Fatalf("Traverse(child)=%d after Delegate — donor still SoT", leafCountAt(c, child))
	}

	parent := c.base.Parent(child)
	if parent == "" {
		parent = "@"
	}
	stillDelegated := false
	c.Browse(func(string) {}, func(own, d string, _ Handoff) {
		if own == parent && d == child {
			stillDelegated = true
		}
	})
	if !stillDelegated {
		t.Fatalf("parent %q dropped delegation for %q — cannot cache/pull", parent, child)
	}

	pset, ok := c.Get(parent)
	if !ok {
		t.Fatal("parent set missing")
	}
	stub, ok := pset.Get(child)
	if !ok {
		t.Fatalf("parent lost Abelian stub for %q", child)
	}
	t.Logf("handed=%d stubStill=%d residualParent=%d (stale stub until pull is expected)",
		len(items), stub.Count(), leafCountAt(c, parent))
}

func TestParentStubConvergesWithoutResurrectingLeaves(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.EnsureSet("@")
	c.Own("@", Delegation{})
	const delegation = 4

	child, _ := fillUntilSplitLoc(t, c, "bb", delegation)
	c.Own(child, Delegation{})
	_, _ = c.Delegate(child)

	c.SetChildSummary(child, NewAbelian(42, []float64{42}))
	if leafCountAt(c, child) != 0 {
		t.Fatalf("Update resurrected %d leaves under delegated child", leafCountAt(c, child))
	}
	parent := c.base.Parent(child)
	if parent == "" {
		parent = "@"
	}
	pset, _ := c.Get(parent)
	stub, ok := pset.Get(child)
	if !ok || stub.Count() != 42 {
		cnt := 0
		if stub != nil {
			cnt = stub.Count()
		}
		t.Fatalf("stub count=%d want 42 ok=%v", cnt, ok)
	}
}

func TestPureParentShellStillListedInBrowse(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.EnsureSet("@")
	c.Own("@", Delegation{})
	const delegation = 4

	child, _ := fillUntilSplitLoc(t, c, "cc", delegation)
	c.Own(child, Delegation{})
	_, _ = c.Delegate(child)

	areas := c.Complete("@")
	for loc, deleg := range areas {
		c.Own(loc, deleg)
	}

	var locs []string
	c.Browse(func(o string) { locs = append(locs, o) }, func(_, _ string, _ Handoff) {})
	if len(locs) < 1 {
		t.Fatal("no ownership after Complete")
	}
	pureParents := 0
	for _, o := range locs {
		if leafCountAt(c, o) == 0 {
			pureParents++
		}
	}
	t.Logf("Browse owned=%d pureParentShells=%d (Status.zones counts shells; Items does not)", len(locs), pureParents)
}

func TestMissingDelegationMarkDoubleCountsLeaves(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	c.EnsureSet("@")
	c.Own("@", Delegation{})

	c.EnsureSet("x")
	c.EnsureSet("xy")
	c.Own("x", Delegation{}) // child NOT marked
	c.Own("xy", Delegation{})
	xs, _ := c.Get("x")
	xys, _ := c.Get("xy")
	xs.Put("xy", NewAbelian(1, []float64{1}))
	xys.Put("xy:a", NewAbelian(1, []float64{1}))
	xs.Put("x:b", NewAbelian(1, []float64{1}))

	parentN := leafCountAt(c, "x")
	childN := leafCountAt(c, "xy")
	if !(parentN >= 2 && childN == 1) {
		t.Fatalf("setup Traverse(x)=%d Traverse(xy)=%d", parentN, childN)
	}
	t.Logf("summing zone Traverses without mark double-counts (%d+%d)", parentN, childN)

	c.Own("x", Delegation{"xy": Handoff{}})
	if leafCountAt(c, "x") != 1 {
		t.Fatalf("after mark Traverse(x)=%d want 1", leafCountAt(c, "x"))
	}
	if leafCountAt(c, "xy") != 1 {
		t.Fatal("child leaves lost")
	}
}
