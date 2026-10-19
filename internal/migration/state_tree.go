package migration

import (
	"bytes"
	"errors"

	"github.com/ethereum/go-ethereum/common"
)

// stateTree is a mutable B+ tree. Leaves own inline key/value pairs; branches
// own only child pointers and their upper bounds. Sorted capture fills nodes
// from left to right without a whole-state staging slice. Random conversion
// writes split full nodes. Deletion prunes empty nodes, without copying surviving
// entries into an overlay or retaining tombstones. All leaves remain at one depth.
const (
	stateLeafSize = 128
	stateFanout   = 32
)

type stateItem[V any] struct {
	key   common.Hash
	value V
}
type stateNode[V any] struct {
	max      common.Hash
	items    []stateItem[V]
	children []*stateNode[V]
}
type stateTree[V any] struct {
	root  *stateNode[V]
	count int
}

func (t *stateTree[V]) Len() int { return t.count }
func (t *stateTree[V]) Get(key common.Hash) (V, bool) {
	n := t.root
	for n != nil && len(n.children) != 0 {
		n = n.children[n.childIndex(key)]
	}
	if n != nil {
		i := n.itemIndex(key)
		if i < len(n.items) && n.items[i].key == key {
			return n.items[i].value, true
		}
	}
	var zero V
	return zero, false
}
func (t *stateTree[V]) Value(key common.Hash) V { v, _ := t.Get(key); return v }
func (n *stateNode[V]) childIndex(key common.Hash) int {
	lo, hi := 0, len(n.children)-1
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(n.children[mid].max[:], key[:]) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func (n *stateNode[V]) itemIndex(key common.Hash) int {
	lo, hi := 0, len(n.items)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(n.items[mid].key[:], key[:]) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}
func newStateLeaf[V any](key common.Hash, value V) *stateNode[V] {
	items := make([]stateItem[V], 1)
	items[0] = stateItem[V]{key, value}
	return &stateNode[V]{max: key, items: items}
}
func (n *stateNode[V]) refresh() {
	if len(n.children) != 0 {
		n.max = n.children[len(n.children)-1].max
	} else if len(n.items) != 0 {
		n.max = n.items[len(n.items)-1].key
	}
}
func (t *stateTree[V]) grow(right *stateNode[V]) {
	if right == nil {
		return
	}
	children := make([]*stateNode[V], 2, stateFanout)
	children[0], children[1] = t.root, right
	t.root = &stateNode[V]{max: right.max, children: children}
}

// Append is the streaming bulk builder. Reject duplicates and disorder instead
// of silently replacing validated source records. Existing full nodes stay full.
func (t *stateTree[V]) Append(key common.Hash, value V) error {
	if t.root == nil {
		t.root = newStateLeaf(key, value)
		t.count = 1
		return nil
	}
	if bytes.Compare(t.root.max[:], key[:]) >= 0 {
		return errors.New("unordered state tree append")
	}
	t.grow(t.root.append(key, value))
	t.count++
	return nil
}
func (n *stateNode[V]) append(key common.Hash, value V) *stateNode[V] {
	if len(n.children) == 0 {
		if len(n.items) == stateLeafSize {
			return newStateLeaf(key, value)
		}
		n.reserveItem()
		n.items = append(n.items, stateItem[V]{key, value})
		n.max = key
		return nil
	}
	right := n.children[len(n.children)-1].append(key, value)
	n.refresh()
	if right == nil {
		return nil
	}
	if len(n.children) == stateFanout {
		children := make([]*stateNode[V], 1, stateFanout)
		children[0] = right
		return &stateNode[V]{max: right.max, children: children}
	}
	n.children = append(n.children, right)
	n.max = right.max
	return nil
}
func (t *stateTree[V]) Set(key common.Hash, value V) {
	if t.root == nil {
		t.root = newStateLeaf(key, value)
		t.count = 1
		return
	}
	right, added := t.root.set(key, value)
	t.grow(right)
	if added {
		t.count++
	}
}
func (n *stateNode[V]) set(key common.Hash, value V) (*stateNode[V], bool) {
	if len(n.children) == 0 {
		return n.setItem(key, value)
	}
	i := n.childIndex(key)
	right, added := n.children[i].set(key, value)
	if right != nil {
		n.children = append(n.children, nil)
		copy(n.children[i+2:], n.children[i+1:])
		n.children[i+1] = right
	}
	n.refresh()
	if len(n.children) <= stateFanout {
		return nil, added
	}
	mid := len(n.children) / 2
	children := make([]*stateNode[V], len(n.children)-mid, stateFanout)
	copy(children, n.children[mid:])
	clear(n.children[mid:])
	n.children = n.children[:mid]
	n.refresh()
	return &stateNode[V]{max: children[len(children)-1].max, children: children}, added
}
func (n *stateNode[V]) setItem(key common.Hash, value V) (*stateNode[V], bool) {
	i := n.itemIndex(key)
	if i < len(n.items) && n.items[i].key == key {
		n.items[i].value = value
		return nil, false
	}
	if len(n.items) == stateLeafSize {
		mid := len(n.items) / 2
		items := make([]stateItem[V], len(n.items)-mid, stateLeafSize)
		copy(items, n.items[mid:])
		clear(n.items[mid:])
		n.items = n.items[:mid]
		n.refresh()
		right := &stateNode[V]{max: items[len(items)-1].key, items: items}
		if bytes.Compare(key[:], n.max[:]) <= 0 {
			n.insertItem(i, key, value)
		} else {
			right.insertItem(i-mid, key, value)
		}
		return right, true
	}
	n.insertItem(i, key, value)
	return nil, true
}

// Explicit capacities avoid allocator size-class rounding retaining more than
// one leaf's entries, while tiny per-account storage starts with one entry.
func (n *stateNode[V]) reserveItem() {
	if len(n.items) < cap(n.items) {
		return
	}
	items := make([]stateItem[V], len(n.items), min(stateLeafSize, max(1, 2*cap(n.items))))
	copy(items, n.items)
	n.items = items
}
func (n *stateNode[V]) insertItem(i int, key common.Hash, value V) {
	n.reserveItem()
	n.items = append(n.items, stateItem[V]{})
	copy(n.items[i+1:], n.items[i:])
	n.items[i] = stateItem[V]{key, value}
	n.refresh()
}
func (t *stateTree[V]) Delete(key common.Hash) {
	if t.root == nil || !t.root.remove(key) {
		return
	}
	t.count--
	if t.count == 0 {
		t.root = nil
		return
	}
	for len(t.root.children) == 1 {
		t.root = t.root.children[0]
	}
}
func (n *stateNode[V]) remove(key common.Hash) bool {
	if len(n.children) == 0 {
		i := n.itemIndex(key)
		if i == len(n.items) || n.items[i].key != key {
			return false
		}
		copy(n.items[i:], n.items[i+1:])
		clear(n.items[len(n.items)-1:])
		n.items = n.items[:len(n.items)-1]
		n.refresh()
		return true
	}
	i := n.childIndex(key)
	child := n.children[i]
	if !child.remove(key) {
		return false
	}
	if len(child.items) == 0 && len(child.children) == 0 {
		copy(n.children[i:], n.children[i+1:])
		n.children[len(n.children)-1] = nil
		n.children = n.children[:len(n.children)-1]
	}
	n.refresh()
	return true
}
func (t *stateTree[V]) Visit(visit func(common.Hash, V) error) error { return t.root.visit(visit) }
func (n *stateNode[V]) visit(visit func(common.Hash, V) error) error {
	if n == nil {
		return nil
	}
	for _, child := range n.children {
		if err := child.visit(visit); err != nil {
			return err
		}
	}
	for _, item := range n.items {
		if err := visit(item.key, item.value); err != nil {
			return err
		}
	}
	return nil
}

// Drain transfers exclusive ownership to a one-shot traversal. Each leaf is
// released only after its entries and the acknowledgement callback succeed.
// A failure drops all remaining state: consumed output cannot be retried. No
// freelist, clone, iterator, or historical root retains consumed nodes.
func (t *stateTree[V]) Drain(visit func(common.Hash, V) error, ack func() error) error {
	root := t.root
	t.root = nil
	t.count = 0
	return root.drain(visit, ack)
}
func (n *stateNode[V]) drain(visit func(common.Hash, V) error, ack func() error) error {
	if n == nil {
		return nil
	}
	if len(n.children) != 0 {
		for i := range n.children {
			child := n.children[i]
			n.children[i] = nil
			if err := child.drain(visit, ack); err != nil {
				clear(n.children)
				return err
			}
		}
		n.children = nil
		return nil
	}
	for _, item := range n.items {
		if err := visit(item.key, item.value); err != nil {
			clear(n.items)
			n.items = nil
			return err
		}
	}
	if ack != nil {
		if err := ack(); err != nil {
			clear(n.items)
			n.items = nil
			return err
		}
	}
	clear(n.items)
	n.items = nil
	return nil
}
