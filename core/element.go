package core

import "gitlab.com/indexus/node/domain"

type Element struct {
	item    *domain.Item
	root    string
	current string
}

func NewElement(item *domain.Item, root, current string) *Element {
	return &Element{
		item:    item,
		root:    root,
		current: current,
	}
}
