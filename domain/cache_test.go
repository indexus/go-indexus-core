package domain

import (
	"sync"
	"testing"
	"time"
)

func TestCacheSetHopsReplacesSummaryPlaceholder(t *testing.T) {
	c := NewCache()
	summary := NewSetFromAbelian(NewAbelian(1, []float64{1}))
	c.SetHops("demo", "@", summary, 1)

	real := NewSet()
	real.Put("a", NewAbelian(1, []float64{1}))
	c.SetHops("demo", "@", real, 1)

	got, ok := c.Get("demo", "@")
	if !ok || got == nil {
		t.Fatal("expected cached set after upgrade")
	}
	if got.IsSummaryPlaceholder() {
		t.Fatal("summary placeholder survived a real child-list write")
	}
	if _, ok := got.Get("a"); !ok {
		t.Fatal("upgraded cache lost the child list")
	}
}

func TestCacheForgetUnder(t *testing.T) {
	c := NewCache()
	c.Set("demo", "aa", NewSet())
	c.Set("demo", "aab", NewSet())
	c.Set("demo", "bb", NewSet())
	c.Set("other", "aa", NewSet())

	c.ForgetUnder("demo", "aa")
	if c.Len() != 2 {
		t.Fatalf("len=%d want 2 (bb + other/aa)", c.Len())
	}
	if _, ok := c.Get("demo", "aa"); ok {
		t.Fatal("expected aa forgotten")
	}
	if _, ok := c.Get("demo", "aab"); ok {
		t.Fatal("expected aab forgotten")
	}
	if _, ok := c.Get("demo", "bb"); !ok {
		t.Fatal("bb should remain")
	}
	if _, ok := c.Get("other", "aa"); !ok {
		t.Fatal("other collection should remain")
	}
}

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

func TestCacheConcurrentGetSetRefresh(t *testing.T) {
	const (
		writers = 4
		readers = 4
		ops     = 500
	)

	cache := NewCache()

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				cache.Set("coll", string(rune('A'+w)), NewSet())
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				_, _ = cache.Get("coll", "A")
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < ops; i++ {
			_ = cache.Refresh(0, 1.0)
		}
	}()

	wg.Wait()
}
