package core

import (
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

func TestGetMultiplePathFillsAndCaches(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := ownerPeerAt(t, n, "demo", "qr", 3)

	props := []func(*domain.Abelian) int{
		func(a *domain.Abelian) int { return a.Count() },
	}
	body, err := n.GetMultiple("demo", []string{"qr"}, 6, props, true, DefaultDeepHops)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("deep GetMultiple returned empty binary")
	}
	if got := owner.calls.Load(); got != 1 {
		t.Fatalf("owner dialed %d times, want 1", got)
	}
	if cached, ok := n.cache.Get("demo", "qr"); !ok || cached == nil {
		t.Fatal("path-fill did not populate LRU")
	}

	body2, err := n.GetMultiple("demo", []string{"qr"}, 6, props, true, DefaultDeepHops)
	if err != nil || len(body2) == 0 {
		t.Fatalf("cached GetMultiple: len=%d err=%v", len(body2), err)
	}
	if got := owner.calls.Load(); got != 1 {
		t.Fatalf("cache hit still dialed owner: calls=%d", got)
	}
}

func TestGetMultipleDeepFalseSkipsMiss(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.create("demo", "@")
	owner := ownerPeerAt(t, n, "demo", "qs", 2)

	props := []func(*domain.Abelian) int{
		func(a *domain.Abelian) int { return a.Count() },
	}
	body, err := n.GetMultiple("demo", []string{"qs"}, 6, props, false, 0)
	if err != nil {
		t.Fatalf("GetMultiple: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("deep=false should omit miss, got %d bytes", len(body))
	}
	if owner.calls.Load() != 0 {
		t.Fatalf("deep=false dialed owner %d times", owner.calls.Load())
	}
}

func TestGetHopExhaustedNoDial(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	owner := ownerPeerAt(t, n, "demo", "qu", 1)

	// deep=true but hop=0 → same as redirect (budget exhausted).
	contact, set, err := n.Get("demo", "qu", true, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set != nil {
		t.Fatal("hop=0 should not path-fill")
	}
	if contact == nil || contact.Name() != owner.Name() {
		t.Fatal("expected soft redirect to owner")
	}
	if owner.calls.Load() != 0 {
		t.Fatalf("hop=0 dialed owner %d times", owner.calls.Load())
	}
}
