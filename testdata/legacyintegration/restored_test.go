package legacyintegration

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/big"
	"os"
	"strconv"
	"testing"

	"github.com/MetisProtocol/mvm/l2geth/common"
	"github.com/MetisProtocol/mvm/l2geth/core/types"
	"github.com/MetisProtocol/mvm/l2geth/ethdb"
	"github.com/MetisProtocol/mvm/l2geth/rlp"
	"github.com/MetisProtocol/mvm/l2geth/trie"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// Only Get/Has are exposed to the independently pinned legacy trie reader.
// Any accidental write/batch/ancient access panics through the nil interface;
// the underlying LevelDB itself is also opened strictly read-only.
type restoredReader struct {
	ethdb.Database
	db *leveldb.DB
}

func (r restoredReader) Get(k []byte) ([]byte, error) { return r.db.Get(k, nil) }
func (r restoredReader) Has(k []byte) (bool, error)   { return r.db.Has(k, nil) }

func TestRestoredLegacyState(t *testing.T) {
	path := os.Getenv("L2STATE_RESTORED_SOURCE")
	if path == "" {
		t.Skip("set L2STATE_RESTORED_SOURCE for a restored mainnet fixture")
	}
	db, err := leveldb.OpenFile(path, &opt.Options{ReadOnly: true, ErrorIfMissing: true, Strict: opt.StrictAll, DisableSeeksCompaction: true, BlockCacheCapacity: restoredBudget(t, "L2STATE_RESTORED_CACHE_MB", 512) << 20, OpenFilesCacheCapacity: restoredBudget(t, "L2STATE_RESTORED_HANDLES", 2048), Filter: filter.NewBloomFilter(10)})
	check(t, err)
	defer func() { check(t, db.Close()) }()
	reader := restoredReader{db: db}
	hash, err := reader.Get([]byte("LastBlock"))
	check(t, err)
	number, err := reader.Get(append([]byte{'H'}, hash...))
	check(t, err)
	if len(number) != 8 {
		t.Fatal("invalid head number")
	}
	prefix := append([]byte{'h'}, number...)
	canonical, err := reader.Get(append(append([]byte{}, prefix...), 'n'))
	check(t, err)
	if !bytes.Equal(canonical, hash) {
		t.Fatal("head is not canonical")
	}
	raw, err := reader.Get(append(prefix, hash...))
	check(t, err)
	var header types.Header
	check(t, rlp.DecodeBytes(raw, &header))
	if !bytes.Equal(header.Hash().Bytes(), hash) || header.Number.Uint64() != binary.BigEndian.Uint64(number) {
		t.Fatal("header identity mismatch")
	}
	nodes := trie.NewDatabase(reader)
	accounts, err := trie.New(header.Root, nodes)
	check(t, err)
	it := trie.NewIterator(accounts.NodeIterator(nil))
	var count, slots, codes, codeBytes uint64
	seen := make(map[common.Hash]bool)
	for it.Next() {
		var a struct {
			Nonce    uint64
			Balance  *big.Int
			Root     common.Hash
			CodeHash []byte
		}
		check(t, rlp.DecodeBytes(it.Value, &a))
		preimage, err := reader.Get(append([]byte("secure-key-"), it.Key...))
		check(t, err)
		if len(preimage) != 20 || !bytes.Equal(crypto.Keccak256(preimage), it.Key) {
			t.Fatal("invalid address preimage")
		}
		storage, err := trie.New(a.Root, nodes)
		check(t, err)
		st := trie.NewIterator(storage.NodeIterator(nil))
		for st.Next() {
			slots++
		}
		check(t, st.Err)
		h := common.BytesToHash(a.CodeHash)
		if !bytes.Equal(a.CodeHash, crypto.Keccak256(nil)) && !seen[h] {
			code, err := reader.Get(a.CodeHash)
			check(t, err)
			if !bytes.Equal(crypto.Keccak256(code), a.CodeHash) {
				t.Fatal("invalid legacy code")
			}
			codes++
			codeBytes += uint64(len(code))
			seen[h] = true
		}
		count++
	}
	check(t, it.Err)
	result := struct {
		Number                                   uint64
		Hash, Root                               common.Hash
		Accounts, StorageSlots, Codes, CodeBytes uint64
	}{header.Number.Uint64(), header.Hash(), header.Root, count, slots, codes, codeBytes}
	b, err := json.Marshal(result)
	check(t, err)
	t.Log(string(b))
}

func restoredBudget(t *testing.T, name string, fallback int) int {
	t.Helper()
	if text := os.Getenv(name); text != "" {
		n, err := strconv.Atoi(text)
		check(t, err)
		if n <= 0 {
			t.Fatalf("%s must be positive", name)
		}
		return n
	}
	return fallback
}
