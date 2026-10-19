package migration

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/internal/storageword"
	"github.com/metis-devops/metis-l2state/statefile"
)

// Blocks never grow or move. Only their small directory can be reallocated.
// Pointer-free code records avoid GC scanning of their payload.
const snapshotBlockBytes = 1 << 20

type snapshotBlocks[T any] struct {
	blocks   [][]T
	length   uint64
	perBlock uint64
}

func (b *snapshotBlocks[T]) append(v T) {
	if b.length%b.perBlock == 0 {
		b.blocks = append(b.blocks, make([]T, b.perBlock))
	}
	b.blocks[b.length/b.perBlock][b.length%b.perBlock] = v
	b.length++
}
func (b *snapshotBlocks[T]) ptr(i uint64) *T { return &b.blocks[i/b.perBlock][i%b.perBlock] }
func (b *snapshotBlocks[T]) at(i uint64) T   { return *b.ptr(i) }

// Account values change only during conversion; output destroys owned trees. Zero balances,
// empty code and empty storage roots need no separately allocated values.
type memoryAccount struct {
	nonce   uint64
	balance *uint256.Int
	code    *common.Hash
	storage stateTree[common.Hash]
	root    *common.Hash
}

func (a *memoryAccount) storageRoot() common.Hash {
	if a.root == nil {
		return types.EmptyRootHash
	}
	return *a.root
}
func (a *memoryAccount) setBalance(value *uint256.Int) {
	a.balance = nil
	if !value.IsZero() {
		a.balance = new(uint256.Int).Set(value)
	}
}
func (a *memoryAccount) setStorage(key, value common.Hash) {
	if value == (common.Hash{}) {
		a.storage.Delete(key)
		return
	}
	a.storage.Set(key, value)
}

type snapshotCode struct {
	hash          common.Hash
	start, length uint64
}

type memoryPhase uint8

const (
	memoryReady memoryPhase = iota
	memoryConverting
	memoryFrozen
	memoryDraining
	memoryConsumed
	memoryFailed
)

// The ordered consumer owns capture. Conversion owns mutation after all source
// validation jobs join. Output consumes exclusively owned trees in bounded, acknowledged batches. Bytecode stays shared
// and immutable; code IDs retain source discovery order independently of tree layout.
type stateSnapshot struct {
	*codeStore
	accounts stateTree[*memoryAccount]
	pending  stateTree[common.Hash]
	phase    memoryPhase
	outputMu sync.Mutex
}

func newStateSnapshot() *stateSnapshot {
	return &stateSnapshot{codeStore: newCodeStore(true)}
}
func (m *stateSnapshot) capture(e accountEvent) error {
	if e.record != nil {
		if e.record.Kind != statefile.StorageEntry {
			return errors.New("invalid snapshot record")
		}
		return m.pending.Append(common.Hash(e.record.Storage.Key), e.record.Storage.Value)
	}
	original := e.done.job.original
	a := &memoryAccount{nonce: original.Nonce, storage: m.pending}
	if original.Root != types.EmptyRootHash {
		root := original.Root
		a.root = &root
	}
	m.codeMu.Lock()
	id, ok := m.codeIDs[common.BytesToHash(original.CodeHash)]
	if ok {
		c := m.codes.ptr(id - 1)
		ok = c.length != 0
		a.code = &c.hash
	}
	m.codeMu.Unlock()
	if !ok && !bytes.Equal(original.CodeHash, types.EmptyCodeHash[:]) {
		return errors.New("snapshot missing validated code")
	}
	m.pending = stateTree[common.Hash]{}
	return m.accounts.Append(e.done.job.hash, a)
}
func (m *stateSnapshot) find(h common.Hash) (*memoryAccount, bool) {
	return m.accounts.Get(h)
}
func (m *stateSnapshot) account(a *memoryAccount) *types.StateAccount {
	result := emptyAccount()
	result.Nonce, result.Root = a.nonce, a.storageRoot()
	if a.balance != nil {
		result.Balance.Set(a.balance)
	}
	if a.code != nil {
		result.CodeHash = a.code.Bytes()
	}
	return result
}
func (m *stateSnapshot) get(h common.Hash) *types.StateAccount {
	a, ok := m.find(h)
	if !ok {
		return emptyAccount()
	}
	return m.account(a)
}
func (m *stateSnapshot) storage(ctx context.Context, owner, root common.Hash, visit func(common.Hash, common.Hash) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a, ok := m.find(owner)
	if !ok || a.storageRoot() != root {
		return errors.New("snapshot storage identity mismatch")
	}
	return a.storage.Visit(func(key, value common.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(key, value)
	})
}
func (m *stateSnapshot) storageValue(owner, root, key common.Hash) (common.Hash, error) {
	a, ok := m.find(owner)
	if !ok || a.storageRoot() != root {
		return common.Hash{}, errors.New("snapshot storage identity mismatch")
	}
	return a.storage.Value(key), nil
}

// Only OVM_ETH storage changes during conversion. Recompute its root before
// freezing, leaving validated roots on untouched accounts available for reuse.
func (m *stateSnapshot) finishConversion(ctx context.Context) error {
	a := m.accounts.Value(ovmHash)
	stack := trie.NewStackTrie(nil)
	if err := a.storage.Visit(func(key, value common.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return storageword.Update(stack, key, value)
	}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.root = nil
	if a.storage.Len() != 0 {
		root := stack.Hash()
		a.root = &root
	} else {
		a.storage = stateTree[common.Hash]{}
	}
	m.phase = memoryFrozen
	return nil
}

func (s *source) prepareSnapshot(ctx context.Context, workers int, l limiter, events *ovmevents.Validated) (*indexes, error) {
	if s.memory != nil {
		return nil, errors.New("source already has a snapshot")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var x *indexes
	var indexErr, stateErr error
	m := newStateSnapshot()
	m.cache = s.codeCache != CodeCacheNone
	wg.Go(func() {
		x, indexErr = s.loadIndexes(ctx, l, events)
		if indexErr != nil {
			cancel()
		}
	})
	wg.Go(func() {
		_, stateErr = s.captureSource(ctx, workers, l, m)
		if stateErr != nil {
			cancel()
		}
	})
	wg.Wait()
	if err := errors.Join(indexErr, stateErr, ctx.Err()); err != nil {
		return nil, err
	}
	s.memory = m
	x.validation = &sourceValidation{source: s, head: s.head}
	return x, nil
}
