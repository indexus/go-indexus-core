package domain

import "testing"

func leaf() *Abelian {
	return NewAbelian(1, []float64{1})
}

// A full set splits its leaves into a child set one character deeper, and
// replaces them by a single aggregate entry.
func TestSetShrinkCreatesChild(t *testing.T) {
	base := fakeBase{length: 2}

	root := NewSet()
	sets := map[string]*Set{"@": root}

	root.add("aa:1", leaf(), base.Length())
	root.add("ab:2", leaf(), base.Length())
	root.add("ac:3", leaf(), base.Length())

	root.Shrink(base, sets, "@", base.Length())

	if _, ok := sets["a"]; !ok {
		t.Fatal("expected shrink to create child set a")
	}
	if got := len(root.List()); got != 1 {
		t.Fatalf("root entries: got %d, want 1 aggregate", got)
	}
	if got := sets["a"].Count(); got != 3 {
		t.Fatalf("child count: got %d, want 3", got)
	}
	agg, ok := root.Get("a")
	if !ok {
		t.Fatal("root missing aggregate for child a")
	}
	if got := agg.Count(); got != 3 {
		t.Fatalf("aggregate count: got %d, want 3", got)
	}
}

// Migrating into a child set that already exists must aggregate the whole
// child, not the moved leaf alone.
func TestSetShrinkReusesChild(t *testing.T) {
	base := fakeBase{length: 2}

	root := NewSet()
	child := NewSet()
	sets := map[string]*Set{"@": root, "a": child}

	child.add("aa:x", leaf(), base.Length())

	root.add("aa:1", leaf(), base.Length())
	root.add("ab:2", leaf(), base.Length())
	root.add("ac:3", leaf(), base.Length())

	root.Shrink(base, sets, "@", base.Length())

	if got := sets["a"].Count(); got != 4 {
		t.Fatalf("child count: got %d, want 4", got)
	}
	agg, ok := root.Get("a")
	if !ok {
		t.Fatal("root missing aggregate for child a")
	}
	if got := agg.Count(); got != 4 {
		t.Fatalf("aggregate count: got %d, want 4", got)
	}
}

// An aggregate already present in the set is copied during the same pass that
// migrates leaves into it, so the refreshed value must win whatever the map
// iteration order was.
func TestSetShrinkAggregateNotStale(t *testing.T) {
	base := fakeBase{length: 2}

	root := NewSet()
	child := NewSet()
	sets := map[string]*Set{"@": root, "a": child}

	child.add("aa:x", leaf(), base.Length())
	child.add("ab:y", leaf(), base.Length())
	root.put("a", child.abelian())

	// A leaf at location "a" lives in the root set, next to the "a" aggregate.
	root.put("a:3", leaf())

	root.Shrink(base, sets, "@", base.Length())

	if got := sets["a"].Count(); got != 3 {
		t.Fatalf("child count: got %d, want 3", got)
	}
	agg, ok := root.Get("a")
	if !ok {
		t.Fatal("root missing aggregate for child a")
	}
	if got := agg.Count(); got != 3 {
		t.Fatalf("aggregate count: got %d, want 3 (stale copy won)", got)
	}
}

// Leaves whose location is the location of the set itself cannot be split any
// further: shrink must leave them alone instead of slicing into their id.
func TestSetShrinkKeepsLeavesAtOwnLocation(t *testing.T) {
	base := fakeBase{length: 2}

	set := NewSet()
	sets := map[string]*Set{"aa": set}

	set.add("aa:1", leaf(), base.Length())
	set.add("aa:2", leaf(), base.Length())
	set.add("aa:3", leaf(), base.Length())

	set.Shrink(base, sets, "aa", base.Length())

	if got := len(sets); got != 1 {
		t.Fatalf("sets: got %d, want 1 (no deeper set is possible)", got)
	}
	for _, key := range []string{"aa:1", "aa:2", "aa:3"} {
		if _, ok := set.Get(key); !ok {
			t.Fatalf("leaf %q dropped by shrink", key)
		}
	}
}

// Deeper locations keep splitting: each recursion consumes one more character.
func TestSetShrinkRecursesOnDeepLocations(t *testing.T) {
	base := fakeBase{length: 2}

	root := NewSet()
	sets := map[string]*Set{"@": root}

	root.add("abc:1", leaf(), base.Length())
	root.add("abd:2", leaf(), base.Length())
	root.add("abe:3", leaf(), base.Length())

	root.Shrink(base, sets, "@", base.Length())

	for _, key := range []string{"a", "ab"} {
		if _, ok := sets[key]; !ok {
			t.Fatalf("expected shrink to create set %q", key)
		}
	}
	if got := len(sets); got != 3 {
		t.Fatalf("sets: got %d, want 3 (@, a, ab)", got)
	}
	if got := sets["ab"].Count(); got != 3 {
		t.Fatalf("leaf holder count: got %d, want 3", got)
	}
	agg, ok := root.Get("a")
	if !ok {
		t.Fatal("root missing aggregate for child a")
	}
	if got := agg.Count(); got != 3 {
		t.Fatalf("aggregate count: got %d, want 3", got)
	}
}
