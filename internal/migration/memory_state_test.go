package migration

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
)

func TestMemoryAccountDefaultsAndStorageWithoutCode(t *testing.T) {
	m := newStateSnapshot()
	emptyKey, storageKey := common.HexToHash("0x01"), common.HexToHash("0x02")
	must(t, m.capture(accountEvent{done: &accountDone{job: accountJob{hash: emptyKey, original: emptyAccount()}}}))
	a := m.accounts.Value(emptyKey)
	if a.balance != nil || a.code != nil || a.root != nil || a.storage.Len() != 0 {
		t.Fatal("empty account allocated optional fields")
	}
	slot, value := common.HexToHash("0x03"), common.HexToHash("0x04")
	root := expectedStorageRoot(t, map[common.Hash]common.Hash{slot: value})
	original := emptyAccount()
	original.Root, original.Nonce = root, 7
	must(t, m.capture(accountEvent{record: &accountRecord{Kind: statefile.StorageEntry, Storage: statefile.Storage{Key: statefile.StorageKeyHash(slot), Value: value}}}))
	must(t, m.capture(accountEvent{done: &accountDone{job: accountJob{hash: storageKey, original: original}}}))
	a = m.accounts.Value(storageKey)
	if a.code != nil || a.storage.Value(slot) != value || a.storageRoot() != root {
		t.Fatal("no-code account lost nonempty storage")
	}
	a.setBalance(uint256.NewInt(9))
	read := m.get(storageKey)
	read.Balance.SetUint64(100)
	read.Nonce = 99
	if m.get(storageKey).Balance.Uint64() != 9 || a.nonce != 7 {
		t.Fatal("account reader aliases memory")
	}
	a.setBalance(new(uint256.Int))
	if a.balance != nil {
		t.Fatal("zero balance retained an allocation")
	}
	if m.get(emptyKey).Root != types.EmptyRootHash || !bytes.Equal(m.get(emptyKey).CodeHash, types.EmptyCodeHash[:]) {
		t.Fatal("implicit defaults not restored")
	}
}

func TestConversionMutatesTreesAfterAccounting(t *testing.T) {
	f := newFixture(t, 3, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	in, err := loadInputs(t.Context(), f.options("unused"))
	must(t, err)
	in.retain[manual] = struct{}{}
	l := make(limiter, 2)
	x, err := s.prepareSnapshot(t.Context(), 2, l, in.events)
	must(t, err)
	ovm := s.memory.accounts.Value(ovmHash)
	beforeStorage := treeMap(&ovm.storage)
	beforeCode := *ovm.code
	sourceRoot := s.head.StateRoot
	eoaHash := crypto.Keccak256Hash(eoa[:])
	if s.memory.accounts.Value(eoaHash) != nil {
		t.Fatal("fixture must insert the EOA during conversion")
	}
	if _, err := s.writeMigration(t.Context(), 2, l, x, nil, func(statefile.Record) error { t.Fatal("unconverted state emitted"); return nil }); err == nil {
		t.Fatal("unconverted memory accepted")
	}
	evidence, err := s.convert(t.Context(), x, in, 2, l)
	must(t, err)
	if s.memory.phase != memoryFrozen || s.memory.accounts.Value(ovmHash) != ovm {
		t.Fatal("conversion did not mutate and freeze the existing tree")
	}
	if evidence.MigratedNative != "44" || evidence.RemainingSupply != "121" {
		t.Fatal(evidence)
	}
	for address, want := range map[common.Address]uint64{eoa: 11, ordinary: 33} {
		a := s.memory.accounts.Value(crypto.Keccak256Hash(address[:]))
		if a.balance == nil || a.balance.Uint64() != want {
			t.Fatal("missing converted native balance")
		}
		if _, ok := ovm.storage.Get(balanceKey(address)); ok {
			t.Fatal("converted token slot not deleted")
		}
	}
	for _, address := range []common.Address{automatic, manual, ovmAddress} {
		if ovm.storage.Value(balanceKey(address)) != beforeStorage[balanceKey(address)] {
			t.Fatal("retained token slot changed")
		}
	}
	if ovm.storage.Value(allowanceKey(eoa, automatic)) != beforeStorage[allowanceKey(eoa, automatic)] {
		t.Fatal("allowance changed")
	}
	if ovm.balance.Uint64() != 121 || *ovm.code != crypto.Keccak256Hash(in.runtime) || ovm.storageRoot() != expectedStorageRoot(t, treeMap(&ovm.storage)) {
		t.Fatal("invalid wrapped account")
	}
	if s.head.StateRoot != sourceRoot || s.codes.codes.at(s.codes.codeIDs[beforeCode]-1).hash != beforeCode {
		t.Fatal("source identity or shared code changed")
	}
	if _, err := s.convert(t.Context(), x, in, 2, l); err == nil {
		t.Fatal("converted state converted twice")
	}
	x.metadata.ToolVersion = "test"
	encodeWalk(t, s, x, in, nil, evidence)
}

func TestConversionFailureDoesNotMutateOrPublishMemory(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "accounting", true: "canceled"}[cancel], func(t *testing.T) {
			f := newFixture(t, 3, 1)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			in, err := loadInputs(t.Context(), f.options("unused"))
			must(t, err)
			l := make(limiter, 2)
			x, err := s.prepareSnapshot(t.Context(), 2, l, in.events)
			must(t, err)
			ovm := s.memory.accounts.Value(ovmHash)
			storage, code, root, count := treeMap(&ovm.storage), *ovm.code, ovm.storageRoot(), s.memory.accounts.Len()
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			if cancel {
				stop()
			}
			_, err = s.convert(ctx, x, in, 2, l)
			if err == nil || (cancel && !errors.Is(err, context.Canceled)) {
				t.Fatal(err)
			}
			if s.memory.phase != memoryFailed || !maps.Equal(storage, treeMap(&ovm.storage)) || *ovm.code != code || ovm.storageRoot() != root || s.memory.accounts.Len() != count {
				t.Fatal("failed analysis changed state")
			}
			must(t, s.memory.accounts.Visit(func(_ common.Hash, a *memoryAccount) error {
				if a.balance != nil {
					t.Fatal("failed analysis set native balance")
				}
				return nil
			}))
			if _, err := s.writeMigration(t.Context(), 2, l, x, nil, func(statefile.Record) error { t.Fatal("failed conversion emitted"); return nil }); err == nil {
				t.Fatal("failed state accepted")
			}
		})
	}
}

func TestConsumedTreeRejectsRepeatedAndConcurrentOutput(t *testing.T) {
	f := newFixture(t, 1025, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	in, err := loadInputs(t.Context(), f.options("unused"))
	must(t, err)
	x, err := s.prepareSnapshot(t.Context(), 2, make(limiter, 2), in.events)
	must(t, err)
	_, err = s.convert(t.Context(), x, in, 2, make(limiter, 2))
	must(t, err)
	var wg sync.WaitGroup
	results := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			_, err := s.writeMigration(t.Context(), 2, make(limiter, 2), x, nil, func(r statefile.Record) error {
				if r.Kind == statefile.AccountStart {
					r.Account.Balance.SetInt64(-1)
				}
				return nil
			})
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 || s.memory.phase != memoryConsumed || s.memory.accounts.Len() != 0 || s.memory.accounts.root != nil {
		t.Fatal("output was not consumed exactly once")
	}
}

func TestConversionTotalSupplyInsertionAndDeletion(t *testing.T) {
	for _, insert := range []bool{false, true} {
		t.Run(map[bool]string{false: "delete", true: "insert"}[insert], func(t *testing.T) {
			f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
				if insert {
					delete(storage, crypto.Keccak256Hash(common.HexToHash("0x02").Bytes()))
					return
				}
				delete(storage, balanceKey(ovmAddress))
				storage[crypto.Keccak256Hash(common.HexToHash("0x02").Bytes())] = common.HexToHash("0x6e")
			})
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			in, err := loadInputs(t.Context(), f.options("unused"))
			must(t, err)
			if insert {
				path := writeAudit(t, f, 165)
				in.audit, _, err = readOVMAuditSummary(t.Context(), path)
				must(t, err)
			} else {
				// No automatic retaining contracts: all remaining balances become native.
				d := f.events(t)
				d.Members = nil
				must(t, ovmevents.Save(f.eventPath(), d))
				in.events, _, err = ovmevents.ReadValidatedFile(t.Context(), f.eventPath())
				must(t, err)
			}
			x, err := s.prepareSnapshot(t.Context(), 2, make(limiter, 2), in.events)
			must(t, err)
			evidence, err := s.convert(t.Context(), x, in, 2, make(limiter, 2))
			must(t, err)
			ovm := s.memory.accounts.Value(ovmHash)
			value, exists := ovm.storage.Get(crypto.Keccak256Hash(common.HexToHash("0x02").Bytes()))
			if insert {
				if !exists || value != common.HexToHash("0x4d") {
					t.Fatal("missing retained supply insertion")
				}
			} else if exists || ovm.balance != nil {
				t.Fatal("zero supply/backing not omitted")
			}
			x.metadata.ToolVersion = "test"
			encodeWalk(t, s, x, in, nil, evidence)
		})
	}
}

// The inserted EOA makes cancellation deterministic after the first mutation,
// without timing races or a production-only failure hook.
type cancelAfterInsertion struct {
	context.Context
	memory *stateSnapshot
	key    common.Hash
}

func (c cancelAfterInsertion) Err() error {
	if c.memory.accounts.Value(c.key) != nil {
		return context.Canceled
	}
	return c.Context.Err()
}
func TestConversionCancellationAfterMutationCannotBeExported(t *testing.T) {
	f := newFixture(t, 3, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	in, err := loadInputs(t.Context(), f.options("unused"))
	must(t, err)
	l := make(limiter, 2)
	x, err := s.prepareSnapshot(t.Context(), 2, l, in.events)
	must(t, err)
	ctx := cancelAfterInsertion{t.Context(), s.memory, crypto.Keccak256Hash(eoa[:])}
	_, err = s.convert(ctx, x, in, 2, l)
	if !errors.Is(err, context.Canceled) || s.memory.accounts.Value(ctx.key) == nil || s.memory.phase != memoryFailed {
		t.Fatalf("expected partial mutation and failed state: %v", err)
	}
	if _, err = s.writeMigration(t.Context(), 2, l, x, nil, func(statefile.Record) error { t.Fatal("partial conversion emitted"); return nil }); err == nil {
		t.Fatal("partial conversion accepted")
	}
	if _, err = s.convert(t.Context(), x, in, 2, l); err == nil {
		t.Fatal("partial conversion retried")
	}
}

func treeMap[V any](tree *stateTree[V]) map[common.Hash]V {
	result := make(map[common.Hash]V)
	_ = tree.Visit(func(k common.Hash, v V) error { result[k] = v; return nil })
	return result
}
