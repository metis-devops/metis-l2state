package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func treeKey(n int) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[24:], uint64(n))
	return h
}

func checkStateTree[V any](t *testing.T, tree *stateTree[V]) {
	t.Helper()
	depth := -1
	count := 0
	var check func(*stateNode[V], int)
	check = func(n *stateNode[V], level int) {
		if n == nil {
			t.Fatal("nil child")
		}
		if len(n.children) == 0 {
			if len(n.items) == 0 || len(n.items) > stateLeafSize || cap(n.items) > stateLeafSize {
				t.Fatal("invalid leaf capacity")
			}
			if depth < 0 {
				depth = level
			}
			if depth != level {
				t.Fatal("unbalanced leaf depth")
			}
			if n.max != n.items[len(n.items)-1].key {
				t.Fatal("leaf upper bound")
			}
			for i, item := range n.items {
				if i > 0 && bytes.Compare(n.items[i-1].key[:], item.key[:]) >= 0 {
					t.Fatal("unordered leaf")
				}
			}
			count += len(n.items)
			return
		}
		if len(n.children) > stateFanout || len(n.items) != 0 {
			t.Fatal("invalid branch")
		}
		for i, c := range n.children {
			if i > 0 && bytes.Compare(n.children[i-1].max[:], c.max[:]) >= 0 {
				t.Fatal("unordered branch")
			}
			check(c, level+1)
		}
		if n.max != n.children[len(n.children)-1].max {
			t.Fatal("branch upper bound")
		}
	}
	if tree.root != nil {
		check(tree.root, 0)
	}
	if count != tree.Len() {
		t.Fatalf("length %d != %d", count, tree.Len())
	}
}

func TestStateTreeBulkMutationAndPruning(t *testing.T) {
	var tree stateTree[common.Hash]
	const size = stateLeafSize*stateFanout*stateFanout + 1
	for i := range size {
		must(t, tree.Append(treeKey(2*i), treeKey(i+1)))
	}
	checkStateTree(t, &tree)
	if err := tree.Append(treeKey(0), common.Hash{}); err == nil {
		t.Fatal("unordered append accepted")
	}
	rng := rand.New(rand.NewPCG(1, 2))
	for _, i := range rng.Perm(10000) {
		tree.Set(treeKey(2*i+1), treeKey(i+5))
		tree.Set(treeKey(2*i), treeKey(i+3))
	}
	checkStateTree(t, &tree)
	for i := range 10000 {
		if tree.Value(treeKey(2*i+1)) != treeKey(i+5) || tree.Value(treeKey(2*i)) != treeKey(i+3) {
			t.Fatal("incorrect replacement/insertion")
		}
	}
	for _, i := range rng.Perm(10000) {
		tree.Delete(treeKey(2*i + 1))
		tree.Delete(treeKey(2*i + 1))
	}
	for i := range size {
		tree.Delete(treeKey(2 * i))
		if i%4096 == 0 {
			checkStateTree(t, &tree)
		}
	}
	checkStateTree(t, &tree)
	if tree.root != nil {
		t.Fatal("empty root retained")
	}
}

func TestStateTreeRandomOperationsMatchMap(t *testing.T) {
	var tree stateTree[uint64]
	want := make(map[common.Hash]uint64)
	rng := rand.New(rand.NewPCG(3, 4))
	for i := range 30000 {
		k, v := treeKey(rng.IntN(5000)), rng.Uint64()
		if rng.IntN(3) == 0 {
			tree.Delete(k)
			delete(want, k)
		} else {
			tree.Set(k, v)
			want[k] = v
		}
		if i%300 == 0 {
			checkStateTree(t, &tree)
			keys := sortedHashes(want)
			offset := 0
			must(t, tree.Visit(func(k common.Hash, v uint64) error {
				if k != keys[offset] || v != want[k] {
					t.Fatal("tree differs from map")
				}
				offset++
				return nil
			}))
			if offset != len(want) {
				t.Fatal("lost entries")
			}
			for k, v := range want {
				if got, ok := tree.Get(k); !ok || got != v {
					t.Fatal("lookup differs")
				}
			}
		}
	}
}

func TestStateTreeDrainAcknowledgementAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		var tree stateTree[*int]
		for i := range stateLeafSize * 3 {
			v := i
			must(t, tree.Append(treeKey(i), &v))
		}
		root := tree.root
		leaves := append([]*stateNode[*int](nil), root.children...)
		calls, acks := 0, 0
		stop := errors.New("writer failed")
		err := tree.Drain(func(k common.Hash, v *int) error {
			if k != treeKey(calls) || *v != calls {
				t.Fatal("drain order")
			}
			calls++
			return nil
		}, func() error {
			if len(leaves[acks].items) == 0 {
				t.Fatal("leaf freed before acknowledgement")
			}
			if acks > 0 && leaves[acks-1].items != nil {
				t.Fatal("previous leaf retained")
			}
			acks++
			if fail {
				return stop
			}
			return nil
		})
		if fail && !errors.Is(err, stop) || !fail && err != nil {
			t.Fatal(err)
		}
		if tree.root != nil || tree.Len() != 0 || len(root.children) != 0 && !fail {
			t.Fatal("drain retained root")
		}
		if !fail && (calls != stateLeafSize*3 || acks != 3) {
			t.Fatal("incorrect drain batches")
		}
		if leaves[0].items != nil {
			t.Fatal("acknowledged or failed leaf retained")
		}
	}
}
