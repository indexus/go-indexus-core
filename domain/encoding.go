package domain

// Encoder interface defines the methods that any encoder should implement.
type Encoder interface {
	Packing() int
	Encode(data []byte) string
	Decode(encoded string) ([]byte, error)
	Root() string
	Length() int
	CharAt(idx int) string
	Parent(hash string) string
	NewID() []byte
	RandomName() (string, error)
}

// ChildCandidates returns the direct child location ids under parent
// (one alphabet extension each). Used for local XOR targeting only —
// not as network probes.
func ChildCandidates(base Encoder, parent string) []string {
	if base == nil {
		return nil
	}
	n := base.Length()
	out := make([]string, 0, n)
	root := base.Root()
	for i := 0; i < n; i++ {
		ch := base.CharAt(i)
		if ch == "" {
			continue
		}
		if parent == root {
			out = append(out, ch)
		} else {
			out = append(out, parent+ch)
		}
	}
	return out
}

// IsDirectChild reports whether child is a one-hop encoding child of parent.
// Rejects empty keys, self-keys (child == parent), and deeper/unrelated hashes.
func IsDirectChild(base Encoder, parent, child string) bool {
	if base == nil || child == "" || child == parent {
		return false
	}
	return base.Parent(child) == parent
}
