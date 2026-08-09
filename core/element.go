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

// Element is what the queue carries. Placement never stores a walk position:
// every attempt climbs from item.Location. via names peers that already
// declined on this attempt (cycle guard); it is cleared on requeue.
type Element struct {
	item *domain.Item
	root string
	via  domain.Visited
	op   OpKind

	meter bool
}

func NewElement(item *domain.Item, root string) *Element {
	return &Element{
		item:  item,
		root:  root,
		op:    OpAdd,
		meter: true,
	}
}

func NewTransferElement(item *domain.Item, root string) *Element {
	return &Element{
		item:  item,
		root:  root,
		op:    OpAdd,
		meter: false,
	}
}

func NewDeleteElement(item *domain.Item, root string) *Element {
	return &Element{
		item: item,
		root: root,
		op:   OpDelete,
	}
}
