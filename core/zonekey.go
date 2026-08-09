package core

import (
	"github.com/indexus/go-indexus-core/encoding"
)

// zoneKeyID merges location into the high bits and collection into the rest so
// subtree locality survives into the XOR keyspace (see zonekey_test.go).
func zoneKeyID(collection, location string) ([]byte, error) {
	return encoding.MergeEncodings(
		encoding.BASE64,
		encoding.BASE64,
		location,
		collection,
	)
}

func (n *Node) ownedZoneCount() int {
	return len(n.listOwnedKeys())
}
