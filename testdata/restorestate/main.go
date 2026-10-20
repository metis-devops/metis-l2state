// restorestate creates a disposable, state-only legacy LevelDB fixture. It is
// deliberately not a production l2state command or a historical chain restore.
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/signal"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/storageword"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

func main() {
	dump := flag.String("dump", "", "original-state dump")
	preimages := flag.String("preimages", "", "geth export-preimages RLP gzip")
	genesis := flag.String("genesis", "", "genesis block JSON")
	target := flag.String("target", "", "optional target block JSON to compare with dump header")
	out := flag.String("out", "", "new state-only LevelDB directory")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := restore(ctx, *dump, *preimages, *genesis, *target, *out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// blockHeader accepts the numeric quantities used by the supplied block JSON
// as well as RPC hex quantities, and verifies its declared hash after encoding.
func blockHeader(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{"number", "difficulty", "gasLimit", "gasUsed", "timestamp"} {
		v := fields[key]
		if len(v) > 0 && v[0] != '"' {
			n, ok := new(big.Int).SetString(string(v), 10)
			if !ok || n.Sign() < 0 {
				return nil, fmt.Errorf("invalid %s", key)
			}
			fields[key], err = json.Marshal("0x" + n.Text(16))
			if err != nil {
				return nil, err
			}
		}
	}
	b, err = json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	var h types.Header
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	var declared common.Hash
	if err := json.Unmarshal(fields["hash"], &declared); err != nil {
		return nil, err
	}
	if h.Number == nil || !h.Number.IsUint64() || h.Hash() != declared {
		return nil, errors.New("block number/hash mismatch")
	}
	return rlp.EncodeToBytes(&h)
}

type sink struct {
	db    *leveldb.DB
	batch leveldb.Batch
	err   error
}

func (s *sink) put(k, v []byte) {
	if s.err != nil {
		return
	}
	s.batch.Put(k, v)
	if s.batch.Len() >= 16384 || len(s.batch.Dump()) >= 4<<20 {
		s.flush()
	}
}
func (s *sink) flush() {
	if s.err == nil {
		s.err = s.db.Write(&s.batch, nil)
		s.batch.Reset()
	}
}
func (s *sink) node(_ []byte, hash common.Hash, blob []byte) { s.put(hash[:], blob) }
func (s *sink) header(raw []byte) error {
	var h types.Header
	if err := rlp.DecodeBytes(raw, &h); err != nil {
		return err
	}
	hash := crypto.Keccak256Hash(raw)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], h.Number.Uint64())
	prefix := append([]byte{'h'}, n[:]...)
	s.put(append(append([]byte{}, prefix...), 'n'), hash[:])
	s.put(append(prefix, hash[:]...), raw)
	s.put(append([]byte{'H'}, hash[:]...), n[:])
	return s.err
}

func restore(ctx context.Context, dump, preimages, genesis, target, out string) (retErr error) {
	if dump == "" || preimages == "" || genesis == "" || out == "" {
		return errors.New("dump, preimages, genesis and out required")
	}
	g, err := blockHeader(genesis)
	if err != nil {
		return err
	}
	var gh types.Header
	if err := rlp.DecodeBytes(g, &gh); err != nil {
		return err
	}
	if gh.Number.Sign() != 0 {
		return errors.New("genesis number must be zero")
	}
	f, err := os.Open(dump)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	r, err := statefile.NewReader(ctx, bufio.NewReaderSize(f, 1<<20))
	if err != nil {
		return err
	}
	defer r.Close()
	header := r.Header()
	if target != "" {
		want, err := blockHeader(target)
		if err != nil {
			return err
		}
		if !bytes.Equal(want, header.HeaderRLP) {
			return errors.New("target header differs from dump RLP")
		}
	}
	if header.Metadata.Genesis != crypto.Keccak256Hash(g) {
		return errors.New("dump genesis mismatch")
	}
	if header.Metadata.IndexVersion != 0 {
		return errors.New("restore requires an original-state dump")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			retErr = errors.Join(retErr, os.RemoveAll(out))
		}
	}()
	db, err := leveldb.OpenFile(out, &opt.Options{ErrorIfExist: true, WriteBuffer: 32 << 20, BlockCacheCapacity: 64 << 20, Filter: filter.NewBloomFilter(10), DisableSeeksCompaction: true})
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, db.Close()); complete = complete && retErr == nil }()
	s := &sink{db: db}
	summary, err := importState(r, s)
	if err != nil {
		return err
	}
	if err := s.header(g); err != nil {
		return err
	}
	if err := s.header(header.HeaderRLP); err != nil {
		return err
	}
	hash := header.Header.Hash()
	s.put([]byte("LastBlock"), hash[:])
	s.put([]byte("LastHeader"), hash[:])
	n, err := importPreimages(ctx, preimages, s)
	if err != nil {
		return err
	}
	s.flush()
	if s.err != nil {
		return s.err
	}
	if err := db.Put([]byte("LastBlock"), hash[:], &opt.WriteOptions{Sync: true}); err != nil {
		return err
	}
	if err := json.NewEncoder(os.Stdout).Encode(struct {
		HeaderHash common.Hash
		Number     uint64
		Summary    statefile.Summary
		Preimages  uint64
	}{hash, header.Header.Number.Uint64(), summary, n}); err != nil {
		return err
	}
	complete = true
	return nil
}

type stateBuilder struct {
	sink              *sink
	accounts, storage *trie.StackTrie
	current           statefile.Account
	count, slots      uint64
}

func importState(r *statefile.Reader, s *sink) (statefile.Summary, error) {
	b := &stateBuilder{sink: s, accounts: trie.NewStackTrie(s.node)}
	last := time.Now()
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return statefile.Summary{}, fmt.Errorf("record after %d accounts/%d slots: %w", b.count, b.slots, err)
		}
		if err := b.accept(rec); err != nil {
			return statefile.Summary{}, err
		}
		if time.Since(last) > 10*time.Second {
			fmt.Fprintf(os.Stderr, "restore accounts=%d slots=%d\n", b.count, b.slots)
			last = time.Now()
		}
	}
	summary, err := r.Result()
	if err != nil {
		return summary, err
	}
	if b.accounts.Hash() != r.Header().Header.Root {
		return summary, errors.New("restored source root mismatch")
	}
	return summary, s.err
}

func (b *stateBuilder) accept(rec statefile.Record) error {
	s := b.sink
	switch rec.Kind {
	case statefile.AccountStart:
		b.current = rec.Account
		b.storage = trie.NewStackTrie(s.node)
		h := crypto.Keccak256Hash(b.current.Address[:])
		s.put(append([]byte("secure-key-"), h[:]...), b.current.Address[:])
	case statefile.StorageEntry:
		if err := storageword.Update(b.storage, common.Hash(rec.Storage.Key), rec.Storage.Value); err != nil {
			return err
		}
		b.slots++
	case statefile.AccountEnd:
		root := b.storage.Hash()
		if root != rec.StorageRoot {
			return errors.New("restored storage root mismatch")
		}
		balance, overflow := uint256.FromBig(b.current.Balance)
		if overflow {
			return errors.New("balance overflow")
		}
		blob, err := rlp.EncodeToBytes(&types.StateAccount{Nonce: b.current.Nonce, Balance: balance, Root: root, CodeHash: b.current.CodeHash.Bytes()})
		if err != nil {
			return err
		}
		h := crypto.Keccak256Hash(b.current.Address[:])
		if err := b.accounts.Update(h[:], blob); err != nil {
			return err
		}
		b.count++
	case statefile.CodeEntry:
		s.put(rec.CodeHash[:], rec.Code) // Legacy l2geth stores code at the bare hash.
	}
	return s.err
}

func importPreimages(ctx context.Context, path string, s *sink) (count uint64, retErr error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, f.Close()) }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return 0, err
	}
	defer func() { retErr = errors.Join(retErr, gz.Close()) }()
	stream := rlp.NewStream(bufio.NewReaderSize(gz, 1<<20), 0)
	for {
		if err := ctx.Err(); err != nil {
			return count, err
		}
		kind, size, err := stream.Kind()
		if errors.Is(err, io.EOF) {
			return count, nil
		}
		if err != nil {
			return count, err
		}
		if kind == rlp.List || size > 1<<20 {
			return count, errors.New("invalid or oversized preimage")
		}
		b, err := stream.Bytes()
		if err != nil {
			return count, err
		}
		h := crypto.Keccak256Hash(b)
		s.put(append([]byte("secure-key-"), h[:]...), b)
		if s.err != nil {
			return count, s.err
		}
		count++
		if count%1000000 == 0 {
			fmt.Fprintf(os.Stderr, "restore preimages=%d\n", count)
		}
	}
}
