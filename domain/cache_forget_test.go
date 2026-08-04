package domain

import "testing"

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
