package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/metis-devops/metis-l2state/statefile"
	"github.com/syndtr/goleveldb/leveldb"
)

func artifactCodes(t *testing.T, raw []byte) []common.Hash {
	t.Helper()
	r, err := statefile.NewReader(t.Context(), bytes.NewReader(raw))
	must(t, err)
	defer r.Close()
	var hashes []common.Hash
	for {
		record, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		must(t, err)
		if record.Kind == statefile.CodeEntry {
			hashes = append(hashes, record.CodeHash)
		}
	}
	_, err = r.Result()
	must(t, err)
	return hashes
}

func TestCodeCacheModesAndAppendOrder(t *testing.T) {
	f := newFixture(t, 1025, 0)
	dir := t.TempDir()
	// A cleared source code is still used by a new account; two alloc accounts
	// share new code. Their JSON order differs from keccak(address) order.
	alloc := filepath.Join(dir, "alloc.json")
	must(t, os.WriteFile(alloc, fmt.Appendf(nil, `{
 "%s":{"code":"0x"}, "%s":{"code":"0x"},
 "0x0000000000000000000000000000000000000099":{"code":"0x604400"},
 "0x0000000000000000000000000000000000000098":{"code":"0x604500"},
 "0x0000000000000000000000000000000000000097":{"code":"0x604400"},
 "0x0000000000000000000000000000000000000096":{"code":"0x60%02x00"}
 }`, ordinary, manual, ordinary[19]), 0600))
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	var original []common.Hash
	must(t, s.eachAccount(t.Context(), make(limiter, 2), walkPlan{}, func(_ common.Hash, a *types.StateAccount) error {
		original = append(original, common.BytesToHash(a.CodeHash))
		return nil
	}))
	must(t, s.close())
	o := f.options("unused")
	o.Alloc = alloc
	in, err := loadInputs(t.Context(), o)
	must(t, err)
	removed := crypto.Keccak256Hash([]byte{0x60, manual[19], 0})
	want := slices.DeleteFunc(slices.Clone(original), func(h common.Hash) bool { return h == f.ovmCode || h == removed })
	want = append(want, crypto.Keccak256Hash(in.runtime))
	for _, key := range sortedHashes(in.alloc) {
		p := in.alloc[key]
		if len(*p.Code) == 0 {
			continue
		}
		h := crypto.Keccak256Hash(*p.Code)
		if !slices.Contains(want, h) {
			want = append(want, h)
		}
	}
	before := filesDigest(t, f.source)
	for _, command := range []string{"migrate", "dump"} {
		for _, codec := range []string{"none", "zstd"} {
			var first []byte
			for _, cache := range []string{"", "memory", "none"} {
				for _, workers := range []int{2, 16} {
					output := filepath.Join(t.TempDir(), "state")
					var result Result
					if command == "migrate" {
						options := f.options(output)
						options.Alloc = alloc
						options.CodeCache, options.Compression, options.Workers = cache, codec, workers
						result, err = Migrate(t.Context(), options)
					} else {
						options := dumpOptions(f, output)
						options.CodeCache, options.Compression, options.Workers = cache, codec, workers
						result, err = Dump(t.Context(), options)
					}
					must(t, err)
					raw, err := os.ReadFile(output)
					must(t, err)
					verified, err := statefile.Verify(t.Context(), bytes.NewReader(raw))
					must(t, err)
					if result.Summary != verified {
						t.Fatal("summary differs")
					}
					if first == nil {
						first = raw
					} else if !bytes.Equal(first, raw) {
						t.Fatalf("cache/worker nondeterminism: %s/%s/%s/%d", command, codec, cache, workers)
					}
					expected := want
					if command == "dump" {
						expected = original
					}
					if !slices.Equal(artifactCodes(t, raw), expected) {
						t.Fatalf("wrong %s code order", command)
					}
				}
			}
		}
	}
	if before != filesDigest(t, f.source) {
		t.Fatal("source changed")
	}
}

type observedCodeDB struct {
	ethdb.Database
	mu                            sync.Mutex
	reads                         map[common.Hash]int
	blocked, storage              common.Hash
	entered, storageRead, release chan struct{}
	once, storageOnce             sync.Once
	ctx                           context.Context
	failure                       error
}

func (d *observedCodeDB) Get(key []byte) ([]byte, error) {
	if len(key) == 32 {
		h := common.Hash(key)
		d.mu.Lock()
		_, code := d.reads[h]
		if code {
			d.reads[h]++
		}
		d.mu.Unlock()
		if h == d.storage && d.storageRead != nil {
			d.storageOnce.Do(func() { close(d.storageRead) })
		}
		if h == d.blocked && d.entered != nil {
			d.once.Do(func() { close(d.entered) })
			select {
			case <-d.release:
			case <-d.ctx.Done():
				return nil, d.ctx.Err()
			}
		}
		if code && d.failure != nil {
			return nil, d.failure
		}
	}
	return d.Database.Get(key)
}

func TestCodeReadOverlapsSameAccountStorage(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRun), func(t *testing.T) {
			f, accounts := newDumpFixture(t, 1025)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			a := accounts[ordinary]
			h := common.BytesToHash(a.CodeHash)
			db := &observedCodeDB{Database: s.db, reads: map[common.Hash]int{h: 0}, blocked: h, storage: a.Root,
				entered: make(chan struct{}), storageRead: make(chan struct{}), release: make(chan struct{}), ctx: ctx}
			s.db = db
			must(t, s.nodes.Close())
			s.nodes = triedb.NewDatabase(s.db, triedb.HashDefaults)
			l := make(limiter, 2)
			done := make(chan error, 1)
			go func() {
				_, err := s.walkWithPolicy(ctx, 2, l, nil, nil, nil, func(statefile.Record) error { return nil }, rawSource)
				done <- err
			}()
			defer func() { cancel(); <-done }()
			select {
			case <-db.entered:
			case <-ctx.Done():
				t.Fatal("code read not started")
			}
			select {
			case <-db.storageRead:
			case <-ctx.Done():
				t.Fatal("storage waited for code")
			}
			// The shared hash has one in-flight read, even across account workers.
			db.mu.Lock()
			count := db.reads[h]
			db.mu.Unlock()
			if count != 1 {
				t.Fatal("duplicate in-flight code read")
			}
			if cancelRun {
				cancel()
			} else {
				close(db.release)
			}
			err = <-done
			done <- err // allow deferred join on all assertion paths
			if cancelRun {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else {
				must(t, err)
			}
			if len(l) != 0 {
				t.Fatal("leaked permits")
			}
		})
	}
}

func TestCodeCacheReadCountsAndRereadErrors(t *testing.T) {
	for _, cache := range []string{"memory", "none"} {
		t.Run(cache, func(t *testing.T) {
			f := newFixture(t, 3, 0)
			s, err := openSource(f.source, 16, 16)
			must(t, err)
			defer func() { must(t, s.close()) }()
			s.codeCache = cache
			db := &observedCodeDB{Database: s.db, reads: make(map[common.Hash]int)}
			for _, a := range []common.Address{ovmAddress, ordinary, automatic, manual} {
				db.reads[crypto.Keccak256Hash([]byte{0x60, a[19], 0})] = 0
			}
			s.db = db
			in, err := loadInputs(t.Context(), f.options("unused"))
			must(t, err)
			x, err := s.prepareSnapshot(t.Context(), 2, make(limiter, 2), in.events)
			must(t, err)
			for _, n := range db.reads {
				if n != 1 {
					t.Fatal("source code validation not deduplicated")
				}
			}
			if (len(s.codes.blobs) > 0) != (cache == "memory") {
				t.Fatal("wrong code retention")
			}
			evidence, err := s.convert(t.Context(), x, in, 2, make(limiter, 2))
			var patches map[common.Hash]*accountPatch
			must(t, err)
			x.metadata.ToolVersion = "test"
			encodeWalk(t, s, x, in, patches, evidence)
			for h, n := range db.reads {
				want := 1
				if cache == "none" && h != f.ovmCode && h != crypto.Keccak256Hash(in.runtime) {
					want++
				}
				if n != want {
					t.Fatalf("%s read %d times, want %d", h, n, want)
				}
			}
			// Output consumes its state. Build a fresh validated state for the
			// separate final-code reread failure, never replay consumed memory.
			s.memory = nil
			x, err = s.prepareSnapshot(t.Context(), 2, make(limiter, 2), in.events)
			must(t, err)
			_, err = s.convert(t.Context(), x, in, 2, make(limiter, 2))
			must(t, err)
			x.metadata.ToolVersion = "test"
			sentinel := errors.New("code read failure after validation")
			db.failure = sentinel
			var out bytes.Buffer
			w, err := statefile.NewWriter(t.Context(), &out, s.header, x.metadata, "none")
			must(t, err)
			defer w.Abort()
			walk, err := s.writeMigration(t.Context(), 2, make(limiter, 2), x, nil, w.WriteRecord)
			must(t, err)
			err = s.writeCodes(t.Context(), in, &walk, w)
			if cache == "none" {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
			} else {
				must(t, err)
			}
		})
	}
}

func TestCodeStoreCompletionOrder(t *testing.T) {
	for _, cache := range []bool{false, true} {
		m := newCodeStore(cache)
		first, second := []byte{1}, bytes.Repeat([]byte{2}, snapshotBlockBytes+3)
		h1, h2 := crypto.Keccak256Hash(first), crypto.Keccak256Hash(second)
		id1, id2 := m.reserve(h1), m.reserve(h2)
		m.complete(id2, second)
		m.complete(id1, first)
		if id1 != 1 || id2 != 2 || m.codes.at(0).hash != h1 || m.codes.at(1).hash != h2 {
			t.Fatal("completion reordered IDs")
		}
		if m.codes.at(0).length != 1 || m.codes.at(1).length != uint64(len(second)) {
			t.Fatal("lost code length")
		}
		if cache {
			got, err := m.code(h1)
			must(t, err)
			if !bytes.Equal(got, first) {
				t.Fatal("wrong first bytes")
			}
			got, err = m.code(h2)
			must(t, err)
			if !bytes.Equal(got, second) {
				t.Fatal("wrong second bytes")
			}
		} else if len(m.blobs) != 0 || m.blobLength != 0 {
			t.Fatal("none retained bytecode")
		}
	}
}

func TestBothCachesRejectSourceCodeReplacedByAlloc(t *testing.T) {
	f := newFixture(t, 0, 0)
	mutate(t, f, func(db *leveldb.DB) {
		h := crypto.Keccak256Hash([]byte{0x60, ordinary[19], 0})
		must(t, db.Delete(h[:], nil))
	})
	for _, cache := range []string{"memory", "none"} {
		directory := t.TempDir()
		o := f.options(filepath.Join(directory, "state"))
		o.CodeCache = cache
		o.Alloc = filepath.Join(directory, "alloc.json")
		must(t, os.WriteFile(o.Alloc, fmt.Appendf(nil, `{"%s":{"code":"0x"}}`, ordinary), 0600))
		if _, err := Migrate(t.Context(), o); err == nil {
			t.Fatal("alloc bypassed source code validation")
		}
		if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("failed source published output")
		}
		entries, err := os.ReadDir(directory)
		must(t, err)
		if len(entries) != 1 {
			t.Fatal("failed source left temporary output")
		}
	}
}

func TestCodeLoaderFullQueueCancellation(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	defer func() { must(t, db.Close()) }()
	var hashes []common.Hash
	for i := range 6 {
		code := []byte{byte(i + 1)}
		h := crypto.Keccak256Hash(code)
		must(t, db.Put(h[:], code))
		hashes = append(hashes, h)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	observed := &observedCodeDB{Database: db, ctx: ctx, blocked: hashes[0], entered: make(chan struct{}), release: make(chan struct{})}
	s := &source{db: observed}
	l := make(limiter, 2)
	store := newCodeStore(false)
	failure := &runFailure{cancel: cancel}
	var jobs sync.WaitGroup
	loader := newCodeLoader(ctx, s, store, l, failure, &jobs)
	defer jobs.Wait()
	defer close(loader.queue)
	defer cancel()
	first, err := loader.submit(ctx, hashes[0])
	must(t, err)
	select {
	case <-observed.entered:
	case <-ctx.Done():
		t.Fatal("code worker not started")
	}
	for _, h := range hashes[1:5] {
		_, err := loader.submit(ctx, h)
		must(t, err)
	}
	if len(loader.queue) != 4 || cap(loader.queue) != 4 {
		t.Fatal("code queue bound changed")
	}
	// One code reader cannot occupy both global permits while blocked in DB I/O.
	select {
	case l <- struct{}{}:
		<-l
	case <-ctx.Done():
		t.Fatal("code pool starved storage")
	}
	submitted := make(chan error, 1)
	go func() {
		load, err := loader.submit(ctx, hashes[5])
		if err == nil {
			err = load.wait(ctx)
		}
		submitted <- err
	}()
	cancel()
	if err := <-submitted; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := first.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
