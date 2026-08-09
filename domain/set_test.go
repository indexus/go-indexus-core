package domain

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

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

// Entries do not all carry the same metrics: a counter raised by incr starts
// with none, an item arrives with its own. Recomputing an aggregate off
// whichever one the map yields first used to read past the end of the total.
func TestSetShrinkMixesEntryWidths(t *testing.T) {
	base := fakeBase{length: 2}

	set := NewSet()
	sets := map[string]*Set{"@": set}

	set.add("aa:1", NewAbelian(1, nil), base.Length())
	set.add("ab:2", NewAbelian(1, []float64{2, 3, 4}), base.Length())
	set.add("ac:3", NewAbelian(1, []float64{1}), base.Length())

	set.Shrink(base, sets, "@", base.Length())

	child, ok := sets["a"]
	if !ok {
		t.Fatal("expected shrink to create child set a")
	}
	if got := child.Count(); got != 3 {
		t.Fatalf("child count: got %d, want 3", got)
	}
	if got := child.Abelian().Metrics(); len(got) != 3 || got[0] != 3 || got[1] != 3 || got[2] != 4 {
		t.Fatalf("child metrics: got %v, want [3 3 4]", got)
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

// Incr on a missing key used to nil-deref inside Abelian.Sum and take the node
// down during Feed / Restore. Same shape as Decr: create an empty entry first.
func TestSetIncrMissingKey(t *testing.T) {
	s := NewSet()
	got := s.Incr("aa:1", leaf())
	if got == nil {
		t.Fatal("Incr returned nil")
	}
	if got.Count() != 1 {
		t.Fatalf("count: got %d, want 1", got.Count())
	}
	if s.Count() != 1 {
		t.Fatalf("set count: got %d, want 1", s.Count())
	}
}

func TestSetMarshalJSONMatchesList(t *testing.T) {
	s := NewSet()
	s.Put("aa:x", NewAbelian(1, []float64{1, 2}))
	s.Put("ab:y", NewAbelian(1, []float64{3}))

	viaSet, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("Marshal set: %v", err)
	}
	viaList, err := json.Marshal(s.List())
	if err != nil {
		t.Fatalf("Marshal list: %v", err)
	}

	var fromSet, fromList map[string]*Abelian
	if err := json.Unmarshal(viaSet, &fromSet); err != nil {
		t.Fatalf("Unmarshal set: %v", err)
	}
	if err := json.Unmarshal(viaList, &fromList); err != nil {
		t.Fatalf("Unmarshal list: %v", err)
	}
	if len(fromSet) != len(fromList) {
		t.Fatalf("len set=%d list=%d", len(fromSet), len(fromList))
	}
	for k, a := range fromList {
		b, ok := fromSet[k]
		if !ok || !a.IsEqual(b) {
			t.Fatalf("key %s mismatch set=%v list=%v", k, b, a)
		}
	}
}

// MarshalJSON must release Set.mu before encoding so writers are not blocked
// for the duration of json.Marshal.
func TestSetMarshalJSONDoesNotHoldLockDuringEncode(t *testing.T) {
	s := NewSet()
	for i := 0; i < 200; i++ {
		s.Put(string(rune('a'+i%26))+string(rune('0'+i%10)), NewAbelian(1, []float64{float64(i)}))
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = json.Marshal(s)
	}()

	// While marshal runs (or right after snapshot), Put must not deadlock.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.Put("zz:concurrent", NewAbelian(1, []float64{1}))
		select {
		case <-done:
			return
		default:
			time.Sleep(time.Millisecond)
		}
	}
	t.Fatal("Put blocked while MarshalJSON held the lock")
}

// Not holding the lock during encode is only safe if the snapshot owns its
// values. Put replaces the stored pointer, so it never caught this; Incr sums
// into the Abelian in place, and a snapshot sharing that pointer lets a reader
// catch a count and its metrics from either side of the same increment — on
// the path that answers /set.
func TestSnapshotSurvivesConcurrentIncrements(t *testing.T) {
	s := NewSet()
	keys := make([]string, 32)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%d", i)
		s.Put(keys[i], NewAbelian(1, []float64{1}))
	}

	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, key := range keys {
				s.Incr(key, NewAbelian(1, []float64{1}))
			}
		}
	}()

	for round := 0; round < 200; round++ {
		for _, abelian := range s.List() {
			_, _ = abelian.Count(), abelian.Metrics()
		}
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("Marshal: %v", err)
		}
	}
	close(stop)
	writer.Wait()
}

func TestSetMarshalJSONNil(t *testing.T) {
	var s *Set
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "null" {
		t.Fatalf("got %s want null", raw)
	}
}
