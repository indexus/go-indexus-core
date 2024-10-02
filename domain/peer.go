package domain

type Peer interface {
	ID() []byte
	Name() string
}
