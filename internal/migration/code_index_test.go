package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func codeIndexEntries(x *codeHashIndex) map[common.Hash]codeUse {
	entries := make(map[common.Hash]codeUse)
	_ = x.Visit(func(h common.Hash, use codeUse) error {
		entries[h] = use
		return nil
	})
	return entries
}

func TestCodeHashIndex(t *testing.T) {
	for _, order := range []string{"ascending", "descending", "shuffled"} {
		t.Run(order, func(t *testing.T) {
			keys := make([]common.Hash, 20000)
			for i := range keys {
				binary.BigEndian.PutUint64(keys[i][24:], uint64(i))
			}
			keys[len(keys)-1] = common.Hash(bytes.Repeat([]byte{255}, 32))
			switch order {
			case "descending":
				slices.Reverse(keys)
			case "shuffled":
				rand.New(rand.NewPCG(1, 2)).Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			}
			var x codeHashIndex
			want := make(map[common.Hash]codeUse)
			for i, h := range keys {
				use := codeUse(i%3 + 1)
				x.Add(h, use)
				want[h] |= use
			}
			// Duplicate references merge membership without moving indexes.
			for i, h := range keys {
				use := codeUse((i+1)%3 + 1)
				x.Add(h, use)
				want[h] |= use
			}
			if !maps.Equal(want, codeIndexEntries(&x)) {
				t.Fatal("lost hash or stage membership")
			}
			var got []common.Hash
			must(t, x.Visit(func(h common.Hash, _ codeUse) error { got = append(got, h); return nil }))
			if !slices.Equal(got, keys) {
				t.Fatal("duplicate or unordered traversal")
			}
			for _, stop := range []int{1, 15, 16, 31, 1000, len(keys)} {
				visits := 0
				err := x.Visit(func(common.Hash, codeUse) error {
					visits++
					if visits == stop {
						return context.Canceled
					}
					return nil
				})
				if !errors.Is(err, context.Canceled) || visits != stop {
					t.Fatalf("traversal ignored stop at %d: visits=%d err=%v", stop, visits, err)
				}
			}
		})
	}
	var empty codeHashIndex
	must(t, empty.Visit(func(common.Hash, codeUse) error { t.Fatal("empty callback"); return nil }))
}

func checkCodeNode(t *testing.T, n *codeNode, root bool, depth int, leafDepth *int) {
	t.Helper()
	if n == nil || n.n < 1 || n.n > codeNodeMax || (!root && n.n < codeDegree-1) {
		t.Fatal("invalid B-tree occupancy")
	}
	for _, item := range n.items[n.n:] {
		if item != (codeReference{}) {
			t.Fatal("split retained inactive hash")
		}
	}
	if n.children == nil {
		if *leafDepth == -1 {
			*leafDepth = depth
		}
		if *leafDepth != depth {
			t.Fatal("unbalanced B-tree")
		}
		return
	}
	for _, child := range n.children[:n.n+1] {
		checkCodeNode(t, child, false, depth+1, leafDepth)
	}
	for _, child := range n.children[n.n+1:] {
		if child != nil {
			t.Fatal("split retained inactive subtree")
		}
	}
}

// Preserve the old map/clone/sort algorithm as a benchmark and test oracle.
// shared reproduces the no-alloc walk's aliased maps.
type mapCodeIndex struct {
	converted, final map[common.Hash]struct{}
	shared           bool
}

func newMapCodeIndex(shared bool) *mapCodeIndex {
	x := &mapCodeIndex{converted: make(map[common.Hash]struct{}), shared: shared}
	if shared {
		x.final = x.converted
	} else {
		x.final = make(map[common.Hash]struct{})
	}
	return x
}

func (x *mapCodeIndex) Add(h common.Hash, use codeUse) {
	if use&convertedCode != 0 {
		x.converted[h] = struct{}{}
	}
	if use&finalCode != 0 && !x.shared {
		x.final[h] = struct{}{}
	}
}

func (x *mapCodeIndex) Visit(visit func(common.Hash, codeUse) error) error {
	set := maps.Clone(x.final)
	maps.Copy(set, x.converted)
	for _, h := range sortedHashes(set) {
		var use codeUse
		if _, ok := x.final[h]; ok {
			use |= finalCode
		}
		if _, ok := x.converted[h]; ok {
			use |= convertedCode
		}
		if err := visit(h, use); err != nil {
			return err
		}
	}
	return nil
}

type codeIndex interface {
	Add(common.Hash, codeUse)
	Visit(func(common.Hash, codeUse) error) error
}

// Benchmark the entire collection + ordered traversal lifecycle, including
// duplicate references. Hash generation is untimed. Each sample should run in
// a fresh process with benchtime=1x; no bytecode or DB I/O is simulated here.
func BenchmarkCodeHashIndex(b *testing.B) {
	for _, size := range []int{10000, 100000, 1000000} {
		for _, mode := range []string{"shared", "overlap", "repeated"} {
			for _, implementation := range []string{"map", "btree", "ordered"} {
				b.Run(fmt.Sprintf("%d/%s/%s", size, mode, implementation), func(b *testing.B) {
					benchmarkCodeHashIndex(b, size, mode, implementation)
				})
			}
		}
	}
}

func benchmarkCodeHashIndex(b *testing.B, size int, mode, implementation string) {
	keys := make([]common.Hash, size)
	for i := range keys {
		var raw [8]byte
		binary.BigEndian.PutUint64(raw[:], uint64(i))
		keys[i] = sha256.Sum256(raw[:])
	}
	repeats := 1
	if mode == "repeated" {
		repeats = 10
	}
	var want uint64
	for i, h := range keys {
		use := benchmarkCodeUse(i, mode)
		want += binary.BigEndian.Uint64(h[:8]) * uint64(use)
	}
	b.ReportAllocs()
	for b.Loop() {
		var x codeIndex
		switch implementation {
		case "map":
			x = newMapCodeIndex(mode != "overlap")
		case "btree":
			x = new(btreeCodeIndex)
		default:
			x = new(codeHashIndex)
		}
		for range repeats {
			for i, h := range keys {
				use := benchmarkCodeUse(i, mode)
				if mode == "overlap" {
					// Overlay walks encounter the two stages separately.
					if use&convertedCode != 0 {
						x.Add(h, convertedCode)
					}
					if use&finalCode != 0 {
						x.Add(h, finalCode)
					}
				} else {
					x.Add(h, use)
				}
			}
		}
		var sum uint64
		var count int
		err := x.Visit(func(h common.Hash, use codeUse) error {
			sum += binary.BigEndian.Uint64(h[:8]) * uint64(use)
			count++
			return nil
		})
		b.StopTimer()
		if err != nil || sum != want || count != size {
			b.Fatalf("index lost entries: count=%d sum=%d err=%v", count, sum, err)
		}
		b.StartTimer()
	}
}

func benchmarkCodeUse(i int, mode string) codeUse {
	if mode == "overlap" {
		return codeUse(i%3 + 1)
	}
	return convertedCode | finalCode
}

func TestHistoricalBTreeOracle(t *testing.T) {
	var x btreeCodeIndex
	for i := range 20000 {
		x.Add(common.BigToHash(new(big.Int).SetInt64(int64(i))), finalCode)
	}
	depth := -1
	checkCodeNode(t, x.root, true, 0, &depth)
}
