package domain

import "testing"

// Regression: when a child set already exists in sets[] before shrinking the
// parent set, Shrink must not dereference a nil list[child] aggregate.
func TestSetShrinkWithPreExistingChildSet(t *testing.T) {
	base := NewBase(64, 96)
	root := base.Root()
	child := base.CharAt(0)
	if child == "" {
		t.Fatal("base alphabet empty")
	}

	parentSet := NewSet()
	sets := map[string]*Set{
		root:  parentSet,
		child: NewSet(), // pre-existing child set triggers the branch
	}

	parentSet.Put(child+":id-1", NewAbelian(1, []float64{1}))

	parentSet.Shrink(base, sets, root, base.Length())

	agg, ok := parentSet.Get(child)
	if !ok || agg == nil {
		t.Fatalf("expected parent aggregate for child %q after shrink", child)
	}
	if agg.Count() != 1 {
		t.Fatalf("unexpected aggregate count: got %d want 1", agg.Count())
	}
}
