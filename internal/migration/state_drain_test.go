package migration

import (
	"context"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/metis-devops/metis-l2state/statefile"
)

func treeLeaves[V any](node *stateNode[V]) []*stateNode[V] {
	if node == nil {
		return nil
	}
	if len(node.children) == 0 {
		return []*stateNode[V]{node}
	}
	var result []*stateNode[V]
	for _, child := range node.children {
		result = append(result, treeLeaves(child)...)
	}
	return result
}

func TestOutputReleasesStorageAfterWriterAcknowledgement(t *testing.T) {
	for _, mode := range []string{"complete", "write-error", "cancel", "overlay"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, 513, 0)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			in, err := loadInputs(t.Context(), f.options("unused"))
			must(t, err)
			l := make(limiter, 2)
			x, err := s.prepareSnapshot(t.Context(), 2, l, in.events)
			must(t, err)
			_, err = s.convert(t.Context(), x, in, 2, l)
			must(t, err)
			h := crypto.Keccak256Hash(ordinary[:])
			account := s.memory.accounts.Value(h)
			leaves := treeLeaves(account.storage.root)
			sizes := make([]int, len(leaves))
			for i, n := range leaves {
				sizes[i] = len(n.items)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sentinel := errors.New("write failed after a released leaf")
			target := false
			slots, leaf, offset := 0, 0, 0
			alloc := map[common.Hash]*accountPatch(nil)
			if mode == "overlay" { // Deleting every slot still needs producer acknowledgements.
				patch := make(map[common.Hash]common.Hash)
				must(t, account.storage.Visit(func(k, _ common.Hash) error { patch[k] = common.Hash{}; return nil }))
				alloc = map[common.Hash]*accountPatch{h: {Address: ordinary, Storage: patch}}
			}
			_, err = s.writeMigration(ctx, 2, l, x, alloc, func(r statefile.Record) error {
				if r.Kind == statefile.AccountStart {
					target = r.Account.Address == ordinary
				}
				if target && r.Kind == statefile.StorageEntry {
					if len(leaves[leaf].items) == 0 {
						t.Error("current leaf released before Writer acknowledgement")
					}
					if leaf > 0 && leaves[leaf-1].items != nil {
						t.Error("previous leaf retained after acknowledgement")
					}
					slots++
					offset++
					if slots == stateLeafSize+2 {
						if mode == "write-error" {
							return sentinel
						}
						if mode == "cancel" {
							cancel()
							return ctx.Err()
						}
					}
					if offset == sizes[leaf] {
						leaf++
						offset = 0
					}
				}
				if target && r.Kind == statefile.AccountEnd {
					for _, n := range leaves {
						if n.items != nil {
							t.Error("storage leaf retained at account end")
						}
					}
				}
				return nil
			})
			switch mode {
			case "write-error":
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			default:
				must(t, err)
			}
			if s.memory.accounts.root != nil || account.storage.root != nil || len(l) != 0 {
				t.Fatal("output retained state or permits")
			}
			if _, err = s.writeMigration(t.Context(), 2, l, x, nil, func(statefile.Record) error { t.Fatal("replayed consumed output"); return nil }); err == nil {
				t.Fatal("output retried consumed state")
			}
		})
	}
}

func TestAccountBatchesWaitForCompletedOutput(t *testing.T) {
	// Construct a validated in-memory fixture with more accounts than a leaf or
	// admission window. All accounts are empty, so roots/metadata need no storage.
	m := newStateSnapshot()
	x := new(indexes)
	x.addresses = make(map[common.Hash]common.Address)
	addresses := make(map[common.Hash]common.Address)
	for n := range stateLeafSize*2 + 1 {
		a := common.BytesToAddress(treeKey(n + 1).Bytes())
		addresses[crypto.Keccak256Hash(a[:])] = a
	}
	for _, h := range sortedHashes(addresses) {
		must(t, m.accounts.Append(h, &memoryAccount{}))
		x.addresses[h] = addresses[h]
	}
	s := &source{memory: m}
	m.phase = memoryFrozen
	x.validation = &sourceValidation{source: s, head: s.head, policy: ovmSource}
	leaves := treeLeaves(m.accounts.root)
	count := 0
	_, err := s.writeMigration(t.Context(), 2, make(limiter, 2), x, nil, func(r statefile.Record) error {
		if r.Kind == statefile.AccountStart {
			index := count / stateLeafSize
			if len(leaves[index].items) == 0 {
				t.Error("account batch released before output")
			}
			if index > 0 && leaves[index-1].items != nil {
				t.Error("completed account batch retained")
			}
			count++
		}
		return nil
	})
	must(t, err)
	if count != len(addresses) {
		t.Fatal("accounts lost across batches")
	}
	for _, leaf := range leaves {
		if leaf.items != nil {
			t.Fatal("account batch retained")
		}
	}
}
