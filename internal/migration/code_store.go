package migration

import (
	"context"
	"errors"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

// Code metadata is always retained. Optional bytecode occupies fixed 1 MiB
// blocks; completion order affects internal offsets only, never IDs or output.
type codeStore struct {
	codes      snapshotBlocks[snapshotCode]
	codeIDs    map[common.Hash]uint64
	blobs      [][]byte
	blobLength uint64
	cache      bool
	codeMu     sync.Mutex
}

func newCodeStore(cache bool) *codeStore {
	return &codeStore{codes: snapshotBlocks[snapshotCode]{perBlock: 1024}, codeIDs: make(map[common.Hash]uint64), cache: cache}
}

// reserve and complete require codeMu. IDs are one-based; zero is empty code.
func (m *codeStore) reserve(h common.Hash) uint64 {
	m.codes.append(snapshotCode{hash: h})
	id := m.codes.length
	m.codeIDs[h] = id
	return id
}

func (m *codeStore) complete(id uint64, code []byte) {
	c := m.codes.ptr(id - 1)
	c.start, c.length = m.blobLength, uint64(len(code))
	if !m.cache {
		return
	}
	for len(code) > 0 {
		offset := m.blobLength % snapshotBlockBytes
		if offset == 0 {
			m.blobs = append(m.blobs, make([]byte, snapshotBlockBytes))
		}
		n := copy(m.blobs[len(m.blobs)-1][offset:], code)
		m.blobLength += uint64(n)
		code = code[n:]
	}
}

func (m *codeStore) code(h common.Hash) ([]byte, error) {
	id, ok := m.codeIDs[h]
	if !ok || !m.cache {
		return nil, errors.New("validated code not cached")
	}
	c := m.codes.at(id - 1)
	if c.length == 0 {
		return nil, errors.New("code validation incomplete")
	}
	out := make([]byte, c.length)
	for offset := uint64(0); offset < c.length; {
		pos := c.start + offset
		n := copy(out[offset:], m.blobs[pos/snapshotBlockBytes][pos%snapshotBlockBytes:])
		offset += uint64(n)
	}
	return out, nil
}

// Only active loads retain a channel. Their count is bounded by account
// admission, including when many accounts share one in-flight code hash.
type codeLoad struct {
	done chan struct{}
	err  error
}

func (c *codeLoad) wait(ctx context.Context) error {
	if c == nil {
		return ctx.Err()
	}
	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type codeTask struct {
	hash common.Hash
	id   uint64
	load *codeLoad
}

type codeLoader struct {
	store   *codeStore
	pending map[common.Hash]*codeLoad // protected by store.codeMu
	queue   chan codeTask
}

func newCodeLoader(ctx context.Context, s *source, store *codeStore, l limiter, failure *runFailure, jobs *sync.WaitGroup) *codeLoader {
	c := &codeLoader{store: store, pending: make(map[common.Hash]*codeLoad), queue: make(chan codeTask, 2*cap(l))}
	// Persistent workers amortize scheduling for short bytecodes. They share the
	// same global permits as trie/storage work, never holding one on queue waits.
	// At most half are code readers, leaving capacity for storage even when
	// every code read is stalled.
	for range max(1, cap(l)/2) {
		jobs.Go(func() {
			for task := range c.queue {
				c.read(ctx, s, task, l, failure)
			}
		})
	}
	return c
}

func (c *codeLoader) submit(ctx context.Context, h common.Hash) (*codeLoad, error) {
	if h == types.EmptyCodeHash {
		return nil, nil
	}
	m := c.store
	m.codeMu.Lock()
	if _, ok := m.codeIDs[h]; ok {
		load := c.pending[h]
		m.codeMu.Unlock()
		return load, nil
	}
	id := m.reserve(h)
	load := &codeLoad{done: make(chan struct{})}
	c.pending[h] = load
	m.codeMu.Unlock()
	task := codeTask{h, id, load}
	select {
	case c.queue <- task:
		return load, nil
	case <-ctx.Done():
		c.complete(task, nil, ctx.Err())
		return nil, ctx.Err()
	}
}

func (c *codeLoader) read(ctx context.Context, s *source, task codeTask, l limiter, failure *runFailure) {
	var code []byte
	err := l.run(ctx, func() (err error) { code, err = s.readCode(task.hash); return err })
	failure.record(err)
	c.complete(task, code, err)
}

func (c *codeLoader) complete(task codeTask, code []byte, err error) {
	// Release the permit before capture, completion notification or another job.
	m := c.store
	m.codeMu.Lock()
	if err == nil {
		m.complete(task.id, code)
	}
	task.load.err = err
	delete(c.pending, task.hash)
	close(task.load.done)
	m.codeMu.Unlock()
}
