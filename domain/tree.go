package domain

import (
	"sync"
)

type tree[N any] struct {
	left, right *tree[N]
	node        N
}

func (t *tree[N]) Insert(idx int, id []byte, node N) {

	if idx/8 == len(id) {
		t.node = node
		return
	}

	if (id[idx/8] >> (7 - idx%8) & 1) == 0 {
		if t.left == nil {
			t.left = &tree[N]{}
		}
		t.left.Insert(idx+1, id, node)
	} else {
		if t.right == nil {
			t.right = &tree[N]{}
		}
		t.right.Insert(idx+1, id, node)
	}
}

func (t *tree[N]) Traverse(idx int, value []byte, process func(int, []byte, N)) {

	if t.left != nil {
		tmp := make([]byte, len(value))
		copy(tmp, value)
		t.left.Traverse(idx+1, tmp, process)
	}

	if t.right != nil {
		tmp := make([]byte, len(value))
		copy(tmp, value)
		tmp[idx/8] |= 1 << (7 - uint(idx%8))
		t.right.Traverse(idx+1, tmp, process)
	}

	if idx > 0 && t.left == nil && t.right == nil {
		process(idx, value, t.node)
	}
}

func (t *tree[N]) Range(idx int, owner, candidate, value []byte, process func(int, []byte, N)) {

	tmp := make([]byte, len(value))
	copy(tmp, value)

	if idx/8 == len(owner) {
		return
	}

	oBit := (owner[idx/8] >> (7 - idx%8) & 1) == 1
	cBit := (candidate[idx/8] >> (7 - idx%8) & 1) == 1

	if oBit != cBit {
		if !cBit && t.left != nil {
			t.left.Traverse(idx+1, tmp, process)
		} else if cBit && t.right != nil {
			tmp[idx/8] |= 1 << (7 - uint(idx%8))
			t.right.Traverse(idx+1, tmp, process)
		}
		return
	}

	if t.left != nil {
		t.left.Range(idx+1, owner, candidate, tmp, process)
	}
	if t.right != nil {
		tmpRight := make([]byte, len(tmp))
		copy(tmpRight, tmp)
		tmpRight[idx/8] |= 1 << (7 - uint(idx%8))
		t.right.Range(idx+1, owner, candidate, tmpRight, process)
	}
}

func (t *tree[N]) Truncate(idx int, owner, branch []byte) {

	if idx/8 == len(owner) {
		return
	}

	oBit := (owner[idx/8] >> (7 - idx%8) & 1) == 1
	bBit := (branch[idx/8] >> (7 - idx%8) & 1) == 1

	if oBit != bBit {
		if bBit && t.right != nil {
			t.right = nil
		} else if !bBit && t.left != nil {
			t.left = nil
		}
		return
	}

	if t.left != nil {
		t.left.Truncate(idx+1, owner, branch)
	}
	if t.right != nil {
		t.right.Truncate(idx+1, owner, branch)
	}
}

func (t *tree[N]) Update(idx int, value []byte, process func(int, []byte, N)) bool {

	if idx/8 == len(value) {
		process(idx, value, t.node)
		return true
	}

	bit := (value[idx/8] >> (7 - idx%8) & 1) == 1

	if t.left != nil && !bit {
		return t.left.Update(idx+1, value, process)
	} else if t.right != nil && bit {
		return t.right.Update(idx+1, value, process)
	}

	return false
}

func (t *tree[N]) Get(idx int, value []byte) (N, bool) {

	if idx/8 == len(value) {
		return t.node, true
	}

	bit := (value[idx/8] >> (7 - idx%8) & 1) == 1

	if t.left != nil && !bit {
		return t.left.Get(idx+1, value)
	} else if t.right != nil && bit {
		return t.right.Get(idx+1, value)
	}

	return t.node, false
}

func (t *tree[N]) Nearest(idx int, value []byte) N {

	if idx/8 == len(value) {
		return t.node
	}

	bit := (value[idx/8] >> (7 - idx%8) & 1) == 1

	if t.left != nil && (!bit || t.right == nil) {
		return t.left.Nearest(idx+1, value)
	} else if t.right != nil {
		return t.right.Nearest(idx+1, value)
	}

	return t.node
}

func (t *tree[N]) Remove(idx int, value []byte) bool {

	if idx/8 == len(value) {
		return true
	}

	bit := (value[idx/8] >> (7 - idx%8) & 1) == 1

	if t.left != nil && !bit {
		if t.left.Remove(idx+1, value) {
			t.left = nil
			return t.right == nil
		}
	} else if t.right != nil && bit {
		if t.right.Remove(idx+1, value) {
			t.right = nil
			return t.left == nil
		}
	}

	return false
}

func (t *tree[N]) Extract(idx int, value []byte, routing *[160]N) {

	if idx/8 == len(value) {
		return
	}

	bit := (value[idx/8] >> (7 - idx%8) & 1) == 1

	if t.left != nil && !bit {
		t.left.Extract(idx+1, value, routing)
	} else if t.right != nil && bit {
		t.right.Extract(idx+1, value, routing)
	}

	if t.left != nil && bit {
		routing[idx] = t.left.Nearest(idx+1, value)
	} else if t.right != nil && !bit {
		routing[idx] = t.right.Nearest(idx+1, value)
	}
}

type BST[N any] struct {
	mu   *sync.Mutex
	tree *tree[N]
}

func NewBST[N any]() *BST[N] {
	return &BST[N]{
		mu:   &sync.Mutex{},
		tree: &tree[N]{},
	}
}

func (bst *BST[N]) Insert(idx int, id []byte, node N) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Insert(idx, id, node)
}

func (bst *BST[N]) Update(idx int, id []byte, process func(int, []byte, N)) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Update(idx, id, process)
}

func (bst *BST[N]) Upsert(idx int, id []byte, new N, process func(int, []byte, N)) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	_, exist := bst.tree.Get(0, id)
	if !exist {
		bst.tree.Insert(0, id, new)
	}

	bst.tree.Update(idx, id, process)
}

func (bst *BST[N]) Traverse(idx int, value []byte, process func(int, []byte, N)) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Traverse(idx, value, process)
}

func (bst *BST[N]) Range(idx int, owner, candidate, value []byte, process func(int, []byte, N)) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Range(idx, owner, candidate, value, process)
}

func (bst *BST[N]) Truncate(idx int, owner, branch []byte) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Truncate(idx, owner, branch)
}

func (bst *BST[N]) Get(idx int, value []byte) (N, bool) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	return bst.tree.Get(idx, value)
}

func (bst *BST[N]) Nearest(idx int, value []byte) N {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	return bst.tree.Nearest(idx, value)
}

func (bst *BST[N]) Remove(idx int, value []byte) bool {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	return bst.tree.Remove(idx, value)
}

func (bst *BST[N]) Extract(idx int, value []byte, routing *[160]N) {
	bst.mu.Lock()
	defer bst.mu.Unlock()

	bst.tree.Extract(idx, value, routing)
}
