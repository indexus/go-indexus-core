package domain

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeBase is a minimal Encoder implementation that mimics base64-style
// hierarchical prefixes well enough to drive Collection through Add and
// GetMultiple without pulling in encoding/base.go (which would create a
// test cycle).
type fakeBase struct{}

func (fakeBase) Packing() int                    { return 6 }
func (fakeBase) Encode(data []byte) string       { return string(data) }
func (fakeBase) Decode(s string) ([]byte, error) { return []byte(s), nil }
func (fakeBase) Root() string                    { return "@" }

// Length controls when Set.shrink is invoked. We deliberately use a huge
// number to avoid triggering Set.shrink, which has an unrelated pre-existing
// nil-deref at set.go:179 and would mask the concurrency assertions of
// these tests.
func (fakeBase) Length() int                 { return 1 << 20 }
func (fakeBase) NewID() []byte               { return make([]byte, 16) }
func (fakeBase) RandomName() (string, error) { return "x", nil }
func (fakeBase) Parent(hash string) string {
	if hash == "@" {
		return ""
	}
	if len(hash) == 1 {
		return "@"
	}
	return hash[:len(hash)-1]
}

// TestCollectionConcurrentAddAndGetMultiple is the canonical race test for
// the read path: GetMultiple iterates c.sets and set.list with no
// synchronisation, so it must race with Add from any other goroutine. We
// also exercise Get and Update in parallel to catch related issues.
func TestCollectionConcurrentAddAndGetMultiple(t *testing.T) {
	c := NewCollection("coll", "@", fakeBase{})

	const (
		writers = 4
		readers = 4
		ops     = 500
	)

	var wg sync.WaitGroup
	var stop atomic.Bool

	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				location := fmt.Sprintf("%c%c", 'a'+w, 'a'+(i%26))
				c.Add(location, fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, 1<<20)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			locations := []string{"@", "a", "b", "c", "aa", "ab"}
			props := []func(*Abelian) int{
				func(a *Abelian) int { return a.Count() },
			}
			for i := 0; i < ops && !stop.Load(); i++ {
				_, _ = c.GetMultiple(locations, 4, props)
				_, _ = c.Get("@")
			}
		}()
	}

	wg.Wait()
	stop.Store(true)
}

// TestCollectionConcurrentAddSameLocation funnels every writer onto the
// same prefix path so they all contend for the same Set instances. This is
// where unprotected map mutations would surface fastest.
func TestCollectionConcurrentAddSameLocation(t *testing.T) {
	c := NewCollection("coll", "@", fakeBase{})

	const (
		writers = 8
		ops     = 250
	)

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				c.Add("aa", fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, 1<<20)
			}
		}()
	}
	wg.Wait()
}
