package domain

import (
	"fmt"
	"sync"
	"testing"
)

// fakeBase is a minimal Encoder: single characters address the hierarchy, so a
// location of length N sits N levels below the root.
type fakeBase struct {
	length int
}

func (fakeBase) Packing() int                    { return 6 }
func (fakeBase) Encode(data []byte) string       { return string(data) }
func (fakeBase) Decode(s string) ([]byte, error) { return []byte(s), nil }
func (fakeBase) Root() string                    { return "@" }
func (b fakeBase) Length() int                   { return b.length }
func (fakeBase) NewID() []byte                   { return make([]byte, 16) }
func (fakeBase) RandomName() (string, error)     { return "x", nil }
func (fakeBase) Parent(hash string) string {
	if hash == "@" {
		return ""
	}
	if len(hash) == 1 {
		return "@"
	}
	return hash[:len(hash)-1]
}

func TestCollectionConcurrentAddAndGetMultiple(t *testing.T) {
	const (
		writers    = 4
		readers    = 4
		ops        = 500
		delegation = 8
	)

	c := NewCollection("coll", "@", fakeBase{length: 4})

	var wg sync.WaitGroup

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				location := fmt.Sprintf("%c%c", 'a'+w, 'a'+(i%26))
				c.Add(location, fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, delegation)
			}
		}()
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			locations := []string{"@", "a", "b", "c", "aa", "ab"}
			properties := []func(*Abelian) int{
				func(a *Abelian) int { return a.Count() },
			}
			for i := 0; i < ops; i++ {
				_, _ = c.GetMultiple(locations, 4, properties)
				_, _ = c.Get("@")
				c.Browse(func(string) {}, func(string, string) {})
			}
		}()
	}

	wg.Wait()
}

func TestCollectionConcurrentAddSameLocation(t *testing.T) {
	const (
		writers    = 8
		ops        = 250
		delegation = 8
	)

	c := NewCollection("coll", "@", fakeBase{length: 4})

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				c.Add("aa", fmt.Sprintf("id-%d-%d", w, i), []float64{1, 2, 3, 4, 5}, delegation)
			}
		}()
	}
	wg.Wait()
}
