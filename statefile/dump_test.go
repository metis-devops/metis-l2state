package statefile

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
)

func dumpSample(t *testing.T, compression string) (FileHeader, []Record, Summary) {
	t.Helper()
	r, err := NewReader(t.Context(), bytes.NewReader(sample(t, compression)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
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
	s, err := r.Result()
	if err != nil {
		t.Fatal(err)
	}
	h := r.Header()
	h.Header.Root = s.FinalStateRoot
	h.HeaderRLP, err = rlp.EncodeToBytes(h.Header)
	if err != nil {
		t.Fatal(err)
	}
	h.Metadata = Metadata{ToolVersion: "test", Genesis: h.Metadata.Genesis}
	s.SourceRoot = s.FinalStateRoot
	s.Conversion = Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"}
	return h, records, s
}

func TestDumpFormat(t *testing.T) {
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		h, records, s := dumpSample(t, compression)
		var out bytes.Buffer
		w, err := NewWriter(t.Context(), &out, h.HeaderRLP, h.Metadata, compression)
		if err != nil {
			t.Fatal(err)
		}
		defer w.Abort()
		for _, rec := range records {
			if err := w.WriteRecord(rec); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(s); err != nil {
			t.Fatal(err)
		}
		got, err := Verify(t.Context(), bytes.NewReader(out.Bytes()))
		if err != nil || got != s {
			t.Fatalf("dump roundtrip: %v, %+v", err, got)
		}
	}
}

func TestDumpProvenanceRejectedByReaderAndWriter(t *testing.T) {
	h, _, _ := dumpSample(t, CompressionNone)
	cases := map[string]func(*Metadata){
		"tool":                    func(m *Metadata) { m.ToolVersion = "" },
		"genesis":                 func(m *Metadata) { m.Genesis = common.Hash{} },
		"version":                 func(m *Metadata) { m.IndexVersion = 2 },
		"migration-without-index": func(m *Metadata) { m.IndexVersion = 1 },
		"index-number":            func(m *Metadata) { m.IndexHeadNumber = 7 },
		"index-hash":              func(m *Metadata) { m.IndexHeadHash[0] = 1 },
		"index-digest":            func(m *Metadata) { m.IndexDigest[0] = 1 },
		"allowances":              func(m *Metadata) { m.AllowanceCount = 1 },
		"members":                 func(m *Metadata) { m.MemberCount = 1 },
		"input":                   func(m *Metadata) { m.Inputs = []InputDigest{{Name: "runtime", SHA256: common.HexToHash("0x01")}} },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			m := h.Metadata
			change(&m)
			w, err := NewWriter(t.Context(), io.Discard, h.HeaderRLP, m, CompressionNone)
			if err == nil {
				w.Abort()
				t.Fatal("writer accepted invalid dump metadata")
			}
			p, err := rlp.EncodeToBytes(headerWire{h.HeaderRLP, m})
			if err != nil {
				t.Fatal(err)
			}
			prefix := make([]byte, 13)
			copy(prefix, magic[:])
			binary.BigEndian.PutUint32(prefix[9:], uint32(len(p)))
			r, err := NewReader(t.Context(), bytes.NewReader(append(prefix, p...)))
			if err == nil {
				r.Close()
				t.Fatal("reader accepted invalid dump metadata")
			}
		})
	}
}

func TestDumpSummaryRejectedByReaderAndWriter(t *testing.T) {
	cases := map[string]func(*Summary){
		"source-root":        func(s *Summary) { s.SourceRoot[0] ^= 1 },
		"converted-root":     func(s *Summary) { s.ConvertedStateRoot[0] ^= 1 },
		"final-root":         func(s *Summary) { s.FinalStateRoot[0] ^= 1 },
		"counts":             func(s *Summary) { s.Counts.Accounts++ },
		"converted-accounts": func(s *Summary) { s.ConvertedCounts.Accounts++ },
		"converted-slots":    func(s *Summary) { s.ConvertedCounts.StorageSlots++ },
		"converted-codes":    func(s *Summary) { s.ConvertedCounts.Codes++ },
		"converted-bytes":    func(s *Summary) { s.ConvertedCounts.CodeBytes++ },
		"supply":             func(s *Summary) { s.Conversion.OriginalSupply = "1" },
		"balances":           func(s *Summary) { s.Conversion.BalanceSum = "1" },
		"native":             func(s *Summary) { s.Conversion.MigratedNative = "1" },
		"remaining":          func(s *Summary) { s.Conversion.RemainingSupply = "1" },
		"self":               func(s *Summary) { s.Conversion.SelfBalance = "1" },
		"eoa":                func(s *Summary) { s.Conversion.EOAs = 1 },
		"contracts":          func(s *Summary) { s.Conversion.ConvertedContracts = 1 },
		"retained":           func(s *Summary) { s.Conversion.RetainedContracts = 1 },
		"audit-height":       func(s *Summary) { s.Conversion.AuditHeight = 1 },
		"audit-hash":         func(s *Summary) { s.Conversion.AuditHash[0] = 1 },
		"valid-migration-amounts": func(s *Summary) {
			s.Conversion.OriginalSupply = "9"
			s.Conversion.BalanceSum = "9"
			s.Conversion.MigratedNative = "9"
		},
	}
	for _, compression := range []string{CompressionNone, CompressionZstd} {
		h, records, summary := dumpSample(t, compression)
		for name, change := range cases {
			t.Run(compression+"/"+name, func(t *testing.T) {
				s := summary
				change(&s)
				w, err := NewWriter(t.Context(), io.Discard, h.HeaderRLP, h.Metadata, compression)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Abort()
				for _, rec := range records {
					if err := w.WriteRecord(rec); err != nil {
						t.Fatal(err)
					}
				}
				if err := w.Close(s); err == nil {
					t.Fatal("writer accepted invalid dump summary")
				}
				if _, err := Verify(t.Context(), bytes.NewReader(uncheckedFile(t, h, records, s))); err == nil {
					t.Fatal("reader accepted checksummed invalid dump summary")
				}
			})
		}
		// A valid migration whose final state differs from the source cannot be
		// relabeled as a dump, even with a correct checksum and zero amounts.
		h.Header.Root[0] ^= 1
		var err error
		h.HeaderRLP, err = rlp.EncodeToBytes(h.Header)
		if err != nil {
			t.Fatal(err)
		}
		summary.SourceRoot = h.Header.Root
		if _, err := Verify(t.Context(), bytes.NewReader(uncheckedFile(t, h, records, summary))); err == nil {
			t.Fatal("reader accepted changed state labeled as dump")
		}
	}
}
