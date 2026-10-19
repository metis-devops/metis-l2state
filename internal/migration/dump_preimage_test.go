package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

// Trap namespace scans and count exact point reads. Account workers share this
// wrapper, so the instrumentation itself must be race-safe.
type dumpPreimageDB struct {
	ethdb.Database
	mu     sync.Mutex
	reads  map[common.Hash]int
	fault  error
	cancel context.CancelFunc
}

func (db *dumpPreimageDB) Get(key []byte) ([]byte, error) {
	if !bytes.HasPrefix(key, []byte(preimageKeyPrefix)) {
		return db.Database.Get(key)
	}
	if len(key) != len(preimageKeyPrefix)+common.HashLength {
		return nil, errors.New("invalid preimage point-read key")
	}
	db.mu.Lock()
	db.reads[common.BytesToHash(key[len(preimageKeyPrefix):])]++
	db.mu.Unlock()
	if db.cancel != nil {
		db.cancel()
	}
	if db.fault != nil {
		return nil, db.fault
	}
	return db.Database.Get(key)
}

func (*dumpPreimageDB) NewIterator([]byte, []byte) ethdb.Iterator {
	panic("dump scanned a database namespace")
}

func TestDumpReadsEachAccountPreimageOnce(t *testing.T) {
	for _, workers := range []int{2, 16} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			f, accounts := newDumpFixture(t, 1025)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			db := &dumpPreimageDB{Database: s.db, reads: make(map[common.Hash]int)}
			s.db = db
			l := make(limiter, workers)
			var out bytes.Buffer
			w, err := statefile.NewWriter(t.Context(), &out, s.header, statefile.Metadata{ToolVersion: "test", Genesis: f.genesis.Hash()}, "none")
			must(t, err)
			defer w.Abort()
			walk, err := s.walkWithPolicy(t.Context(), workers, l, nil, nil, nil, w.WriteRecord, rawSource)
			must(t, err)
			must(t, s.writeCodes(t.Context(), nil, &walk, w))
			must(t, w.Close(statefile.Summary{
				SourceRoot: s.head.StateRoot, ConvertedStateRoot: walk.convertedRoot, FinalStateRoot: walk.finalRoot,
				Counts: walk.counts, ConvertedCounts: walk.convertedCounts,
				Conversion: statefile.Conversion{OriginalSupply: "0", BalanceSum: "0", MigratedNative: "0", RemainingSupply: "0", SelfBalance: "0"},
			}))
			_, err = statefile.Verify(t.Context(), bytes.NewReader(out.Bytes()))
			must(t, err)
			if len(db.reads) != len(accounts) || s.memory != nil || len(l) != 0 {
				t.Fatal("unexpected address retention, reads or worker permits")
			}
			for address := range accounts {
				if db.reads[crypto.Keccak256Hash(address[:])] != 1 {
					t.Fatal("account preimage not fetched exactly once")
				}
			}
		})
	}
}

func TestDumpIgnoresUnreferencedPreimages(t *testing.T) {
	for _, slots := range []int{-1, 1025} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			f, _ := newDumpFixture(t, slots)
			first := dumpOptions(f, filepath.Join(t.TempDir(), "before"))
			_, err := Dump(t.Context(), first)
			must(t, err)
			mutate(t, f, func(db *leveldb.DB) {
				// Neither malformed namespace keys nor unrelated corrupt values
				// can affect exporting the selected account trie.
				must(t, db.Put([]byte(preimageKeyPrefix+"short"), []byte{1}, nil))
				for n := range 1000 {
					h := crypto.Keccak256Hash(fmt.Appendf(nil, "unused-%d", n))
					must(t, db.Put(append([]byte(preimageKeyPrefix), h[:]...), []byte("corrupt unrelated preimage"), nil))
				}
			})
			before := filesDigest(t, f.source)
			second := dumpOptions(f, filepath.Join(t.TempDir(), "after"))
			_, err = Dump(t.Context(), second)
			must(t, err)
			if filesDigest(t, f.source) != before {
				t.Fatal("dump changed source")
			}
			a, err := os.ReadFile(first.Output)
			must(t, err)
			b, err := os.ReadFile(second.Output)
			must(t, err)
			if !bytes.Equal(a, b) {
				t.Fatal("unreferenced preimages changed dump bytes")
			}
			readArtifact(t, second.Output)
		})
	}
}

func TestDumpPreimageReadFailureAndCancellation(t *testing.T) {
	f, _ := newDumpFixture(t, 1025)
	for _, mode := range []string{"error", "cancel-before", "cancel-during"} {
		t.Run(mode, func(t *testing.T) {
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			db := &dumpPreimageDB{Database: s.db, reads: make(map[common.Hash]int)}
			s.db = db
			want := context.Canceled
			switch mode {
			case "error":
				want = errors.New("preimage read failed")
				db.fault = want
			case "cancel-before":
				cancel()
			case "cancel-during":
				db.cancel = cancel
			}
			l := make(limiter, 2)
			_, err = s.walkWithPolicy(ctx, 2, l, nil, nil, nil, func(statefile.Record) error { return ctx.Err() }, rawSource)
			if !errors.Is(err, want) || len(l) != 0 {
				t.Fatalf("lost preimage failure or leaked workers: %v", err)
			}
			if mode == "cancel-before" && len(db.reads) != 0 {
				t.Fatal("canceled dump read preimages")
			}
		})
	}
}

// Production dump reads addresses through the pinned handle, while trie/code
// reads still use the ordinary DB. A failing ordinary preimage Get detects a
// fallback or second lookup. Release must precede closing the owning DB.
func TestDumpSnapshotReadPathAndLifetime(t *testing.T) {
	for _, mode := range []string{"success", "released", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := newDumpFixture(t, 1025)
			before := filesDigest(t, f.source)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			closed := false
			defer func() {
				if !closed {
					must(t, s.close())
				}
			}()
			s.preimages, err = s.kv.Snapshot()
			must(t, err)
			snapshot := s.preimages
			db := &dumpPreimageDB{Database: s.db, reads: make(map[common.Hash]int), fault: errors.New("unexpected ordinary preimage lookup")}
			s.db = db
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var want error
			switch mode {
			case "released":
				snapshot.Release()
				want = leveldb.ErrSnapshotReleased
			case "cancel":
				cancel()
				want = context.Canceled
			}
			l := make(limiter, 16)
			_, err = s.walkWithPolicy(ctx, 16, l, nil, nil, nil, func(statefile.Record) error { return nil }, rawSource)
			if !errors.Is(err, want) || len(l) != 0 || len(db.reads) != 0 {
				t.Fatalf("snapshot read failed or leaked workers: %v, permits=%d, fallback reads=%d", err, len(l), len(db.reads))
			}
			must(t, s.close())
			closed = true
			if s.preimages != nil {
				t.Fatal("source retained snapshot")
			}
			if _, err := snapshot.Get([]byte("key")); !errors.Is(err, leveldb.ErrSnapshotReleased) {
				t.Fatalf("snapshot not released before source close: %v", err)
			}
			if filesDigest(t, f.source) != before {
				t.Fatal("snapshot path changed source")
			}
		})
	}
}
