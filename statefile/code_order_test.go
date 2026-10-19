package statefile

import (
	"bytes"
	"errors"
	"io"
	"math/big"
	"slices"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestCodeOrderAndNonAdjacentDuplicates(t *testing.T) {
	for _, codec := range []string{CompressionNone, CompressionZstd} {
		source, err := NewReader(t.Context(), bytes.NewReader(sample(t, codec)))
		if err != nil {
			t.Fatal(err)
		}
		header := source.Header()
		source.Close()
		var out bytes.Buffer
		w, err := NewWriter(t.Context(), &out, header.HeaderRLP, header.Metadata, codec)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Abort()
		codes := []Record{{Kind: CodeEntry, Code: []byte{1}}, {Kind: CodeEntry, Code: []byte{2}}}
		for i := range codes {
			codes[i].CodeHash = crypto.Keccak256Hash(codes[i].Code)
		}
		slices.SortFunc(codes, func(a, b Record) int { return -bytes.Compare(a.CodeHash[:], b.CodeHash[:]) })
		addresses := []common.Address{common.HexToAddress("0x1"), common.HexToAddress("0x2")}
		slices.SortFunc(addresses, func(a, b common.Address) int {
			x, y := crypto.Keccak256Hash(a[:]), crypto.Keccak256Hash(b[:])
			return bytes.Compare(x[:], y[:])
		})
		var records []Record
		for i, a := range addresses {
			records = append(records, Record{Kind: AccountStart, Account: Account{Address: a, CodeHash: codes[i].CodeHash, Balance: new(big.Int)}}, Record{Kind: AccountEnd, StorageRoot: types.EmptyRootHash})
		}
		records = append(records, codes...)
		for _, record := range records {
			if err := w.WriteRecord(record); err != nil {
				t.Fatal(err)
			}
		}
		root := w.state.accounts.Hash()
		summary := Summary{SourceRoot: types.EmptyRootHash, ConvertedStateRoot: root, FinalStateRoot: root, Counts: w.state.counts, ConvertedCounts: w.state.counts,
			Conversion: Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"}}
		if err := w.Close(summary); err != nil {
			t.Fatal(err)
		}
		if got, err := Verify(t.Context(), bytes.NewReader(out.Bytes())); err != nil || got != summary {
			t.Fatalf("descending code rejected: %v", err)
		}
		bad := append(slices.Clone(records), codes[0])
		if _, err := Verify(t.Context(), bytes.NewReader(uncheckedFile(t, header, bad, summary))); err == nil {
			t.Fatal("accepted nonadjacent duplicate")
		}
		// Verify the reader preserves the producer's order.
		r, err := NewReader(t.Context(), bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		var got []common.Hash
		for {
			record, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if record.Kind == CodeEntry {
				got = append(got, record.CodeHash)
			}
		}
		r.Close()
		if !slices.Equal(got, []common.Hash{codes[0].CodeHash, codes[1].CodeHash}) {
			t.Fatal("reader reordered code")
		}
	}
}
