package domain

// Encoder interface defines the methods that any encoder should implement.
type Encoder interface {
	Packing() int
	Encode(data []byte) string
	Decode(encoded string) ([]byte, error)
	Root() string
	Length() int
	Parent(hash string) string
	NewID() []byte
	RandomName() (string, error)
}
