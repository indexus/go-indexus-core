package domain

import (
	"fmt"
	"testing"
	"time"
)

// Guarantee P3: no key aliases itself and no walk can loop. Shrink and Traverse
// are the two places a bad edge gets created or followed, so they are pinned
// together — a fix on one side that leaves the other open is not a fix.

func listKeys(s *Set) []string {
	out := make([]string, 0, len(s.List()))
	for k := range s.List() {
		out = append(out, k)
	}
	return out
}

// countItems walks the subtree with a watchdog: a cycle shows up as a hang or a
// stack overflow, neither of which a plain call would survive to report.
func countItems(t *testing.T, c *Collection, parent string) int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		n := 0
		c.Traverse(parent, func(string, *Abelian) {}, func(string, string, string, *Abelian) { n++ })
		done <- n
	}()
	select {
	case n := <-done:
		return n
	case <-time.After(2 * time.Second):
		t.Fatalf("Traverse hung or overflowed under %q", parent)
		return 0
	}
}

// Root shrink used precision=0, so location "@" yielded child "@" — the set
// being shrunk. That recursed forever and/or installed a self-key aggregate.
func TestShrinkRootItemsDoNotSelfAlias(t *testing.T) {
	base := fakeBase{length: 2}
	root := NewSet()
	sets := map[string]*Set{"@": root}

	root.add("@:x", leaf(), 1)
	root.add("@:y", leaf(), 1)

	done := make(chan struct{})
	go func() {
		root.Shrink(base, sets, "@", 1, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shrink hung/stack-overflowed on @: leaves")
	}

	if _, ok := root.Get("@"); ok {
		t.Fatalf("self-key @ present after root shrink; keys=%v", listKeys(root))
	}
	for _, k := range []string{"@:x", "@:y"} {
		if _, ok := root.Get(k); !ok {
			t.Fatalf("leaf %q dropped; keys=%v", k, listKeys(root))
		}
	}
}

func TestShrinkDropsExistingSelfAggregate(t *testing.T) {
	base := fakeBase{length: 4}
	zone := NewSet()
	sets := map[string]*Set{"abcde": zone}
	zone.Put("abcde", NewAbelian(9, []float64{9}))
	zone.add("abcde:1", leaf(), 4)
	zone.Shrink(base, sets, "abcde", 4, nil)
	if _, ok := zone.Get("abcde"); ok {
		t.Fatalf("shrink should drop the self-aggregate; keys=%v", listKeys(zone))
	}
}

func TestAddRootItemsNoSelfKey(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	for i := 0; i < 20; i++ {
		c.Add("@", fmt.Sprintf("id-%d", i), []float64{1}, 100000)
	}
	root, _ := c.Get("@")
	if _, ok := root.Get("@"); ok {
		t.Fatalf("Add(@) created a self-key; keys=%v", listKeys(root))
	}
}

// The production crash: a zone set holding its own location as a child key made
// traverse recurse forever (Snapshot/Checkpoint → stack overflow on txSrV).
func TestTraverseSelfKeyNoStackOverflow(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 8})
	c.EnsureSet("abcde")
	c.Own("abcde", Delegation{})

	set, ok := c.Get("abcde")
	if !ok {
		t.Fatal("missing set")
	}
	set.Put("abcde:leaf", NewAbelian(1, []float64{1}))
	// Corrupt edge observed in the wild: a location-only key equal to the zone.
	set.Put("abcde", NewAbelian(2, []float64{2}))

	if n := countItems(t, c, "abcde"); n != 1 {
		t.Fatalf("item count=%d want 1 (the self-key must not be followed)", n)
	}
}

func TestTraverseSiblingCycleNoStackOverflow(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 8})
	c.EnsureSet("abcd")
	c.EnsureSet("abcde")
	c.EnsureSet("abcdf")
	c.Own("abcd", Delegation{})

	// Illicit sibling edges at the same depth: IsDirectChild allows abcd→abcde
	// and abcd→abcdf, so only the visited set can stop the mutual links below.
	ab, _ := c.Get("abcde")
	af, _ := c.Get("abcdf")
	ab.Put("abcde:x", NewAbelian(1, nil))
	af.Put("abcdf:y", NewAbelian(1, nil))
	ab.Put("abcdf", NewAbelian(1, nil)) // sibling pointer
	af.Put("abcde", NewAbelian(1, nil)) // back edge

	parent, _ := c.Get("abcd")
	parent.Put("abcde", NewAbelian(1, nil))
	parent.Put("abcdf", NewAbelian(1, nil))

	if n := countItems(t, c, "abcd"); n != 2 {
		t.Fatalf("item count=%d want 2", n)
	}
}

// A chain of zones each pointing back at an ancestor: the walk must still
// terminate, and must count every leaf exactly once on the way.
func TestTraverseBackEdgeToAncestorTerminates(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 8})
	for _, loc := range []string{"a", "ab", "abc"} {
		c.EnsureSet(loc)
	}
	c.Own("a", Delegation{})

	for i, loc := range []string{"a", "ab", "abc"} {
		set, _ := c.Get(loc)
		set.Put(fmt.Sprintf("%s:leaf-%d", loc, i), NewAbelian(1, nil))
	}
	a, _ := c.Get("a")
	ab, _ := c.Get("ab")
	abc, _ := c.Get("abc")
	a.Put("ab", NewAbelian(1, nil))
	ab.Put("abc", NewAbelian(1, nil))
	abc.Put("a", NewAbelian(1, nil)) // back edge to the ancestor

	if n := countItems(t, c, "a"); n != 3 {
		t.Fatalf("item count=%d want 3 (each leaf once, no revisit)", n)
	}
}
