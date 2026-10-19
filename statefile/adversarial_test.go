package statefile

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Bypass writer validation, but construct a correctly framed/checksummed file.
// This verifies the Reader rejects semantic corruption independently of hashes.
func uncheckedFile(t *testing.T, h FileHeader, records []Record, summary Summary) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := NewWriter(t.Context(), &out, h.HeaderRLP, h.Metadata, h.Compression)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Abort()
	for _, r := range records {
		p, err := encodeRecord(r, new([maxFixedPayload]byte))
		if err != nil {
			t.Fatal(err)
		}
		if err := w.write(r.Kind, p, true); err != nil {
			t.Fatal(err)
		}
	}
	p, err := rlp.EncodeToBytes(summary)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.digest.Write(p)
	if err := w.write(Footer, append(p, w.digest.Sum(nil)...), false); err != nil {
		t.Fatal(err)
	}
	if err := w.blocks.close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestReaderRejectsChecksummedSemanticCorruption(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		r, err := NewReader(t.Context(), bytes.NewReader(sample(t, compression)))
		if err != nil {
			t.Fatal(err)
		}
		var records []Record
		for {
			rec, err := r.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if rec.Kind != Footer {
				records = append(records, rec)
			}
		}
		summary, err := r.Result()
		if err != nil {
			t.Fatal(err)
		}
		h := r.Header()
		r.Close()
		for _, name := range []string{"account-duplicate", "storage-duplicate", "storage-order", "missing-code", "orphan-code", "duplicate-code", "code-hash", "wrong-root", "wrong-count", "wrong-storage-root"} {
			t.Run(compression+"/"+name, func(t *testing.T) {
				bad := append([]Record(nil), records...)
				s := summary
				switch name {
				case "account-duplicate":
					bad = append(append(append([]Record(nil), records[:3]...), records[:3]...), records[3])
				case "storage-duplicate":
					bad = []Record{records[0], records[1], records[1], records[2], records[3]}
				case "storage-order":
					small := records[1]
					small.Storage.Key = StorageKeyHash{}
					bad = []Record{records[0], records[1], small, records[2], records[3]}
				case "missing-code":
					bad = bad[:3]
				case "orphan-code":
					bad = append(bad, Record{Kind: CodeEntry, CodeHash: crypto.Keccak256Hash([]byte{1}), Code: []byte{1}})
				case "duplicate-code":
					bad = append(bad, records[3])
				case "code-hash":
					bad[3].Code = []byte{2}
				case "wrong-root":
					s.FinalStateRoot = common.HexToHash("0x01")
				case "wrong-count":
					s.Counts.Accounts++
				case "wrong-storage-root":
					bad[2].StorageRoot = common.HexToHash("0x01")
				}
				if _, err := Verify(t.Context(), bytes.NewReader(uncheckedFile(t, h, bad, s))); err == nil {
					t.Fatal("accepted semantic corruption with valid checksum")
				}
			})
		}
	}
}
