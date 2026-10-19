package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

func TestSharedAndOverlaidInventories(t *testing.T) {
	f := newFixture(t, 3, 0)
	baseline, err := Migrate(t.Context(), f.options(filepath.Join(t.TempDir(), "base")))
	must(t, err)
	if baseline.Summary.ConvertedStateRoot != baseline.Summary.FinalStateRoot || baseline.Summary.ConvertedCounts != baseline.Summary.Counts {
		t.Fatal("no-alloc inventories diverged")
	}
	cases := []struct {
		name, body string
		changed    bool
	}{
		{"empty-map", `{}`, false},
		{"empty-account", fmt.Sprintf(`{"%s":{},"0x0000000000000000000000000000000000000098":{"storage":{}}}`, ordinary.Hex()), false},
		{"replace-clear-share", fmt.Sprintf(`{"%s":{"code":"0x"},"%s":{"code":"0x600100"},"0x0000000000000000000000000000000000000099":{"code":"0x60026000"},"0x0000000000000000000000000000000000000098":{"balance":"0x0","storage":{"0x01":"0x00"}}}`, ordinary.Hex(), manual.Hex()), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			o := f.options(filepath.Join(dir, "out"))
			o.Alloc = filepath.Join(dir, "alloc.json")
			must(t, os.WriteFile(o.Alloc, []byte(tc.body), 0600))
			result, err := Migrate(t.Context(), o)
			must(t, err)
			accounts, _, summary := readArtifact(t, o.Output)
			if result.Summary != summary || summary.ConvertedStateRoot != baseline.Summary.ConvertedStateRoot || summary.ConvertedCounts != baseline.Summary.ConvertedCounts {
				t.Fatal("alloc changed pre-alloc root or inventory")
			}
			if !tc.changed {
				if summary != baseline.Summary {
					t.Fatal("no-op alloc changed summary")
				}
				return
			}
			if summary.FinalStateRoot == summary.ConvertedStateRoot || summary.Counts.Codes != 3 || summary.Counts.CodeBytes != 10 || summary.ConvertedCounts.Codes != 4 || summary.ConvertedCounts.CodeBytes != 12 {
				t.Fatalf("wrong split inventories: %+v", summary)
			}
			if _, ok := accounts[common.HexToAddress("0x98")]; !ok {
				t.Fatal("explicit zero account lost")
			}
		})
	}
}

// The validation-only path must still visit storage and code, and join cancelled
// producers. These failures happen before any conversion or alloc is applied.
func TestSourceValidationDamageAndCancellation(t *testing.T) {
	for _, damage := range []string{"code", "storage", "cancel"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t, 1025, 0)
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
				case "storage":
					must(t, db.Delete(a.Root[:], nil))
				}
			})
			s, err = openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if damage == "cancel" {
				cancel()
			}
			l := make(limiter, 2)
			_, err = s.walk(ctx, 2, l, nil, nil, nil, nil)
			if err == nil {
				t.Fatal("validation accepted damaged/cancelled source")
			}
			if damage == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if len(l) != 0 {
				t.Fatal("validation leaked worker permits")
			}
		})
	}
}

type codeReadCounter struct {
	ethdb.Database
	reads map[common.Hash]int
	mu    sync.Mutex
}

func (d *codeReadCounter) Get(key []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(key) == common.HashLength {
		h := common.Hash(key)
		if _, ok := d.reads[h]; ok {
			d.reads[h]++
		}
	}
	return d.Database.Get(key)
}

func TestCodeUnionResolvesOnce(t *testing.T) {
	for _, overlay := range []bool{false, true} {
		t.Run(fmt.Sprint(overlay), func(t *testing.T) { testCodeUnionResolvesOnce(t, overlay) })
	}
}

func testCodeUnionResolvesOnce(t *testing.T, overlay bool) {
	t.Helper()
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	in, err := loadInputs(t.Context(), f.options("unused"))
	must(t, err)
	if overlay {
		replacement, empty := []byte{0x60, 0x99, 0}, []byte{}
		in.alloc = map[common.Hash]*accountPatch{
			crypto.Keccak256Hash(ordinary[:]): {Address: ordinary, Code: &replacement},
			crypto.Keccak256Hash(manual[:]):   {Address: manual, Code: &empty},
		}
	}
	l := make(limiter, 2)
	reads := make(map[common.Hash]int)
	r := accountReader{s: s}
	for _, address := range []common.Address{ovmAddress, ordinary, manual, automatic} {
		a, err := r.get(crypto.Keccak256Hash(address[:]))
		must(t, err)
		reads[common.BytesToHash(a.CodeHash)] = 0
	}
	s.db = &codeReadCounter{Database: s.db, reads: reads}
	x, err := s.prepare(t.Context(), 2, l, f.validatedEvents(t))
	must(t, err)
	for h, n := range reads {
		if n != 1 {
			t.Fatalf("preflight code %s read %d times, want 1", h, n)
		}
		reads[h] = 0
	}
	x.metadata.ToolVersion = "test"
	patches, evidence, err := s.convertLegacy(t.Context(), x, in, 2, l)
	must(t, err)
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, x.metadata, "none")
	must(t, err)
	defer w.Abort()
	walk, err := s.walk(t.Context(), 2, l, x, patches, in.alloc, w.WriteRecord)
	must(t, err)
	for h, n := range reads {
		if n != 0 {
			t.Fatalf("validated output reread source code %s %d times", h, n)
		}
	}
	clear(reads)
	must(t, walk.codes.Visit(func(h common.Hash, _ codeUse) error {
		reads[h] = 0
		return nil
	}))
	must(t, s.writeCodes(t.Context(), in, &walk, w))
	overrides := codeOverrides(in)
	for h, n := range reads {
		want := 1
		if walk.codes.items.at(walk.codes.ids[h]).use&finalCode == 0 {
			want = 0
		}
		if _, ok := overrides[h]; ok {
			want = 0
		}
		if n != want {
			t.Fatalf("code %s read %d times, want %d", h, n, want)
		}
	}
	summary := statefile.Summary{SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot, Counts: walk.counts, ConvertedCounts: walk.convertedCounts, Conversion: evidence}
	must(t, w.Close(summary))
	got, err := statefile.Verify(t.Context(), bytes.NewReader(out.Bytes()))
	must(t, err)
	if got != summary {
		t.Fatal("union output changed summary")
	}
}

func TestWriteCodesStopsBeforeCountingFailure(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	_, err = s.prepare(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	for _, failure := range []string{"cancel", "converted-only-missing", "writer"} {
		t.Run(failure, func(t *testing.T) {
			var walk walkResult
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var w *statefile.Writer
			switch failure {
			case "cancel":
				walk.codes.Add(f.ovmCode, convertedCode|finalCode)
				cancel()
			case "converted-only-missing":
				walk.codes.Add(common.HexToHash("0xbad"), convertedCode)
			case "writer":
				walk.codes.Add(f.ovmCode, convertedCode|finalCode)
				w, err = statefile.NewWriter(ctx, new(bytes.Buffer), s.header, statefile.Metadata{ToolVersion: "test", Genesis: f.genesis.Hash()}, "none")
				must(t, err)
				defer w.Abort()
				// Closing without the state fails validation and closes the writer.
				if err := w.Close(statefile.Summary{}); err == nil {
					t.Fatal("accepted empty summary")
				}
			}
			err := s.writeCodes(ctx, nil, &walk, w)
			if err == nil || (failure == "cancel" && !errors.Is(err, context.Canceled)) {
				t.Fatalf("lost %s failure: %v", failure, err)
			}
			if walk.counts.Codes != 0 || walk.counts.CodeBytes != 0 || walk.convertedCounts.Codes != 0 || walk.convertedCounts.CodeBytes != 0 {
				t.Fatal("counted failed code resolution or write")
			}
		})
	}
}

func TestRepeatedAddressEvidenceRemainsStrict(t *testing.T) {
	x := newIndexes()
	h := crypto.Keccak256Hash(eoa[:])
	key := append([]byte(preimageKeyPrefix), h[:]...)
	must(t, x.preimage(key, eoa[:]))
	must(t, x.addAddress(eoa))
	a := ovmevents.Allowance{Owner: eoa, Spender: automatic}
	must(t, x.addAllowance(a))
	must(t, x.addAllowance(a))
	if len(x.addresses) != 2 || len(x.balances) != 2 || x.balances[balanceKey(eoa)] != eoa {
		t.Fatal("duplicate evidence changed identity maps")
	}
	if err := x.preimage(key, ordinary[:]); err == nil {
		t.Fatal("cached address bypassed preimage validation")
	}
	a.Owner = manual
	must(t, x.addAllowance(a))
	if _, ok := x.allowances[allowanceKey(manual, automatic)]; !ok || len(x.allowances) != 2 {
		t.Fatal("changed owner did not derive a distinct allowance key")
	}
	if err := x.addKnownAddress(h, ordinary); err == nil || !strings.Contains(err.Error(), "ambiguous account") {
		t.Fatal("lost address collision check")
	}
	y := newIndexes()
	y.balances[balanceKey(eoa)] = ordinary
	if err := y.addAddress(eoa); err == nil {
		t.Fatal("lost balance ownership check")
	}
	if len(y.addresses) != 0 {
		t.Fatal("failed insertion published a partial identity")
	}
}
