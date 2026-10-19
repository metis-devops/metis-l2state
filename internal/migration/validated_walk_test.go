package migration

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

// Compare root reuse with the fully recomputing path and an independent reader.
// Include empty storage, the partition threshold, inserted accounts, and deletion
// of every slot: a nil accumulator must not confuse a reused root with an empty one.
func TestValidatedWalkParity(t *testing.T) {
	for _, slots := range []int{0, 3, 1025} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			f := newFixture(t, slots, 0)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			in, err := loadInputs(t.Context(), f.options("unused"))
			must(t, err)
			l := make(limiter, 2)
			x, err := s.prepare(t.Context(), 2, l, in.events)
			must(t, err)
			x.metadata.ToolVersion = "test"
			patches, evidence, err := s.convertLegacy(t.Context(), x, in, 2, l)
			must(t, err)
			h := crypto.Keccak256Hash(ordinary[:])
			deleted := make(map[common.Hash]common.Hash)
			for k := range f.storage {
				deleted[k] = common.Hash{}
			}
			created := common.HexToAddress("0x99")
			createdHash := crypto.Keccak256Hash(created[:])
			cases := []struct {
				name  string
				alloc map[common.Hash]*accountPatch
			}{
				{"none", nil},
				{"noop", map[common.Hash]*accountPatch{h: {Address: ordinary}}},
				{"delete-all", map[common.Hash]*accountPatch{h: {Address: ordinary, Storage: deleted}}},
				{"insert", map[common.Hash]*accountPatch{
					h:           {Address: ordinary, Storage: map[common.Hash]common.Hash{common.HexToHash("0x11"): common.HexToHash("0x22")}},
					createdHash: {Address: created, Balance: new(uint256.Int), Storage: map[common.Hash]common.Hash{common.HexToHash("0x33"): common.HexToHash("0x44")}},
				}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					in.alloc = tc.alloc
					full := *x
					full.validation = nil
					want := encodeWalk(t, s, &full, in, patches, evidence)
					got := encodeWalk(t, s, x, in, patches, evidence)
					if !bytes.Equal(want, got) {
						t.Fatal("validated root reuse changed artifact bytes")
					}
				})
			}
		})
	}
}

func encodeWalk(t *testing.T, s *source, x *indexes, in *inputs, patches map[common.Hash]*accountPatch, evidence statefile.Conversion) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, x.metadata, "none")
	must(t, err)
	defer w.Abort()
	var walk walkResult
	if s.memory != nil {
		walk, err = s.writeMigration(t.Context(), 2, make(limiter, 2), x, in.alloc, w.WriteRecord)
	} else {
		walk, err = s.walk(t.Context(), 2, make(limiter, 2), x, patches, in.alloc, w.WriteRecord)
	}
	must(t, err)
	must(t, s.writeCodes(t.Context(), in, &walk, w))
	summary := statefile.Summary{SourceRoot: walk.sourceRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot, Counts: walk.counts, ConvertedCounts: walk.convertedCounts, Conversion: evidence}
	must(t, w.Close(summary))
	got, err := statefile.Verify(t.Context(), bytes.NewReader(out.Bytes()))
	must(t, err)
	if got != summary {
		t.Fatal("independent reader disagrees with validated output")
	}
	return out.Bytes()
}

func TestValidationBoundToSourceAndHead(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	x, err := s.prepare(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	other := *s
	for _, change := range []string{"source", "head", "empty"} {
		t.Run(change, func(t *testing.T) {
			bad := *x
			proof := *x.validation
			bad.validation = &proof
			switch change {
			case "source":
				proof.source = &other
			case "head":
				proof.head.StateRoot = common.HexToHash("0x01")
			case "empty":
				proof = sourceValidation{}
			}
			_, err := s.walk(t.Context(), 2, make(limiter, 2), &bad, nil, nil, func(statefile.Record) error {
				t.Fatal("invalid validation evidence reached output")
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), "source validation") {
				t.Fatalf("accepted unrelated validation evidence: %v", err)
			}
		})
	}
}

func TestWriterChecksReusedStorageRoot(t *testing.T) {
	f := newFixture(t, 3, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	x, err := s.prepare(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	x.metadata.ToolVersion = "test"
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, x.metadata, "none")
	must(t, err)
	defer w.Abort()
	var current common.Address
	changed := false
	_, err = s.walk(t.Context(), 2, make(limiter, 2), x, nil, nil, func(r statefile.Record) error {
		if r.Kind == statefile.AccountStart {
			current = r.Account.Address
		}
		if r.Kind == statefile.StorageEntry && current == ordinary && !changed {
			r.Storage.Value = common.HexToHash("0xffff")
			changed = true
		}
		return w.WriteRecord(r)
	})
	if !changed || err == nil || !strings.Contains(err.Error(), "storage root or count mismatch") {
		t.Fatalf("Writer did not reject corrupted records against reused root: %v", err)
	}
}

// Even code that conversion/alloc would replace must be validated in the source.
func TestPreflightDamageCannotBeHiddenByOverlay(t *testing.T) {
	for _, damage := range []string{"code", "ovm-code", "storage"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t, 3, 0)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			r := accountReader{s: s}
			a, err := r.get(crypto.Keccak256Hash(ordinary[:]))
			must(t, err)
			must(t, s.close())
			mutate(t, f, func(db *leveldb.DB) {
				switch damage {
				case "code":
					must(t, db.Delete(a.CodeHash, nil))
				case "ovm-code":
					must(t, db.Delete(f.ovmCode[:], nil))
				case "storage":
					must(t, db.Delete(a.Root[:], nil))
				}
			})
			dir := t.TempDir()
			o := f.options(filepath.Join(dir, "out"))
			o.Alloc = filepath.Join(t.TempDir(), "alloc.json")
			must(t, os.WriteFile(o.Alloc, fmt.Appendf(nil, `{"%s":{"code":"0x","storage":{"0x00":"0x00"}}}`, ordinary), 0600))
			var progress bytes.Buffer
			o.Progress = &progress
			_, err = Migrate(t.Context(), o)
			if err == nil || errors.Is(err, os.ErrExist) {
				t.Fatalf("accepted damaged preflight: %v", err)
			}
			if bytes.Contains(progress.Bytes(), []byte("phase=convert_ovm")) || bytes.Contains(progress.Bytes(), []byte("phase=write_state")) {
				t.Fatal("damaged source reached conversion or alloc")
			}
			entries, err := os.ReadDir(dir)
			must(t, err)
			if len(entries) != 0 {
				t.Fatal("preflight failure left output or partial")
			}
		})
	}
}
