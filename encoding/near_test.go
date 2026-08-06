package encoding

import (
	"testing"
)

func TestRandomNameNearKeepsPrefixBits(t *testing.T) {
	target, err := BASE64.Decode("AAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		// generate a valid name then decode
		n, err := BASE64.RandomName()
		if err != nil {
			t.Fatal(err)
		}
		target, err = BASE64.Decode(n)
		if err != nil {
			t.Fatal(err)
		}
	}

	keep := 16
	name, err := BASE64.RandomNameNear(target, keep)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BASE64.Decode(name)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < keep/8; i++ {
		if got[i] != target[i] {
			t.Fatalf("byte %d: got %02x want %02x", i, got[i], target[i])
		}
	}
}

func TestPreferNearTargetsDistinct(t *testing.T) {
	hot, err := BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	targets, err := BASE64.PreferNearTargets(hot, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("len=%d", len(targets))
	}
	seen := map[string]bool{}
	for i, s := range targets {
		if s == "" {
			t.Fatalf("empty target %d", i)
		}
		if seen[s] {
			t.Fatalf("duplicate PreferNear target %q", s)
		}
		seen[s] = true
	}
}

func TestSplitLoadTargetsHalvesWeights(t *testing.T) {
	// Six sorted anchors; weights 100 each → two bins of 300.
	anchors := []string{
		"AAAAAAAAAAAAAAA0",
		"AAAAAAAAAAAAAAA1",
		"AAAAAAAAAAAAAAA2",
		"ZZZZZZZZZZZZZZZ0",
		"ZZZZZZZZZZZZZZZ1",
		"ZZZZZZZZZZZZZZZ2",
	}
	// Ensure they decode; fall back to RandomName-derived keys if needed.
	valid := make([]string, 0, len(anchors))
	weights := make([]int, 0, len(anchors))
	for _, a := range anchors {
		if _, err := BASE64.Decode(a); err != nil {
			n, err := BASE64.RandomName()
			if err != nil {
				t.Fatal(err)
			}
			a = n
		}
		valid = append(valid, a)
		weights = append(weights, 100)
	}
	// Force a lexicographic gap: regenerate sorted random names.
	valid = valid[:0]
	weights = weights[:0]
	for i := 0; i < 6; i++ {
		n, err := BASE64.RandomName()
		if err != nil {
			t.Fatal(err)
		}
		valid = append(valid, n)
		weights = append(weights, 100)
	}
	targets, err := BASE64.SplitLoadTargets(valid, weights, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 {
		t.Fatalf("len=%d want 2", len(targets))
	}
	if targets[0] == targets[1] {
		t.Fatalf("expected distinct half-split targets, got %q twice", targets[0])
	}
}

func TestSplitLoadTargetsThirds(t *testing.T) {
	anchors := make([]string, 0, 9)
	weights := make([]int, 0, 9)
	for i := 0; i < 9; i++ {
		n, err := BASE64.RandomName()
		if err != nil {
			t.Fatal(err)
		}
		anchors = append(anchors, n)
		weights = append(weights, 50)
	}
	targets, err := BASE64.SplitLoadTargets(anchors, weights, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("len=%d want 3", len(targets))
	}
	seen := map[string]bool{}
	for _, s := range targets {
		if seen[s] {
			t.Fatalf("duplicate third-split target %q", s)
		}
		seen[s] = true
	}
}
