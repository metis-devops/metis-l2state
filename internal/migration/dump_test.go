package migration

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

// A non-OVM source with native funds, an explicitly empty account, shared code
// and no event file. Negative slots selects a completely empty state trie.
func newDumpFixture(t *testing.T, slots int) (fixture, map[common.Address]*types.StateAccount) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	db, err := leveldb.OpenFile(path, nil)
	must(t, err)
	put := func(k, v []byte) { must(t, db.Put(k, v, nil)) }
	sink := func(_ []byte, h common.Hash, b []byte) { put(h[:], b) }
	storage := make(map[common.Hash]common.Hash)
	for n := range max(0, slots) {
		storage[crypto.Keccak256Hash(new(big.Int).SetInt64(int64(n)).Bytes())] = common.BigToHash(big.NewInt(int64(n + 1)))
	}
	st := trie.NewStackTrie(sink)
	for _, k := range sortedHashes(storage) {
		must(t, st.Update(k[:], slotRLP(storage[k])))
	}
	code := []byte{0x60, 0x42, 0x00}
	ch := crypto.Keccak256Hash(code)
	accounts := make(map[common.Address]*types.StateAccount)
	if slots >= 0 {
		put(ch[:], code)
		accounts[eoa] = &types.StateAccount{Balance: uint256.NewInt(1234567), Nonce: 9, Root: types.EmptyRootHash, CodeHash: types.EmptyCodeHash.Bytes()}
		accounts[ordinary] = &types.StateAccount{Balance: uint256.NewInt(42), Nonce: 7, Root: st.Hash(), CodeHash: ch.Bytes()}
		accounts[automatic] = &types.StateAccount{Balance: new(uint256.Int), Nonce: 1, Root: types.EmptyRootHash, CodeHash: ch.Bytes()}
		accounts[manual] = emptyAccount()
	}
	byHash := make(map[common.Hash]*types.StateAccount)
	for a, account := range accounts {
		h := crypto.Keccak256Hash(a[:])
		byHash[h] = account
		put(append([]byte(preimageKeyPrefix), h[:]...), a[:])
	}
	stack := trie.NewStackTrie(sink)
	for _, h := range sortedHashes(byHash) {
		b, err := rlp.EncodeToBytes(byHash[h])
		must(t, err)
		must(t, stack.Update(h[:], b))
	}
	g := &types.Header{Number: new(big.Int), Difficulty: big.NewInt(1), Root: stack.Hash(), GasLimit: 10000000}
	h := types.CopyHeader(g)
	h.Number = big.NewInt(1)
	h.ParentHash = g.Hash()
	for _, header := range []*types.Header{g, h} {
		raw, err := rlp.EncodeToBytes(header)
		must(t, err)
		hash := header.Hash()
		put(numberKey(header.Number.Uint64(), hash[:]), raw)
		put(numberKey(header.Number.Uint64(), []byte{'n'}), hash[:])
		var number [8]byte
		binary.BigEndian.PutUint64(number[:], header.Number.Uint64())
		put(append([]byte{'H'}, hash[:]...), number[:])
	}
	hash := h.Hash()
	put([]byte("LastBlock"), hash[:])
	// Unrelated OVM index bytes must never be read or validated by dump.
	put(append([]byte("ovm-allowance-v1-"), 0xff), []byte("invalid"))
	put(append([]byte("ovm-transfer-v1/member/"), 0xff), []byte("invalid"))
	must(t, db.Close())
	return fixture{source: path, header: h, genesis: g, storage: storage}, accounts
}

func dumpOptions(f fixture, out string) DumpOptions {
	return DumpOptions{Source: f.source, Output: out, Workers: 2, CacheMB: 16, Handles: 16}
}

func TestDumpRoundTrip(t *testing.T) {
	for _, slots := range []int{-1, 0, 1024, 1025} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			f, want := newDumpFixture(t, slots)
			before := filesDigest(t, f.source)
			for _, compression := range []string{"none", "zstd"} {
				var first []byte
				for _, workers := range []int{2, 4, 16} {
					o := dumpOptions(f, filepath.Join(t.TempDir(), "dump"))
					o.Compression, o.Workers = compression, workers
					result, err := Dump(t.Context(), o)
					must(t, err)
					accounts, storage, summary := readArtifact(t, o.Output)
					if len(accounts) != len(want) || result.Summary != summary || summary.SourceRoot != f.header.Root || summary.ConvertedStateRoot != f.header.Root || summary.FinalStateRoot != f.header.Root || summary.Counts != summary.ConvertedCounts {
						t.Fatal("dump changed inventory or roots")
					}
					for a, expected := range want {
						got, ok := accounts[a]
						if !ok || got.Balance.Cmp(expected.Balance.ToBig()) != 0 || got.Nonce != expected.Nonce || got.CodeHash != common.BytesToHash(expected.CodeHash) {
							t.Fatalf("changed account %s: %+v", a, got)
						}
						if a == ordinary {
							if !maps.Equal(storage[a], f.storage) {
								t.Fatal("changed storage keys/values")
							}
						} else if len(storage[a]) != 0 {
							t.Fatal("invented storage")
						}
					}
					data, err := os.ReadFile(o.Output)
					must(t, err)
					if first == nil {
						first = data
					} else if !bytes.Equal(first, data) {
						t.Fatal("workers changed dump bytes")
					}
					checkDumpHeaderAndCode(t, data, f, slots >= 0)
				}
			}
			if filesDigest(t, f.source) != before {
				t.Fatal("dump modified source")
			}
		})
	}
}

func checkDumpHeaderAndCode(t *testing.T, data []byte, f fixture, hasCode bool) {
	t.Helper()
	r, err := statefile.NewReader(t.Context(), bytes.NewReader(data))
	must(t, err)
	defer r.Close()
	h := r.Header()
	raw, err := rlp.EncodeToBytes(f.header)
	must(t, err)
	if !bytes.Equal(raw, h.HeaderRLP) || h.Metadata.IndexVersion != 0 || h.Metadata.Genesis != f.genesis.Hash() || len(h.Metadata.Inputs) != 0 {
		t.Fatal("changed header or fabricated provenance")
	}
	codes := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		if rec.Kind == statefile.CodeEntry {
			codes++
			if !bytes.Equal(rec.Code, []byte{0x60, 0x42, 0x00}) {
				t.Fatal("changed code")
			}
		}
	}
	if (hasCode && codes != 1) || (!hasCode && codes != 0) {
		t.Fatal("code not deduplicated")
	}
}

func TestDumpDoesNotApplyOVMPolicy(t *testing.T) {
	f := newFixtureWithStorage(t, 3, 1, func(storage map[common.Hash]common.Hash) {
		storage[common.HexToHash("0x1234")] = common.HexToHash("0x5678")
	})
	must(t, os.Remove(f.runtime))
	must(t, os.Remove(f.eventPath()))
	o := dumpOptions(f, filepath.Join(t.TempDir(), "dump"))
	result, err := Dump(t.Context(), o)
	must(t, err)
	accounts, storage, summary := readArtifact(t, o.Output)
	if result.Summary != summary || summary.FinalStateRoot != f.header.Root || accounts[ovmAddress].CodeHash != f.ovmCode || accounts[ovmAddress].Balance.Sign() != 0 || storage[ovmAddress][common.HexToHash("0x1234")] != common.HexToHash("0x5678") {
		t.Fatal("OVM state transformed")
	}
	if _, ok := accounts[eoa]; ok {
		t.Fatal("created OVM token holder account")
	}
	supply := common.HexToHash("0x02")
	if storage[ovmAddress][crypto.Keccak256Hash(supply[:])] != common.BigToHash(big.NewInt(164)) || storage[ovmAddress][balanceKey(eoa)] != common.HexToHash("0x0b") {
		t.Fatal("changed OVM supply or balances")
	}
}

func TestDumpRejectsDamagedSource(t *testing.T) {
	for _, damage := range []string{"preimage-missing", "preimage-hash", "preimage-length", "preimage-empty", "code-missing", "code-hash", "storage", "state", "genesis-missing", "genesis-zero", "genesis-short", "head"} {
		t.Run(damage, func(t *testing.T) {
			f, accounts := newDumpFixture(t, 3)
			mutate(t, f, func(db *leveldb.DB) {
				a := accounts[ordinary]
				h := crypto.Keccak256Hash(ordinary[:])
				preimage := append([]byte(preimageKeyPrefix), h[:]...)
				switch damage {
				case "preimage-missing":
					must(t, db.Delete(preimage, nil))
				case "preimage-hash":
					must(t, db.Put(preimage, eoa[:], nil))
				case "preimage-length":
					must(t, db.Put(preimage, ordinary[:19], nil))
				case "preimage-empty":
					must(t, db.Put(preimage, nil, nil))
				case "code-missing":
					must(t, db.Delete(a.CodeHash, nil))
				case "code-hash":
					must(t, db.Put(a.CodeHash, []byte{0xff}, nil))
				case "storage":
					must(t, db.Delete(a.Root[:], nil))
				case "state":
					must(t, db.Delete(f.header.Root[:], nil))
				case "genesis-missing":
					must(t, db.Delete(numberKey(0, []byte{'n'}), nil))
				case "genesis-zero":
					must(t, db.Put(numberKey(0, []byte{'n'}), make([]byte, 32), nil))
				case "genesis-short":
					must(t, db.Put(numberKey(0, []byte{'n'}), []byte{1}, nil))
				case "head":
					must(t, db.Put([]byte("LastBlock"), make([]byte, 32), nil))
				}
			})
			before := filesDigest(t, f.source)
			dir := t.TempDir()
			o := dumpOptions(f, filepath.Join(dir, "dump"))
			var progress bytes.Buffer
			o.Progress = &progress
			if _, err := Dump(t.Context(), o); err == nil {
				t.Fatal("accepted damaged source")
			}
			duringOutput := strings.HasPrefix(damage, "preimage-") || strings.HasPrefix(damage, "code-") || damage == "storage" || damage == "state"
			if bytes.Contains(progress.Bytes(), []byte("phase=write_state")) != duringOutput {
				t.Fatal("source validation ran in the wrong phase")
			}
			if bytes.Contains(progress.Bytes(), []byte("phase=complete")) {
				t.Fatal("damaged source was published")
			}
			assertEmptyDirectory(t, dir)
			if before != filesDigest(t, f.source) {
				t.Fatal("failed dump changed source")
			}
		})
	}
	// Even the well-known OVM_ETH address must come from the source preimages.
	f := newFixture(t, 0, 0)
	mutate(t, f, func(db *leveldb.DB) { must(t, db.Delete(append([]byte(preimageKeyPrefix), ovmHash[:]...), nil)) })
	dir := t.TempDir()
	if _, err := Dump(t.Context(), dumpOptions(f, filepath.Join(dir, "dump"))); err == nil || !strings.Contains(err.Error(), "preimage") {
		t.Fatalf("seeded OVM_ETH: %v", err)
	}
	assertEmptyDirectory(t, dir)
}

func assertEmptyDirectory(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	if len(entries) != 0 {
		t.Fatalf("left output or partial: %v", entries)
	}
}

func TestDumpCancellationAndDestinationRace(t *testing.T) {
	f, _ := newDumpFixture(t, 1025)
	for _, phase := range []string{"prepare", "write_state"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			o := dumpOptions(f, filepath.Join(dir, "dump"))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			o.Progress = progressHook(func(p []byte) {
				if bytes.Contains(p, []byte("phase="+phase)) {
					cancel()
				}
			})
			if _, err := Dump(ctx, o); !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			assertEmptyDirectory(t, dir)
			o.Progress = nil
			_, err := Dump(t.Context(), o)
			must(t, err)
		})
	}
	dir := t.TempDir()
	o := dumpOptions(f, filepath.Join(dir, "dump"))
	var hookErr error
	o.Progress = progressHook(func(p []byte) {
		if bytes.Contains(p, []byte("phase=write_state")) {
			hookErr = os.WriteFile(o.Output, []byte("existing"), 0600)
		}
	})
	if _, err := Dump(t.Context(), o); err == nil {
		t.Fatal("replaced competing output")
	}
	must(t, hookErr)
	b, err := os.ReadFile(o.Output)
	must(t, err)
	entries, err := os.ReadDir(dir)
	must(t, err)
	if string(b) != "existing" || len(entries) != 1 {
		t.Fatal("replaced output or leaked partial")
	}
}

func TestDumpValidationCannotAuthorizeMigration(t *testing.T) {
	f, _ := newDumpFixture(t, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	// A raw-source proof must still be rejected by migration, even though
	// production dump no longer constructs indexes or validation evidence.
	x := &indexes{}
	_, err = s.walkWithPolicy(t.Context(), 2, l, x, nil, nil, nil, rawSource)
	must(t, err)
	x.validation = &sourceValidation{source: s, head: s.head, policy: rawSource}
	_, err = s.walk(t.Context(), 2, l, x, nil, nil, func(statefile.Record) error { t.Fatal("cross-policy validation reached output"); return nil })
	if err == nil || !strings.Contains(err.Error(), "policy") {
		t.Fatalf("accepted dump proof for migration: %v", err)
	}
	_, err = s.walk(t.Context(), 2, l, nil, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "zero source native") {
		t.Fatalf("migration accepted native balances: %v", err)
	}
}

func TestDumpWriteFailureJoinsWorkers(t *testing.T) {
	f, _ := newDumpFixture(t, 1025)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	var x *indexes
	for _, cancelOutput := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		sentinel := errors.New("dump output failed")
		calls := 0
		_, err := s.walkWithPolicy(ctx, 2, l, x, nil, nil, func(statefile.Record) error {
			calls++
			if calls == 70 {
				if cancelOutput {
					cancel()
					return ctx.Err()
				}
				return sentinel
			}
			return nil
		}, rawSource)
		cancel()
		want := sentinel
		if cancelOutput {
			want = context.Canceled
		}
		if !errors.Is(err, want) || calls != 70 || len(l) != 0 {
			t.Fatalf("lost failure or worker join: %v, calls=%d permits=%d", err, calls, len(l))
		}
	}
	// The same source is usable after either failure and no jobs remain.
	_, err = s.walkWithPolicy(t.Context(), 2, l, x, nil, nil, func(statefile.Record) error { return nil }, rawSource)
	must(t, err)
}

func TestDumpInvalidOptions(t *testing.T) {
	for name, o := range map[string]DumpOptions{
		"source":       {Output: "unused"},
		"output":       {Source: "unused"},
		"workers-low":  {Source: "unused", Output: "unused", Workers: 1},
		"workers-high": {Source: "unused", Output: "unused", Workers: 17},
		"cache":        {Source: "unused", Output: "unused", CacheMB: -1},
		"handles":      {Source: "unused", Output: "unused", Handles: -1},
		"compression":  {Source: "unused", Output: "unused", Compression: "gzip"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Dump(t.Context(), o); err == nil || errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid options reached source: %v", err)
			}
		})
	}
	f, _ := newDumpFixture(t, 0)
	before := filesDigest(t, f.source)
	if _, err := Dump(t.Context(), dumpOptions(f, filepath.Join(f.source, "dump"))); err == nil {
		t.Fatal("accepted output in source")
	}
	if before != filesDigest(t, f.source) {
		t.Fatal("invalid output modified source")
	}
}
