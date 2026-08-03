package domain

import (
	"testing"
	"time"
)

func TestCacheHopTTL(t *testing.T) {
	c := NewCache()
	s := NewSet()
	s.Put("x", NewAbelian(1, []float64{1}))

	c.SetHops("coll", "near", s, 0)
	c.SetHops("coll", "far", s, 3)

	// base 100ms, beta=1 => hop0 keeps 100ms, hop3 shrinks to the 1s floor.
	time.Sleep(30 * time.Millisecond)
	refresh := c.Refresh(100*time.Millisecond, 1.0)
	if _, ok := refresh["coll"]["near"]; !ok {
		t.Fatalf("expected near entry to still be refreshing")
	}

	time.Sleep(100 * time.Millisecond)
	refresh = c.Refresh(100*time.Millisecond, 1.0)
	if _, ok := refresh["coll"]["near"]; ok {
		t.Fatalf("expected near entry to expire")
	}
}

// Pull-on-read: a copy nobody reads is not worth a round trip to the owner.
func TestCacheRefreshOnlyPullsRecentlyRead(t *testing.T) {
	c := NewCache()
	c.SetTouchWindow(30 * time.Millisecond)

	hot, cold := NewSet(), NewSet()
	hot.Put("x", NewAbelian(1, []float64{1}))
	cold.Put("y", NewAbelian(1, []float64{1}))

	c.SetHops("coll", "hot", hot, 0)
	c.SetHops("coll", "cold", cold, 0)

	time.Sleep(50 * time.Millisecond)

	// Only "hot" sees traffic, which bumps its last-read stamp.
	if _, ok := c.Get("coll", "hot"); !ok {
		t.Fatal("expected hot entry to be cached")
	}

	refresh := c.Refresh(time.Minute, 0)
	if _, ok := refresh["coll"]["hot"]; !ok {
		t.Fatal("entry read within the touch window must be refreshed")
	}
	if _, ok := refresh["coll"]["cold"]; ok {
		t.Fatal("entry with no traffic must not be refreshed")
	}
	// Untouched but unexpired copies stay usable, they are just not re-pulled.
	if _, ok := c.Get("coll", "cold"); !ok {
		t.Fatal("cold entry should still be served from cache")
	}
}

// A miss placeholder must not live forever: nothing ever expires it via TTL.
func TestCacheDropsColdPlaceholders(t *testing.T) {
	c := NewCache()
	c.SetTouchWindow(20 * time.Millisecond)

	c.SetHops("coll", "missing", nil, 0)
	if got := c.Len(); got != 1 {
		t.Fatalf("placeholder not stored: len=%d want 1", got)
	}

	// Still inside the window: kept, but never proposed for refresh.
	refresh := c.Refresh(time.Minute, 0)
	if _, ok := refresh["coll"]["missing"]; ok {
		t.Fatal("placeholder must never be actively refreshed")
	}
	if got := c.Len(); got != 1 {
		t.Fatalf("placeholder dropped too early: len=%d want 1", got)
	}

	time.Sleep(40 * time.Millisecond)
	c.Refresh(time.Minute, 0)
	if got := c.Len(); got != 0 {
		t.Fatalf("cold placeholder was not dropped: len=%d want 0", got)
	}
}

func TestCacheMaxEntriesEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewCache()
	c.SetMax(2)

	mk := func(key string) *Set {
		s := NewSet()
		s.Put(key, NewAbelian(1, []float64{1}))
		return s
	}

	c.SetHops("coll", "a", mk("a"), 0)
	time.Sleep(2 * time.Millisecond)
	c.SetHops("coll", "b", mk("b"), 0)
	time.Sleep(2 * time.Millisecond)

	// Reading "a" makes "b" the least recently used one.
	if _, ok := c.Get("coll", "a"); !ok {
		t.Fatal("expected a to be cached")
	}
	time.Sleep(2 * time.Millisecond)

	c.SetHops("coll", "c", mk("c"), 0)

	if got := c.Len(); got != 2 {
		t.Fatalf("cache exceeded its cap: len=%d want 2", got)
	}
	if _, ok := c.Get("coll", "b"); ok {
		t.Fatal("least recently used entry should have been evicted")
	}
	if _, ok := c.Get("coll", "a"); !ok {
		t.Fatal("recently read entry should have been kept")
	}
	if _, ok := c.Get("coll", "c"); !ok {
		t.Fatal("newest entry should have been kept")
	}
}

func TestQueueTryAddBackpressure(t *testing.T) {
	q := NewQueue[int]()
	if !q.TryAdd(1, 2) || !q.TryAdd(2, 2) {
		t.Fatal("expected first two adds to succeed")
	}
	if q.TryAdd(3, 2) {
		t.Fatal("expected backpressure on third add")
	}
	if q.Length() != 2 {
		t.Fatalf("length=%d want 2", q.Length())
	}
}
