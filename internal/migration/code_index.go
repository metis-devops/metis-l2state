package migration

import "github.com/ethereum/go-ethereum/common"

type codeUse uint8

const (
	convertedCode codeUse = 1 << iota
	finalCode
	emittedCode
)

type codeReference struct {
	hash common.Hash
	use  codeUse
}

// codeHashIndex records first-reference order. One owner merges stage membership
// through stable indexes; fixed-size blocks never copy the complete directory.
type codeHashIndex struct {
	ids   map[common.Hash]uint64
	items snapshotBlocks[codeReference]
}

func (x *codeHashIndex) Add(hash common.Hash, use codeUse) {
	if x.ids == nil {
		x.ids = make(map[common.Hash]uint64)
		x.items.perBlock = 1024
	}
	if id, ok := x.ids[hash]; ok {
		x.items.ptr(id).use |= use
		return
	}
	x.ids[hash] = x.items.length
	x.items.append(codeReference{hash, use})
}

func (x *codeHashIndex) Visit(visit func(common.Hash, codeUse) error) error {
	for i := uint64(0); i < x.items.length; i++ {
		r := x.items.at(i)
		if err := visit(r.hash, r.use); err != nil {
			return err
		}
	}
	return nil
}
