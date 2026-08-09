package domain

// Delegation contract (docs/protocol.md §O).
//
// Ownership moves through a strict three-step protocol:
//   1. Add crossing the delegation threshold splits an area off: the parent
//      records it as delegated and Add returns it so the node can claim it.
//   2. A delegated area refuses local writes until Own() — the gap between
//      "split decided" and "ownership claimed" must be closed by the caller,
//      never papered over by accepting writes into limbo.
//   3. Delegate(area) hands the whole subtree (items + tombstones) out,
//      re-marks the parent, and drops local ownership — after which local
//      writes are refused again until someone re-owns.
//
// These are the exact invariants transferToPeer and SoftLeave lean on.

import (
	"fmt"
	"testing"
)

// splitArea drives adds under location until Add returns a delegated area,
// and returns it with the number of items inserted so far.
func splitArea(t *testing.T, c *Collection, location string, delegation int) (string, int) {
	t.Helper()
	for i := 0; i < 200; i++ {
		areas := c.Add(location, fmt.Sprintf("id-%d", i), []float64{1}, delegation)
		if areas == nil {
			t.Fatalf("add %d refused before any split", i)
		}
		for area := range areas {
			if area != c.base.Root() {
				return area, i + 1
			}
		}
	}
	t.Fatal("no delegation split after 200 adds")
	return "", 0
}

// Step 1+2: the split area is recorded as delegated on the parent, and local
// adds under it are refused until Own claims it.
func TestSplitAreaRefusesAddsUntilOwned(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	const delegation = 4

	area, _ := splitArea(t, c, "aa", delegation)
	c.MarkDelegatedTo(c.base.Parent(area), area, "local-owner")

	if c.Allowing(area) {
		t.Fatalf("area %q split off but still allowed before Own", area)
	}
	if got := c.Add(area, "limbo", []float64{1}, delegation); got != nil {
		t.Fatalf("add under delegated area %q accepted before Own — write into limbo", area)
	}

	c.Own(area, Delegation{})

	if !c.Allowing(area) {
		t.Fatalf("area %q still refused after Own", area)
	}
	if got := c.Add(area, "landed", []float64{1}, delegation); got == nil {
		t.Fatalf("add under area %q refused after Own", area)
	}
}

// Step 3: Delegate hands out every live item and every tombstone under the
// area, exactly once — the second call returns nothing, so a retried
// transfer cannot double-send, and local adds are refused after the handoff.
func TestDelegateHandsSubtreeOnceAndDropsOwnership(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	const delegation = 4

	area, added := splitArea(t, c, "aa", delegation)
	c.Own(area, Delegation{})

	// One delete inside the area: its tombstone must travel with the batch.
	if _, _, ok := c.Remove("aa", "id-0"); !ok {
		t.Fatal("remove of a live id refused")
	}

	items, _ := c.Delegate(area)
	c.MarkDelegatedTo(c.base.Parent(area), area, "receiver")

	live, tombs := 0, 0
	for _, it := range items {
		if it.Tombstone {
			tombs++
			continue
		}
		live++
	}
	if live != added-1 {
		t.Fatalf("Delegate returned %d live items, area held %d", live, added-1)
	}
	if tombs != 1 {
		t.Fatalf("Delegate carried %d tombstones, want 1 — receiver would resurrect the deleted id", tombs)
	}

	if c.Allowing(area) {
		t.Fatalf("area %q still allowed after Delegate — donor would keep absorbing writes it no longer owns", area)
	}
	if got := c.Add("aa", "late", []float64{1}, delegation); got != nil {
		t.Fatal("late add accepted locally after Delegate — the item would be invisible to the new owner")
	}

	again, _ := c.Delegate(area)
	if len(again) != 0 {
		t.Fatalf("second Delegate returned %d items — a retried transfer would double-send", len(again))
	}
}

// A child is either owned here or held by a peer, and the parent must be able
// to tell them apart: /children reports both, and only the second kind needs an
// /aggregates pull. A mark that is not a direct child is a cycle waiting to be
// followed, so it is refused rather than recorded.
func TestOwnedAndDelegatedChildrenAreDistinguished(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 4})
	c.EnsureSet("@")
	c.Own("@", Delegation{})
	c.EnsureSet("a")
	c.Own("a", Delegation{})
	set, _ := c.Get("a")
	set.Put("a:x", NewAbelian(3, []float64{3}))

	if owned := c.OwnedChildren("@"); owned["a"] == nil || owned["a"].Count() != 3 {
		t.Fatalf("OwnedChildren(@)=%#v want a=3", owned)
	}

	c.MarkDelegatedTo("@", "b", "peer-b")
	if !c.IsDelegated("@", "b") {
		t.Fatal("expected b delegated under @")
	}
	if dels := c.DelegatedChildren("@"); len(dels) != 1 || dels[0] != "b" {
		t.Fatalf("DelegatedChildren(@)=%v want [b]", dels)
	}

	c.MarkDelegated("@", "@") // self-key
	c.MarkDelegated("a", "a") // self-key
	c.MarkDelegated("a", "b") // b is not a direct child of a
	if c.IsDelegated("@", "@") || c.IsDelegated("a", "a") || c.IsDelegated("a", "b") {
		t.Fatal("MarkDelegated accepted an edge that is not a direct child")
	}
}

func TestMarkDelegatedToStoresPeerAndLegacyMarkIsAnonymous(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 4})
	c.EnsureSet("@")
	c.Own("@", Delegation{})

	c.MarkDelegatedTo("@", "a", "peer-alpha")
	d := c.owned["@"]["a"]
	if d.Peer != "peer-alpha" {
		t.Fatalf("MarkDelegatedTo peer: got %q want peer-alpha", d.Peer)
	}
	if d.At == 0 {
		t.Fatal("MarkDelegatedTo must set At")
	}

	c.MarkDelegated("@", "b")
	legacy := c.owned["@"]["b"]
	if legacy.Peer != "" {
		t.Fatalf("MarkDelegated peer: got %q want empty", legacy.Peer)
	}
	if legacy.At == 0 {
		t.Fatal("MarkDelegated must set At")
	}
	// Old snapshots contain only "delegation|child": no peer and no tick.
	c.NoteDelegatedHandoff("b", Handoff{})
	if !c.Allowing("ba") {
		t.Fatal("anonymous legacy mark must not block writes")
	}
	if _, _, blocked := c.DelegatedCover("ba"); blocked {
		t.Fatal("anonymous legacy mark must not produce a delegated cover")
	}
	if c.IsDelegated("@", "b") {
		t.Fatal("anonymous legacy mark must remain unresolved, not authoritative")
	}
}

// Re-owning after a failed transfer (restoreDelegated's path) must restore
// the exact pre-Delegate state: same items, same aggregate count.
func TestDelegateThenReAddRoundTrips(t *testing.T) {
	c := NewCollection("demo", "@", fakeBase{length: 2})
	const delegation = 4

	area, _ := splitArea(t, c, "aa", delegation)
	c.Own(area, Delegation{})

	root, _ := c.Get("@")
	before := root.Abelian().Count()

	items, _ := c.Delegate(area)

	// Failed transfer: re-own and put everything back (restoreDelegated).
	c.Own(area, Delegation{})
	for _, it := range items {
		if it.Tombstone {
			c.ApplyTombstone(it.Location, it.Id, it.Gen)
			continue
		}
		if got := c.Add(it.Location, it.Id, it.Metrics, delegation); got == nil {
			t.Fatalf("restore of %s:%s refused", it.Location, it.Id)
		}
	}

	root, _ = c.Get("@")
	if after := root.Abelian().Count(); after != before {
		t.Fatalf("restore does not round-trip: root count %d want %d", after, before)
	}
}
