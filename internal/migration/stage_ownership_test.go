package migration

import (
	"bytes"
	"fmt"
	"maps"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
)

// Exercise simultaneous conversion/alloc storage changes, including deletion of
// the entire final storage. Record consumers must not alias internal balances.
func TestAccountStageOwnership(t *testing.T) {
	f := newFixture(t, 1025, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	h := crypto.Keccak256Hash(ordinary[:])
	r := accountReader{s: s}
	original, err := r.get(h)
	must(t, err)
	before, err := rlp.EncodeToBytes(original)
	must(t, err)
	keys := sortedHashes(f.storage)
	cp := &accountPatch{Address: ordinary, Balance: uint256.NewInt(77), Storage: map[common.Hash]common.Hash{keys[0]: common.HexToHash("0x99"), keys[1]: {}}}
	convertedStorage := maps.Clone(f.storage)
	convertedStorage[keys[0]] = common.HexToHash("0x99")
	delete(convertedStorage, keys[1])
	deleted := make(map[common.Hash]common.Hash)
	for k := range convertedStorage {
		deleted[k] = common.Hash{}
	}
	zero := uint64(0)
	emptyCode := []byte{}
	cases := []struct {
		name    string
		mode    walkMode
		alloc   *accountPatch
		storage map[common.Hash]common.Hash
		balance uint64
		reuse   bool
	}{
		{"shared", convertState, nil, convertedStorage, 77, false},
		{"noop-overlay", overlayState, &accountPatch{Address: ordinary}, convertedStorage, 77, false},
		{"explicit-zero-scalars", overlayState, &accountPatch{Address: ordinary, Balance: new(uint256.Int), Nonce: &zero, Code: &emptyCode}, convertedStorage, 0, false},
		{"delete-all-storage", overlayState, &accountPatch{Address: ordinary, Storage: deleted}, map[common.Hash]common.Hash{}, 77, false},
	}
	for _, tc := range cases {
		tc.name += "-validated"
		tc.reuse = true
		cases = append(cases, tc)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := newIndexes()
			must(t, x.addAddress(ordinary))
			emitted := make(map[common.Hash]common.Hash)
			var done *accountDone
			var end common.Hash
			var count uint64
			must(t, s.processAccount(t.Context(), accountJob{hash: h, original: original, converted: cp, final: tc.alloc}, make(limiter, 2), x, walkPlan{mode: tc.mode, reuseSource: tc.reuse}, func(e accountEvent) error {
				if e.done != nil {
					done = e.done
					return nil
				}
				switch e.record.Kind {
				case statefile.AccountStart:
					e.record.Account.Balance.SetInt64(-1)
				case statefile.StorageEntry:
					emitted[common.Hash(e.record.Storage.Key)] = e.record.Storage.Value
				case statefile.AccountEnd:
					end, count = e.record.StorageRoot, e.record.StorageCount
				}
				return nil
			}))
			if done == nil || !done.hasConverted || !done.hasFinal {
				t.Fatal("lost account existence")
			}
			if !maps.Equal(emitted, tc.storage) || count != uint64(len(tc.storage)) || done.finalSlots != count || done.convertedSlots != uint64(len(convertedStorage)) {
				t.Fatal("storage stages contaminated")
			}
			if done.converted.Root != expectedStorageRoot(t, convertedStorage) || done.final.Root != expectedStorageRoot(t, tc.storage) || end != done.final.Root {
				t.Fatal("wrong stage root")
			}
			if done.converted.Balance.Uint64() != 77 || done.final.Balance.Uint64() != tc.balance || done.converted.Nonce != original.Nonce || !bytes.Equal(done.converted.CodeHash, original.CodeHash) {
				t.Fatal("alloc/consumer changed conversion scalars")
			}
			if tc.alloc != nil && tc.alloc.Code != nil && (done.final.Nonce != 0 || !bytes.Equal(done.final.CodeHash, types.EmptyCodeHash[:])) {
				t.Fatal("explicit zero lost")
			}
			after, err := rlp.EncodeToBytes(original)
			must(t, err)
			if !bytes.Equal(before, after) || cp.Balance.Uint64() != 77 || cp.Storage[keys[0]] != common.HexToHash("0x99") || len(cp.Storage) != 2 {
				t.Fatal("input state mutated")
			}
		})
	}
}

func expectedStorageRoot(t *testing.T, storage map[common.Hash]common.Hash) common.Hash {
	t.Helper()
	stack := trie.NewStackTrie(nil)
	for _, k := range sortedHashes(storage) {
		must(t, stack.Update(k[:], slotRLP(storage[k])))
	}
	return stack.Hash()
}

// Each walk owns its accumulators. Shared immutable indexes and patches may be
// read concurrently; even a consumer modifying its records cannot change roots.
func TestConcurrentWalkStageIsolation(t *testing.T) {
	f := newFixture(t, 1025, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	in, err := loadInputs(t.Context(), f.options("unused"))
	must(t, err)
	l := make(limiter, 2)
	x, err := s.prepare(t.Context(), 2, l, f.validatedEvents(t))
	must(t, err)
	patches, _, err := s.convertLegacy(t.Context(), x, in, 2, l)
	must(t, err)
	consume := func(r statefile.Record) error {
		if r.Kind == statefile.AccountStart {
			r.Account.Balance.SetInt64(-1)
		}
		return nil
	}
	for _, alloc := range []map[common.Hash]*accountPatch{nil, {crypto.Keccak256Hash(ordinary[:]): {Address: ordinary, Balance: new(uint256.Int), Storage: map[common.Hash]common.Hash{sortedHashes(f.storage)[0]: {}}}}} {
		want, err := s.walk(t.Context(), 2, l, x, patches, alloc, consume)
		must(t, err)
		errs := make(chan error, 4)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				got, err := s.walk(t.Context(), 2, l, x, patches, alloc, consume)
				if err == nil && (got.sourceRoot != want.sourceRoot || got.convertedRoot != want.convertedRoot || got.finalRoot != want.finalRoot || got.counts != want.counts || got.convertedCounts != want.convertedCounts || !maps.Equal(codeIndexEntries(&got.codes), codeIndexEntries(&want.codes))) {
					err = fmt.Errorf("parallel walk changed roots or inventories")
				}
				// Other walks must not share this returned set, even in no-alloc mode.
				if err == nil {
					got.codes.Add(common.HexToHash("0x1234"), finalCode)
				}
				errs <- err
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			must(t, err)
		}
		if len(l) != 0 {
			t.Fatal("parallel walks leaked permits")
		}
	}
}
