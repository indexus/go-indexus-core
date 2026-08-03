package core

import "github.com/indexus/go-indexus-core/domain"

type OpKind int

const (
	OpAdd OpKind = iota
	OpDelete
)

type Element struct {
	item    *domain.Item
	root    string
	current string
	op      OpKind
}

func NewElement(item *domain.Item, root, current string) *Element {
	return &Element{
		item:    item,
		root:    root,
		current: current,
		op:      OpAdd,
	}
}

func NewDeleteElement(item *domain.Item, root, current string) *Element {
	return &Element{
		item:    item,
		root:    root,
		current: current,
		op:      OpDelete,
	}
}
