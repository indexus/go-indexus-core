package core

import "github.com/indexus/go-indexus-core/domain"

type OpKind int

const (
	OpAdd OpKind = iota
	OpDelete
)

func (o OpKind) String() string {
	if o == OpDelete {
		return "delete"
	}
	return "add"
}

type Element struct {
	item    *domain.Item
	root    string
	current string
	op      OpKind

	meter bool
}

func NewElement(item *domain.Item, root, current string) *Element {
	return &Element{
		item:    item,
		root:    root,
		current: current,
		op:      OpAdd,
		meter:   true,
	}
}

func NewTransferElement(item *domain.Item, root, current string) *Element {
	return &Element{
		item:    item,
		root:    root,
		current: current,
		op:      OpAdd,
		meter:   false,
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
