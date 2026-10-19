package statefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/holiman/uint256"
)

func compactRoundTrip(t *testing.T, record Record) []byte {
	t.Helper()
	p, err := encodeRecord(record, new([maxFixedPayload]byte))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRecord(record.Kind, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, record) {
		t.Fatalf("round trip changed record: %#v != %#v", decoded, record)
	}
	return p
}

func TestCompactIntegerBoundaries(t *testing.T) {
	for width := 0; width <= 32; width++ {
		for _, fill := range []byte{1, 255} {
			value := bytes.Repeat([]byte{fill}, width)
			balance := new(big.Int).SetBytes(value)
			for _, code := range []common.Hash{types.EmptyCodeHash, crypto.Keccak256Hash([]byte{1})} {
				for n := 0; n <= 8; n++ {
					nonce := uint64(0)
					for range n {
						nonce = nonce<<8 | uint64(fill)
					}
					a := Account{Balance: balance, Nonce: nonce, CodeHash: code}
					p := compactRoundTrip(t, Record{Kind: AccountStart, Account: a})
					expected := 23 + width + n
					if code != types.EmptyCodeHash {
						expected += 32
					}
					if len(p) != expected {
						t.Fatalf("account length %d != %d", len(p), expected)
					}
					root := types.EmptyRootHash
					if nonce != 0 {
						root = common.HexToHash("0x1234")
					}
					compactRoundTrip(t, Record{Kind: AccountEnd, StorageRoot: root, StorageCount: nonce})
				}
			}
			if width > 0 {
				compactRoundTrip(t, Record{Kind: StorageEntry, Storage: Storage{Key: StorageKeyHash(crypto.Keccak256Hash(value)), Value: common.BytesToHash(value)}})
			}
		}
	}
}

func TestCompactKnownBytes(t *testing.T) {
	a := Account{CodeHash: types.EmptyCodeHash, Balance: new(big.Int)}
	start := compactRoundTrip(t, Record{Kind: AccountStart, Account: a})
	end := compactRoundTrip(t, Record{Kind: AccountEnd, StorageRoot: types.EmptyRootHash})
	if !bytes.Equal(start, make([]byte, 23)) || !bytes.Equal(end, []byte{0}) || 2+len(start)+2+len(end) != 28 {
		t.Fatal("empty account encoding changed")
	}
	a.Balance.SetUint64(256)
	a.Nonce = 255
	start = compactRoundTrip(t, Record{Kind: AccountStart, Account: a})
	if !bytes.Equal(start[20:], []byte{0, 2, 1, 0, 1, 255}) {
		t.Fatalf("unexpected scalar wire bytes %x", start[20:])
	}
}

func TestCompactRejectsMalformedPayloads(t *testing.T) {
	empty := make([]byte, 23)
	explicit := append(append(bytes.Clone(empty[:20]), 1, 0, 0), types.EmptyCodeHash[:]...)
	cases := []struct {
		kind    Kind
		payload []byte
	}{
		{AccountStart, empty[:22]},
		{AccountStart, append(bytes.Clone(empty), 0)},
		{AccountStart, append(bytes.Clone(empty[:20]), 2, 0, 0)},
		{AccountStart, append(bytes.Clone(empty[:21]), 33, 1, 0)},
		{AccountStart, append(bytes.Clone(empty[:21]), 1, 0, 0)},
		{AccountStart, append(bytes.Clone(empty[:21]), 0, 9, 1)},
		{AccountStart, append(bytes.Clone(empty[:21]), 0, 1, 0)},
		{AccountStart, explicit},
		{AccountStart, append(append(bytes.Clone(empty[:20]), 1, 0, 0), make([]byte, 32)...)},
		{AccountStart, explicit[:len(explicit)-1]},
		{StorageEntry, append(make([]byte, 32), 0)},
		{StorageEntry, append(make([]byte, 32), 1, 0)},
		{StorageEntry, append(make([]byte, 32), 2, 1)},
		{StorageEntry, append(make([]byte, 32), 1, 1, 0)},
		{StorageEntry, append(make([]byte, 32), 33, 1)},
		{AccountEnd, []byte{0, 0}},
		{AccountEnd, []byte{1, 0}},
		{AccountEnd, []byte{1, 1}},
		{AccountEnd, append([]byte{0}, types.EmptyRootHash[:]...)},
		{AccountEnd, append([]byte{9}, make([]byte, 9)...)},
	}
	for i, c := range cases {
		if _, err := decodeRecord(c.kind, c.payload); err == nil {
			t.Fatalf("accepted case %d: %x", i, c.payload)
		}
	}
	for _, r := range []Record{
		{Kind: AccountEnd, StorageRoot: common.Hash{}},
		{Kind: StorageEntry},
		{Kind: AccountStart, Account: Account{CodeHash: types.EmptyCodeHash, Balance: big.NewInt(-1)}},
		{Kind: AccountStart, Account: Account{CodeHash: types.EmptyCodeHash, Balance: new(big.Int).Lsh(big.NewInt(1), 256)}},
		{Kind: AccountStart, Account: Account{Balance: new(big.Int)}},
	} {
		if _, err := encodeRecord(r, new([maxFixedPayload]byte)); err == nil {
			t.Fatal("accepted invalid writer fields")
		}
	}
}

func TestCompactRecordHeaders(t *testing.T) {
	for _, n := range []uint64{0, 1, 127, 128, 16383, 16384, 1<<21 - 1, 1 << 21, 1<<28 - 1, 1 << 28, math.MaxUint32} {
		p := binary.AppendUvarint([]byte{byte(CodeEntry)}, n)
		k, got, raw, err := readRecordHeader(bytes.NewReader(p), new([6]byte))
		if err != nil || k != CodeEntry || uint64(got) != n || !bytes.Equal(raw, p) {
			t.Fatalf("header %x: %d %v", p, got, err)
		}
	}
	for _, length := range [][]byte{{0x80, 0}, {0x81, 0}, {0xff, 0xff, 0xff, 0xff, 0x10}, {0x80, 0x80, 0x80, 0x80, 0x80, 0}, {0x80}} {
		if _, _, _, err := readRecordHeader(bytes.NewReader(append([]byte{1}, length...)), new([6]byte)); err == nil {
			t.Fatalf("accepted %x", length)
		}
	}
}

func TestCompactMalformedHeaderSticky(t *testing.T) {
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		for _, header := range [][]byte{{1, 0}, {1, 96}, {1, 0x80, 0}, {1, 0xff, 0xff, 0xff, 0xff, 0x10}, {1, 0x80}, {99, 1}} {
			r, err := NewReader(t.Context(), bytes.NewReader(sample(t, codec)))
			if err != nil {
				t.Fatal(err)
			}
			h := r.Header()
			r.Close()
			var out bytes.Buffer
			w, err := NewWriter(t.Context(), &out, h.HeaderRLP, h.Metadata, codec)
			if err != nil {
				t.Fatal(err)
			}
			if err := writeAll(w.blocks, header); err != nil {
				t.Fatal(err)
			}
			if err := w.blocks.close(); err != nil {
				t.Fatal(err)
			}
			w.Abort()
			r, err = NewReader(t.Context(), bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			_, first := r.Next()
			_, second := r.Next()
			if first == nil || errors.Is(first, io.EOF) || !errors.Is(second, first) {
				t.Fatalf("nonsticky invalid header: %v / %v", first, second)
			}
			if _, err := r.Result(); err == nil {
				t.Fatal("invalid header produced result")
			}
			r.Close()
		}
	}
}

func TestCompactHeadersAndPayloadAcrossBlocks(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, remaining := range []int{0, 1, 2, 3, 4, 16, 64} {
			var out bytes.Buffer
			w, err := newBlockWriter(&out, compressed)
			if err != nil {
				t.Fatal(err)
			}
			padding := blockSize - remaining
			if err := writeAll(w, make([]byte, padding)); err != nil {
				t.Fatal(err)
			}
			payload := bytes.Repeat([]byte{0xa5}, 257)
			header := binary.AppendUvarint([]byte{byte(CodeEntry)}, uint64(len(payload)))
			if err := writeAll(w, append(bytes.Clone(header), payload...)); err != nil {
				t.Fatal(err)
			}
			if err := w.close(); err != nil {
				t.Fatal(err)
			}
			r, err := newBlockReader(bytes.NewReader(out.Bytes()), compressed)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(io.Discard, r, int64(padding)); err != nil {
				t.Fatal(err)
			}
			k, n, h, err := readRecordHeader(r, new([6]byte))
			if err != nil || k != CodeEntry || n != uint32(len(payload)) || !bytes.Equal(h, header) {
				t.Fatalf("cross-block header: %v", err)
			}
			got := make([]byte, n)
			if _, err := io.ReadFull(r, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("cross-block payload changed")
			}
			var tail [1]byte
			if _, err := r.Read(tail[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("terminator: %v", err)
			}
			r.close()
		}
	}
}

// Empty code does not imply empty storage. Both omitted defaults and maximum
// scalars must rebuild the same Ethereum account and storage tries.
func TestCompactEmptyCodeWithStorage(t *testing.T) {
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		base, err := NewReader(t.Context(), bytes.NewReader(sample(t, codec)))
		if err != nil {
			t.Fatal(err)
		}
		h := base.Header()
		base.Close()
		var out bytes.Buffer
		w, err := NewWriter(t.Context(), &out, h.HeaderRLP, h.Metadata, codec)
		if err != nil {
			t.Fatal(err)
		}
		a := Account{Address: common.HexToAddress("0x123"), CodeHash: types.EmptyCodeHash, Balance: new(big.Int).SetBytes(bytes.Repeat([]byte{255}, 32)), Nonce: math.MaxUint64}
		s := Storage{Key: StorageKeyHash(crypto.Keccak256Hash([]byte{1})), Value: common.BytesToHash(bytes.Repeat([]byte{255}, 32))}
		storage := trie.NewStackTrie(nil)
		encoded, err := rlp.EncodeToBytes(s.Value[:])
		if err != nil {
			t.Fatal(err)
		}
		if err := storage.Update(s.Key[:], encoded); err != nil {
			t.Fatal(err)
		}
		root := storage.Hash()
		for _, rec := range []Record{{Kind: AccountStart, Account: a}, {Kind: StorageEntry, Storage: s}, {Kind: AccountEnd, StorageRoot: root, StorageCount: 1}} {
			if err := w.WriteRecord(rec); err != nil {
				t.Fatal(err)
			}
		}
		accountTrie := trie.NewStackTrie(nil)
		balance, overflow := uint256.FromBig(a.Balance)
		if overflow {
			t.Fatal("overflow")
		}
		encoded, err = rlp.EncodeToBytes(&types.StateAccount{Nonce: a.Nonce, Balance: balance, Root: root, CodeHash: a.CodeHash[:]})
		if err != nil {
			t.Fatal(err)
		}
		key := crypto.Keccak256Hash(a.Address[:])
		if err := accountTrie.Update(key[:], encoded); err != nil {
			t.Fatal(err)
		}
		final := accountTrie.Hash()
		counts := Counts{Accounts: 1, StorageSlots: 1}
		summary := Summary{SourceRoot: h.Header.Root, ConvertedStateRoot: final, FinalStateRoot: final, Counts: counts, ConvertedCounts: counts, Conversion: Conversion{OriginalSupply: a.Balance.String(), BalanceSum: a.Balance.String(), MigratedNative: a.Balance.String(), RemainingSupply: "0", SelfBalance: "0"}}
		if err := w.Close(summary); err != nil {
			t.Fatal(err)
		}
		got, err := Verify(t.Context(), bytes.NewReader(out.Bytes()))
		if err != nil || got != summary {
			t.Fatalf("full verification: %v", err)
		}
	}
}
