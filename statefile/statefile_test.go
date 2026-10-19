package statefile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

func sample(t testing.TB, compression string) []byte {
	t.Helper()
	header := &types.Header{Number: big.NewInt(7), Difficulty: new(big.Int), Root: types.EmptyRootHash, GasLimit: 8000000}
	raw, err := rlp.EncodeToBytes(header)
	if err != nil {
		t.Fatal(err)
	}
	m := Metadata{ToolVersion: "test", IndexVersion: 1, Genesis: common.HexToHash("0x01"), IndexHeadNumber: 7, IndexHeadHash: crypto.Keccak256Hash(raw), IndexDigest: common.HexToHash("0x02")}
	var out bytes.Buffer
	w, err := NewWriter(context.Background(), &out, raw, m, compression)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	a := Account{Address: common.HexToAddress("0x01"), CodeHash: crypto.Keccak256Hash([]byte{0}), Balance: big.NewInt(9), Nonce: 3}
	slot := Storage{Key: StorageKeyHash(common.HexToHash("0x23")), Value: common.HexToHash("0x17")}
	storage := trie.NewStackTrie(nil)
	blob, _ := rlp.EncodeToBytes([]byte{0x17})
	if err := storage.Update(slot.Key[:], blob); err != nil {
		t.Fatal(err)
	}
	root := storage.Hash()
	records := []Record{{Kind: AccountStart, Account: a}, {Kind: StorageEntry, Storage: slot}, {Kind: AccountEnd, StorageRoot: root, StorageCount: 1}, {Kind: CodeEntry, CodeHash: a.CodeHash, Code: []byte{0}}}
	for _, r := range records {
		if err := w.WriteRecord(r); err != nil {
			t.Fatal(err)
		}
	}
	accounts := trie.NewStackTrie(nil)
	accountRLP, _ := rlp.EncodeToBytes(&types.StateAccount{Nonce: 3, Balance: uint256.NewInt(9), Root: root, CodeHash: a.CodeHash[:]})
	hash := crypto.Keccak256Hash(a.Address[:])
	if err := accounts.Update(hash[:], accountRLP); err != nil {
		t.Fatal(err)
	}
	final := accounts.Hash()
	counts := Counts{1, 1, 1, 1}
	summary := Summary{SourceRoot: header.Root, ConvertedStateRoot: final, FinalStateRoot: final, Counts: counts, ConvertedCounts: counts, Conversion: Conversion{OriginalSupply: "9", BalanceSum: "9", MigratedNative: "9", RemainingSupply: "0", SelfBalance: "0"}}
	if err := w.Close(summary); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}
func TestRoundTrip(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		t.Run(compression, func(t *testing.T) {
			data := sample(t, compression)
			for range 2 {
				r, err := NewReader(t.Context(), bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := r.Result(); err == nil {
					t.Fatal("accepted unread file")
				}
				var kinds []Kind
				for {
					record, err := r.Next()
					if errors.Is(err, io.EOF) {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					kinds = append(kinds, record.Kind)
				}
				s, err := r.Result()
				if err != nil {
					t.Fatal(err)
				}
				if s.FinalStateRoot == r.Header().Header.Root || len(kinds) != 5 {
					t.Fatal("source/final roots or records")
				}
				r.Close()
			}
			if !bytes.Equal(data, sample(t, compression)) {
				t.Fatal("nondeterministic bytes")
			}
		})
	}
}
func TestCorruption(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		data := sample(t, compression)
		for _, n := range []int{0, 8, 13, len(data) / 2, len(data) - 1, len(data) - 8} {
			if _, err := Verify(t.Context(), bytes.NewReader(data[:n])); err == nil {
				t.Fatalf("accepted truncation %s %d", compression, n)
			}
		}
		for _, tail := range [][]byte{{0}, {1, 2, 3}, data} {
			bad := append(bytes.Clone(data), tail...)
			if _, err := Verify(t.Context(), bytes.NewReader(bad)); err == nil {
				t.Fatal("accepted trailing data")
			}
		}
		bad := bytes.Clone(data)
		bad[len(bad)/2] ^= 1
		if _, err := Verify(t.Context(), bytes.NewReader(bad)); err == nil {
			t.Fatal("accepted corruption")
		}
	}
}
func TestSemanticRejection(t *testing.T) {
	a := Account{Address: common.HexToAddress("0x01"), CodeHash: types.EmptyCodeHash, Balance: new(big.Int)}
	tests := [][]Record{
		{{Kind: StorageEntry}}, {{Kind: AccountEnd}}, {{Kind: CodeEntry, Code: []byte{1}, CodeHash: crypto.Keccak256Hash([]byte{1})}},
		{{Kind: AccountStart, Account: a}, {Kind: AccountStart, Account: a}},
		{{Kind: AccountStart, Account: a}, {Kind: AccountEnd, StorageRoot: common.Hash{}}},
		{{Kind: AccountStart, Account: a}, {Kind: StorageEntry, Storage: Storage{}}},
		{{Kind: AccountStart, Account: a}, {Kind: AccountEnd, StorageRoot: types.EmptyRootHash}, {Kind: AccountStart, Account: a}},
	}
	for i, records := range tests {
		v := newValidator()
		var err error
		for _, r := range records {
			err = v.accept(r)
			if err != nil {
				break
			}
		}
		if err == nil {
			t.Fatalf("accepted invalid sequence %d", i)
		}
	}
}
func TestMissingCode(t *testing.T) {
	v := newValidator()
	if err := v.accept(Record{Kind: AccountStart, Account: Account{CodeHash: crypto.Keccak256Hash([]byte{1}), Balance: new(big.Int)}}); err != nil {
		t.Fatal(err)
	}
	if err := v.accept(Record{Kind: AccountEnd, StorageRoot: types.EmptyRootHash}); err != nil {
		t.Fatal(err)
	}
	if err := v.finish(Summary{}, common.Hash{}); err == nil {
		t.Fatal("accepted missing code")
	}
}
func FuzzFile(f *testing.F) {
	f.Add(sample(f, CompressionNone))
	f.Add(sample(f, CompressionZstd))
	f.Add([]byte("bad"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 2<<20 {
			t.Skip()
		}
		_, _ = Verify(t.Context(), bytes.NewReader(b))
	})
}
func FuzzRecord(f *testing.F) {
	f.Add(byte(AccountStart), make([]byte, 23))
	f.Add(byte(StorageEntry), append(make([]byte, 32), 1, 1))
	f.Add(byte(AccountEnd), []byte{0})
	f.Add(byte(AccountEnd), append([]byte{1, 1}, types.EmptyRootHash[:]...))
	f.Fuzz(func(t *testing.T, k byte, b []byte) {
		if len(b) > 1<<20 {
			t.Skip()
		}
		_, _ = decodeRecord(Kind(k), b)
	})
}
