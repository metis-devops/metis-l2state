package migration

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2state/internal/version"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

// Keep the old two-pass algorithm as a test oracle, with independent Reader
// verification. Production dump must produce exactly the same public bytes.
func TestDumpSinglePassMatchesTwoPass(t *testing.T) {
	for _, slots := range []int{-1, 0, 1024, 1025} {
		for _, codec := range []string{"none", "zstd"} {
			t.Run(fmt.Sprintf("%d/%s", slots, codec), func(t *testing.T) {
				f, _ := newDumpFixture(t, slots)
				want := twoPassDump(t, f, codec)
				for _, workers := range []int{2, 16} {
					o := dumpOptions(f, filepath.Join(t.TempDir(), "dump"))
					o.Compression, o.Workers = codec, workers
					_, err := Dump(t.Context(), o)
					must(t, err)
					got, err := os.ReadFile(o.Output)
					must(t, err)
					if !bytes.Equal(got, want) {
						t.Fatal("single-pass dump changed artifact bytes")
					}
					readArtifact(t, o.Output)
				}
			})
		}
	}
}

func twoPassDump(t *testing.T, f fixture, codec string) []byte {
	t.Helper()
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	x := &indexes{}
	_, err = s.walkWithPolicy(t.Context(), 2, l, x, nil, nil, nil, rawSource)
	must(t, err)
	x.validation = &sourceValidation{source: s, head: s.head, policy: rawSource}
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, statefile.Metadata{ToolVersion: version.ToolVersion, Genesis: f.genesis.Hash()}, codec)
	must(t, err)
	defer w.Abort()
	walk, err := s.walkWithPolicy(t.Context(), 2, l, x, nil, nil, w.WriteRecord, rawSource)
	must(t, err)
	must(t, s.writeCodes(t.Context(), nil, &walk, w))
	summary := statefile.Summary{
		SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot,
		Counts: walk.counts, ConvertedCounts: walk.convertedCounts,
		Conversion: statefile.Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"},
	}
	must(t, w.Close(summary))
	verified, err := statefile.Verify(t.Context(), bytes.NewReader(out.Bytes()))
	must(t, err)
	if verified != summary {
		t.Fatal("two-pass oracle failed independent verification")
	}
	return out.Bytes()
}

func TestDumpSinglePassWriterRejectsChangedStorage(t *testing.T) {
	f, _ := newDumpFixture(t, 1025)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	x := &indexes{}
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, statefile.Metadata{ToolVersion: version.ToolVersion, Genesis: f.genesis.Hash()}, "none")
	must(t, err)
	defer w.Abort()
	changed := false
	_, err = s.walkWithPolicy(t.Context(), 2, l, x, nil, nil, func(r statefile.Record) error {
		if r.Kind == statefile.StorageEntry && !changed {
			r.Storage.Value[0] ^= 1
			changed = true
		}
		return w.WriteRecord(r)
	}, rawSource)
	if err == nil || !changed || len(l) != 0 {
		t.Fatalf("changed output escaped Writer validation or leaked workers: %v", err)
	}
}

// Validly encoded but hash-mismatched trie nodes must still be caught by the
// source accumulators now that output starts before source validation finishes.
func TestDumpRejectsRootMismatchDuringOutput(t *testing.T) {
	for _, damage := range []string{"storage", "state", "storage-value"} {
		t.Run(damage, func(t *testing.T) {
			f, accounts := newDumpFixture(t, 3)
			mutate(t, f, func(db *leveldb.DB) {
				sink := func(_ []byte, h common.Hash, b []byte) { must(t, db.Put(h[:], b, nil)) }
				stack := trie.NewStackTrie(sink)
				old := accounts[ordinary].Root
				if damage == "state" {
					old = f.header.Root
					accounts[ordinary].Nonce++
					leaves := make(map[common.Hash][]byte)
					for a, account := range accounts {
						b, err := rlp.EncodeToBytes(account)
						must(t, err)
						leaves[crypto.Keccak256Hash(a[:])] = b
					}
					for _, h := range sortedHashes(leaves) {
						must(t, stack.Update(h[:], leaves[h]))
					}
				} else {
					for _, k := range sortedHashes(f.storage) {
						v := f.storage[k]
						v[0] ^= 1
						raw := slotRLP(v)
						if damage == "storage-value" {
							raw = []byte{0} // Invalid zero source storage leaf.
						}
						must(t, stack.Update(k[:], raw))
					}
				}
				h := stack.Hash()
				raw, err := db.Get(h[:], nil)
				must(t, err)
				must(t, db.Put(old[:], raw, nil))
			})
			before := filesDigest(t, f.source)
			dir := t.TempDir()
			o := dumpOptions(f, filepath.Join(dir, "dump"))
			started := false
			o.Progress = progressHook(func(p []byte) {
				if !bytes.Contains(p, []byte("phase=write_state")) {
					return
				}
				files, err := filepath.Glob(filepath.Join(dir, ".dump.partial-*"))
				must(t, err)
				if len(files) != 1 {
					t.Fatal("missing unpublished output")
				}
				info, err := os.Stat(files[0])
				must(t, err)
				started = info.Size() > 0
			})
			_, err := Dump(t.Context(), o)
			want := "source " + damage + " root mismatch"
			if damage == "storage-value" {
				want = "noncanonical source storage value"
			}
			if err == nil || !strings.Contains(err.Error(), want) || !started {
				t.Fatalf("lost streaming source validation: %v, started=%v", err, started)
			}
			assertEmptyDirectory(t, dir)
			if before != filesDigest(t, f.source) {
				t.Fatal("failed dump changed source")
			}
		})
	}
}
