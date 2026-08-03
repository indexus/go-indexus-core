package core

import (
	"fmt"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Routing() ([]domain.Contact, error) {
	return n.traverseRouting(true), nil
}

func (n *Node) Acknowledged() ([]domain.Contact, error) {
	return n.traverseAcknowledged(true), nil
}

func (n *Node) Registered() ([]domain.Contact, error) {
	return n.traverseRegistered(true), nil
}

func (n *Node) Ownership() (map[string]map[string]map[string]any, error) {
	collections := make(map[string]map[string]map[string]any)

	for _, collection := range n.collections.List() {
		collections[collection.Name()] = make(map[string]map[string]any)

		collection.Browse(
			func(ownership string) {
				collections[collection.Name()][ownership] = make(map[string]any)
			},
			func(ownership, delegation string) {
				collections[collection.Name()][ownership][delegation] = nil
			},
		)
	}
	return collections, nil
}

func (n *Node) Count() (int, error) {
	total := 0

	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				return
			}
			collection.Traverse(
				key.Location,
				func(s string, a *domain.Abelian) {},
				func(s1, s2, s3 string, a *domain.Abelian) { total++ },
			)
		}
	})
	return total, nil
}

func (n *Node) Check() []string {
	result := make([]string, 0)

	// Ownerships and owned keys are collected before being looked up: holding a
	// collection lock while taking the owned tree lock (or the reverse) would
	// deadlock against the ingest path.
	for _, collection := range n.collections.List() {
		ownerships := make([]string, 0)
		collection.Browse(
			func(ownership string) {
				ownerships = append(ownerships, ownership)
			},
			func(ownership, delegation string) {},
		)

		for _, ownership := range ownerships {
			id, err := encoding.MergeEncodings(
				encoding.BASE64,
				encoding.BASE64,
				ownership,
				collection.Name(),
			)
			if err != nil {
				continue
			}

			_, ok := n.owned.Get(0, id)
			if !ok {
				result = append(result, fmt.Sprintf("%s:%s", collection.Name(), ownership))
			}
		}
	}

	owned := make([]domain.Key, 0)
	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			owned = append(owned, key)
		}
	})

	for _, key := range owned {
		collection, ok := n.collections.Get(key.Collection)
		if !ok {
			result = append(result, fmt.Sprintf("%s:%s", key.Collection, key.Location))
			continue
		}

		_, ok = collection.Get(key.Location)
		if !ok {
			result = append(result, fmt.Sprintf("%s:%s", key.Collection, key.Location))
		}
	}
	return result
}

func (n *Node) Queue() int {
	return n.queue.Length()
}
