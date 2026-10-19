package migration

import (
	"bytes"
	"github.com/ethereum/go-ethereum/common"
)

const (
	codeDegree  = 16
	codeNodeMax = 2*codeDegree - 1
)

// btreeCodeIndex is an insert-only B-tree owned by one account accumulator.
// Each distinct hash is stored once with both stages' references. The zero value
// is ready to use. Visit needs only the tree's logarithmic recursion stack;
// it neither clones the index nor materializes a sorted hash slice.
type btreeCodeIndex struct {
	root *codeNode
}

type codeNode struct {
	items [codeNodeMax]codeReference
	n     int
	// Leaves do not allocate child arrays.
	children *[codeNodeMax + 1]*codeNode
}

func (x *btreeCodeIndex) Add(hash common.Hash, use codeUse) {
	if x.root == nil {
		x.root = new(codeNode)
	}
	if x.root.n == codeNodeMax {
		root := &codeNode{children: new([codeNodeMax + 1]*codeNode)}
		root.children[0] = x.root
		root.split(0)
		x.root = root
	}
	x.root.add(codeReference{hash, use})
}

// split promotes the full child's median into a non-full parent. Items and
// children move between fixed-size nodes, never into a whole-index copy.
func (n *codeNode) split(i int) {
	left := n.children[i]
	right := &codeNode{n: codeDegree - 1}
	copy(right.items[:], left.items[codeDegree:])
	if left.children != nil {
		right.children = new([codeNodeMax + 1]*codeNode)
		copy(right.children[:], left.children[codeDegree:])
		clear(left.children[codeDegree:])
	}
	copy(n.items[i+1:n.n+1], n.items[i:n.n])
	n.items[i] = left.items[codeDegree-1]
	copy(n.children[i+2:n.n+2], n.children[i+1:n.n+1])
	n.children[i+1] = right
	n.n++
	left.n = codeDegree - 1
	clear(left.items[left.n:])
}

func (n *codeNode) find(hash common.Hash) (int, bool) {
	lo, hi := 0, n.n
	for lo < hi {
		mid := lo + (hi-lo)/2
		switch bytes.Compare(hash[:], n.items[mid].hash[:]) {
		case -1:
			hi = mid
		case 0:
			return mid, true
		case 1:
			lo = mid + 1
		}
	}
	return lo, false
}

func (n *codeNode) add(item codeReference) {
	for {
		i, found := n.find(item.hash)
		if found {
			n.items[i].use |= item.use
			return
		}
		if n.children == nil {
			copy(n.items[i+1:n.n+1], n.items[i:n.n])
			n.items[i] = item
			n.n++
			return
		}
		if n.children[i].n == codeNodeMax {
			n.split(i)
			switch bytes.Compare(item.hash[:], n.items[i].hash[:]) {
			case 0:
				n.items[i].use |= item.use
				return
			case 1:
				i++
			}
		}
		n = n.children[i]
	}
}

// Visit visits each hash exactly once in byte order. The callback must not
// mutate the index. Its first error stops traversal and is returned unchanged.
func (x *btreeCodeIndex) Visit(visit func(common.Hash, codeUse) error) error {
	return x.root.ascend(visit)
}

func (n *codeNode) ascend(visit func(common.Hash, codeUse) error) error {
	if n == nil {
		return nil
	}
	for i := range n.n {
		if n.children != nil {
			if err := n.children[i].ascend(visit); err != nil {
				return err
			}
		}
		if err := visit(n.items[i].hash, n.items[i].use); err != nil {
			return err
		}
	}
	if n.children != nil {
		return n.children[n.n].ascend(visit)
	}
	return nil
}
