package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
)

const partitions = 16
const storageThreshold = 1024
const streamAhead = 64

type limiter chan struct{}

func (l limiter) run(ctx context.Context, f func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case l <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-l }()
	return f()
}

type leaf struct {
	key   common.Hash
	value []byte
}

// Partition streams carry only fixed-size storage words or bounded account
// RLP. The limiter is NEVER held while sending to a queue or joining producers.
func (s *source) partitionStream(ctx context.Context, id *trie.ID, l limiter, visit func(leaf) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	failure := &runFailure{cancel: cancel}
	channels := make([]chan leaf, partitions)
	results := make([]chan error, partitions)
	for p := range partitions {
		channels[p] = make(chan leaf, streamAhead)
		results[p] = make(chan error, 1)
		wg.Go(func() {
			defer close(channels[p])
			err := s.producePartition(ctx, id, byte(p), l, channels[p])
			failure.record(err)
			results[p] <- err
		})
	}
	var ret error
	for p := range partitions {
		for v := range channels[p] {
			if ret == nil {
				ret = visit(v)
				failure.record(ret)
			}
		}
		if err := <-results[p]; ret == nil && err != nil {
			ret = err
		}
	}
	wg.Wait()
	if failure.err != nil {
		return failure.err
	}
	return ctx.Err()
}
func (s *source) producePartition(ctx context.Context, id *trie.ID, p byte, l limiter, out chan<- leaf) error {
	var it *trie.Iterator
	err := l.run(ctx, func() error {
		t, err := trie.New(id, s.nodes)
		if err != nil {
			return err
		}
		var start, end []byte
		if p > 0 {
			start = make([]byte, 32)
			start[0] = p << 4
		}
		if p < 15 {
			end = make([]byte, 32)
			end[0] = (p + 1) << 4
		}
		nodes, err := t.NodeIteratorWithRange(start, end)
		if err != nil {
			return err
		}
		it = trie.NewIterator(nodes)
		return nil
	})
	if err != nil {
		return err
	}
	for {
		var v leaf
		var next bool
		err := l.run(ctx, func() error {
			next = it.Next()
			if !next {
				return it.Err
			}
			if len(it.Key) != 32 || len(it.Value) > 110 {
				return errors.New("invalid trie leaf framing")
			}
			v = leaf{common.Hash(it.Key), bytes.Clone(it.Value)}
			return nil
		})
		if err != nil {
			return err
		}
		if !next {
			return nil
		}
		select {
		case out <- v:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (s *source) storage(ctx context.Context, owner, root common.Hash, l limiter, visit func(common.Hash, common.Hash) error) error {
	if root == types.EmptyRootHash {
		return nil
	}
	if s.memory != nil {
		return s.memory.storage(ctx, owner, root, visit)
	}
	id := trie.StorageTrieID(s.head.StateRoot, owner, root)
	var small []struct{ key, value common.Hash }
	large := false
	err := l.run(ctx, func() error {
		it, err := s.iterator(id, nil)
		if err != nil {
			return err
		}
		for len(small) <= storageThreshold {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !it.Next() {
				return it.Err
			}
			if len(it.Key) != 32 {
				return errors.New("invalid storage key")
			}
			v, err := decodeSlot(it.Value)
			if err != nil {
				return err
			}
			small = append(small, struct{ key, value common.Hash }{common.Hash(it.Key), v})
		}
		large = true
		return nil
	})
	if err != nil {
		return err
	}
	if !large {
		for _, v := range small {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(v.key, v.value); err != nil {
				return err
			}
		}
		return nil
	}
	small = nil
	return s.partitionStream(ctx, id, l, func(v leaf) error {
		value, err := decodeSlot(v.value)
		if err != nil {
			return err
		}
		return visit(v.key, value)
	})
}

func sortedHashes[V any](m map[common.Hash]V) []common.Hash {
	keys := make([]common.Hash, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b common.Hash) int { return bytes.Compare(a[:], b[:]) })
	return keys
}

// mergeSlots overlays sorted patches without retaining the source storage.
type slotMerger struct {
	keys   []common.Hash
	patch  map[common.Hash]common.Hash
	offset int
	emit   func(common.Hash, common.Hash) error
}

func newSlotMerger(p map[common.Hash]common.Hash, emit func(common.Hash, common.Hash) error) *slotMerger {
	return &slotMerger{keys: sortedHashes(p), patch: p, emit: emit}
}
func (m *slotMerger) put(k, v common.Hash) error {
	if v == (common.Hash{}) {
		return nil
	}
	return m.emit(k, v)
}
func (m *slotMerger) accept(k, v common.Hash) error {
	for m.offset < len(m.keys) && bytes.Compare(m.keys[m.offset][:], k[:]) < 0 {
		key := m.keys[m.offset]
		m.offset++
		if err := m.put(key, m.patch[key]); err != nil {
			return err
		}
	}
	if m.offset < len(m.keys) && m.keys[m.offset] == k {
		v = m.patch[k]
		m.offset++
	}
	return m.put(k, v)
}
func (m *slotMerger) finish() error {
	for m.offset < len(m.keys) {
		k := m.keys[m.offset]
		m.offset++
		if err := m.put(k, m.patch[k]); err != nil {
			return err
		}
	}
	return nil
}
func updateAccount(t *trie.StackTrie, hash common.Hash, a *types.StateAccount) error {
	b, err := rlp.EncodeToBytes(a)
	if err != nil {
		return err
	}
	if err := t.Update(hash[:], b); err != nil {
		return fmt.Errorf("rebuild account: %w", err)
	}
	return nil
}
