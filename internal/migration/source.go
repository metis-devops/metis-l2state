package migration

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/readonlydb"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

type head struct {
	BlockNumber uint64
	BlockHash   common.Hash
	StateRoot   common.Hash
}
type source struct {
	memory    *stateSnapshot
	codes     *codeStore
	codeCache string
	kv        *readonlydb.Database
	preimages *readonlydb.Snapshot
	db        ethdb.Database
	nodes     *triedb.Database
	head      head
	header    []byte
}

func openSource(path string, cache, handles int) (*source, error) {
	kv, err := readonlydb.Open(path, cache, handles)
	if err != nil {
		return nil, err
	}
	db := rawdb.NewDatabase(kv)
	h, raw, err := readHead(db)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &source{kv: kv, db: db, nodes: triedb.NewDatabase(db, triedb.HashDefaults), head: h, header: raw}, nil
}
func (s *source) close() error {
	// Dump owns only a LevelDB sequence handle here. All walk workers have
	// joined before close; migration never creates this handle.
	if s.preimages != nil {
		s.preimages.Release()
		s.preimages = nil
	}
	return errors.Join(s.nodes.Close(), s.db.Close())
}
func optional(db ethdb.KeyValueReader, key []byte) ([]byte, error) {
	v, err := db.Get(key)
	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(v) == 0 {
		return nil, fmt.Errorf("empty source record %x", key)
	}
	return v, nil
}
func required(db ethdb.KeyValueReader, key []byte) ([]byte, error) {
	v, err := optional(db, key)
	if err == nil && v == nil {
		err = fmt.Errorf("missing hot source record %x", key)
	}
	return v, err
}
func numberKey(n uint64, suffix []byte) []byte {
	k := make([]byte, 9, len(suffix)+9)
	k[0] = 'h'
	binary.BigEndian.PutUint64(k[1:], n)
	return append(k, suffix...)
}
func readHeader(db ethdb.KeyValueReader, n uint64) (*types.Header, []byte, error) {
	h, err := required(db, numberKey(n, []byte{'n'}))
	if err != nil {
		return nil, nil, err
	}
	if len(h) != 32 {
		return nil, nil, errors.New("invalid canonical hash")
	}
	raw, err := required(db, numberKey(n, h))
	if err != nil {
		return nil, nil, err
	}
	var header types.Header
	if err := rlp.DecodeBytes(raw, &header); err != nil {
		return nil, nil, err
	}
	if header.Number == nil || !header.Number.IsUint64() || header.Number.Uint64() != n || crypto.Keccak256Hash(raw) != common.Hash(h) || header.Root == (common.Hash{}) {
		return nil, nil, errors.New("invalid canonical header")
	}
	return &header, raw, nil
}
func readHead(db ethdb.KeyValueReader) (head, []byte, error) {
	hash, err := required(db, []byte("LastBlock"))
	if err != nil {
		return head{}, nil, err
	}
	if len(hash) != 32 {
		return head{}, nil, errors.New("invalid LastBlock")
	}
	n, err := required(db, append([]byte{'H'}, hash...))
	if err != nil {
		return head{}, nil, err
	}
	if len(n) != 8 {
		return head{}, nil, errors.New("invalid header number mapping")
	}
	header, raw, err := readHeader(db, binary.BigEndian.Uint64(n))
	if err != nil {
		return head{}, nil, err
	}
	if crypto.Keccak256Hash(raw) != common.Hash(hash) {
		return head{}, nil, errors.New("LastBlock is not canonical")
	}
	return head{header.Number.Uint64(), common.Hash(hash), header.Root}, raw, nil
}
func (s *source) code(hash common.Hash) ([]byte, error) {
	if hash == types.EmptyCodeHash {
		return nil, nil
	}
	if s.codes != nil && s.codes.cache {
		return s.codes.code(hash)
	}
	return s.readCode(hash)
}

func (s *source) readCode(hash common.Hash) (code []byte, retErr error) {
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("source code %s: %w", hash, retErr)
		}
	}()
	var err error
	code, err = required(s.db, hash[:])
	if err != nil {
		return nil, err
	}
	if len(code) > statefile.MaxCodeSize {
		return nil, errors.New("source code exceeds maximum size")
	}
	if crypto.Keccak256Hash(code) != hash {
		return nil, errors.New("source code hash mismatch")
	}
	return code, nil
}
func decodeAccount(b []byte) (*types.StateAccount, error) {
	var a types.StateAccount
	if err := rlp.DecodeBytes(b, &a); err != nil {
		return nil, err
	}
	var zeroHash common.Hash
	if a.Balance == nil || len(a.CodeHash) != 32 || a.Root == (common.Hash{}) || bytes.Equal(a.CodeHash, zeroHash[:]) {
		return nil, errors.New("invalid source account")
	}
	return &a, nil
}
func emptyAccount() *types.StateAccount {
	return &types.StateAccount{Balance: new(uint256.Int), Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()}
}
func copyAccount(a *types.StateAccount) *types.StateAccount {
	return &types.StateAccount{Nonce: a.Nonce, Balance: new(uint256.Int).Set(a.Balance), Root: a.Root, CodeHash: bytes.Clone(a.CodeHash)}
}
func decodeSlot(b []byte) (common.Hash, error) {
	kind, p, rest, err := rlp.Split(b)
	if err != nil {
		return common.Hash{}, err
	}
	if (kind != rlp.Byte && kind != rlp.String) || len(rest) != 0 || len(p) == 0 || len(p) > 32 || p[0] == 0 {
		return common.Hash{}, errors.New("noncanonical source storage value")
	}
	return common.BytesToHash(p), nil
}

// accountReader is exclusively owned by a job/worker. Discard decoded trie
// paths every 32 reads to retain locality without an operation-wide trie cache.
type accountReader struct {
	s     *source
	t     *trie.Trie
	reads int
}

func (r *accountReader) get(hash common.Hash) (*types.StateAccount, error) {
	if r.s.memory != nil {
		return r.s.memory.get(hash), nil
	}
	if r.t == nil {
		var err error
		r.t, err = trie.New(trie.StateTrieID(r.s.head.StateRoot), r.s.nodes)
		if err != nil {
			return nil, err
		}
	}
	b, err := r.t.Get(hash[:])
	r.reads++
	if r.reads == 32 || err != nil {
		r.t = nil
		r.reads = 0
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return emptyAccount(), nil
	}
	return decodeAccount(b)
}
func (s *source) iterator(id *trie.ID, start []byte) (*trie.Iterator, error) {
	t, err := trie.New(id, s.nodes)
	if err != nil {
		return nil, err
	}
	it, err := t.NodeIterator(start)
	if err != nil {
		return nil, err
	}
	return trie.NewIterator(it), nil
}
func (s *source) storageValue(owner, root, slot common.Hash) (common.Hash, error) {
	if s.memory != nil {
		return s.memory.storageValue(owner, root, crypto.Keccak256Hash(slot[:]))
	}
	t, err := trie.New(trie.StorageTrieID(s.head.StateRoot, owner, root), s.nodes)
	if err != nil {
		return common.Hash{}, err
	}
	key := crypto.Keccak256Hash(slot[:])
	b, err := t.Get(key[:])
	if err != nil {
		return common.Hash{}, err
	}
	if len(b) == 0 {
		return common.Hash{}, nil
	}
	return decodeSlot(b)
}

func (s *source) genesisIdentity() (common.Hash, error) {
	raw, err := required(s.db, numberKey(0, []byte{'n'}))
	if err != nil {
		return common.Hash{}, err
	}
	if len(raw) != 32 || common.BytesToHash(raw) == (common.Hash{}) {
		return common.Hash{}, errors.New("invalid source genesis")
	}
	return common.BytesToHash(raw), nil
}

func (s *source) confirmGenesis(want common.Hash) error {
	genesis, err := s.genesisIdentity()
	if err != nil {
		return err
	}
	if genesis != want {
		return errors.New("source genesis identity mismatch")
	}
	return nil
}
