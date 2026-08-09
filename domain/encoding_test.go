// Package domain_test holds the domain tests that need the real BASE64
// alphabet. They cannot be internal: encoding imports domain, so a test inside
// package domain that imported encoding back would close the cycle.
package domain_test

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func TestChildCandidatesRootAndNested(t *testing.T) {
	base := encoding.BASE64
	rootKids := domain.ChildCandidates(base, base.Root())
	if len(rootKids) != base.Length() {
		t.Fatalf("root candidates=%d want %d", len(rootKids), base.Length())
	}
	for _, k := range rootKids {
		if len(k) != 1 {
			t.Fatalf("root child %q want len 1", k)
		}
		if base.Parent(k) != base.Root() {
			t.Fatalf("Parent(%q)=%q want root", k, base.Parent(k))
		}
	}

	nested := domain.ChildCandidates(base, "w")
	if len(nested) != base.Length() {
		t.Fatalf("nested candidates=%d want %d", len(nested), base.Length())
	}
	for _, k := range nested {
		if base.Parent(k) != "w" {
			t.Fatalf("Parent(%q)=%q want w", k, base.Parent(k))
		}
	}
}

func TestIsDirectChild(t *testing.T) {
	base := encoding.BASE64
	if !domain.IsDirectChild(base, base.Root(), "a") {
		t.Fatal("root→a should be direct")
	}
	if domain.IsDirectChild(base, base.Root(), "ab") {
		t.Fatal("root→ab is not direct")
	}
	if domain.IsDirectChild(base, "a", "a") {
		t.Fatal("a self-key must be rejected")
	}
	if !domain.IsDirectChild(base, "a", "ab") {
		t.Fatal("a→ab should be direct")
	}
	// The edges traverse must refuse: a sibling branch one level down looks
	// close but has a different parent, and following it builds a cycle.
	if domain.IsDirectChild(base, "abcd", "abce") {
		t.Fatal("abce is a sibling branch, its parent is abc")
	}
	if !domain.IsDirectChild(base, "abcd", "abcde") {
		t.Fatal("abcde should be a direct child of abcd")
	}
	// Every letter is an ordinary location. Reserving one as a marker hides
	// every zone whose name starts with it (P4).
	for i := 0; i < base.Length(); i++ {
		child := base.CharAt(i)
		if !domain.IsDirectChild(base, base.Root(), child) {
			t.Fatalf("%q is a letter of the alphabet, so a child of the root", child)
		}
	}
}

// P4: the key space belongs to locations. A set pulled from /aggregates carries
// a total without a child list; marking it with a reserved key costs a
// location, and "_" is a letter. Reserving it hid every zone under "_" from
// traverse — items stored, counted nowhere. The total lives on the aggregate
// instead, where nothing can collide.
func TestSummaryPlaceholderCarriesNoReservedKey(t *testing.T) {
	summary := domain.NewSetFromAbelian(domain.NewAbelian(7, []float64{1}))
	if !summary.IsSummaryPlaceholder() {
		t.Fatal("an aggregate-only set must read as a placeholder")
	}
	if summary.Count() != 7 {
		t.Fatalf("placeholder count=%d want 7", summary.Count())
	}
	if len(summary.List()) != 0 {
		t.Fatalf("placeholder exposes children: %v", summary.List())
	}

	real := domain.NewSet()
	real.Put("_", domain.NewAbelian(3, nil))
	if real.IsSummaryPlaceholder() {
		t.Fatal("a set holding the zone \"_\" is a real child list")
	}
	if _, ok := real.Get("_"); !ok {
		t.Fatal("Put dropped the zone \"_\"")
	}
}

func TestItemsUnderEveryLetterAreCounted(t *testing.T) {
	base := encoding.BASE64
	c := domain.NewCollection("demo", base.Root(), base)
	c.EnsureSet(base.Root())
	c.Own(base.Root(), domain.Delegation{})

	// One item per alphabet letter, so "_" and "-" are exercised alongside the
	// alphanumerics they are usually lost behind.
	for i := 0; i < base.Length(); i++ {
		location := base.CharAt(i)
		if c.Add(location, "id"+location, []float64{1}, 1000) == nil {
			t.Fatalf("Add refused location %q", location)
		}
	}

	if got := c.ItemCount(base.Root()); got != base.Length() {
		t.Fatalf("counted %d items under the root, want %d", got, base.Length())
	}
}
