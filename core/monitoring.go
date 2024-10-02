package core

import (
	"fmt"

	"gitlab.com/indexus/node/domain"
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

	n.owned.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				return
			}
			collection.Traverse(
				key.Location,
				func(s string, i int) {},
				func(s1, s2, s3 string) { total++ },
			)
		}
	})
	return total, nil
}

func (n *Node) Check() []string {
	result := make([]string, 0)

	for _, collection := range n.collections.List() {
		collection.Browse(
			func(ownership string) {
				id, err := domain.DecodeLocation(collection.Name(), ownership)
				if err != nil {
					return
				}

				_, ok := n.owned.Get(0, id)
				if !ok {
					result = append(result, fmt.Sprintf("%s:%s", collection.Name(), ownership))
				}
			},
			func(ownership, delegation string) {},
		)
	}

	n.owned.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, ok := n.collections.Get(key.Collection)
			if !ok {
				result = append(result, fmt.Sprintf("%s:%s", collection.Name(), key.Location))
			}

			_, ok = collection.Get(key.Location)
			if !ok {
				result = append(result, fmt.Sprintf("%s:%s", collection.Name(), key.Location))
			}
		}
	})
	return result
}

func (n *Node) Queue() int {
	return n.queue.Length()
}
