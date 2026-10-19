package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
)

// A prepared migration must never touch the source again for state or code.
// Close still delegates to the real source and remains part of publication.
type snapshotNoReads struct{ ethdb.Database }

func (snapshotNoReads) Get([]byte) ([]byte, error) {
	return nil, errors.New("unexpected source read after snapshot")
}
func (snapshotNoReads) NewIterator([]byte, []byte) ethdb.Iterator {
	panic("unexpected source iterator after snapshot")
}

func TestSnapshotParityWithoutSourceReads(t *testing.T) {
	for _, slots := range []int{0, 1024, 1025, 16385} {
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
			empty := []byte{}
			overlays := []map[common.Hash]*accountPatch{
				nil,
				{h: {Address: ordinary}},
				{h: {Address: ordinary, Storage: deleted, Code: &empty}},
				{crypto.Keccak256Hash(created[:]): {Address: created, Balance: new(uint256.Int), Storage: map[common.Hash]common.Hash{common.HexToHash("0x11"): common.HexToHash("0x22")}}},
			}
			var want [][]byte
			for _, alloc := range overlays {
				in.alloc = alloc
				want = append(want, encodeWalk(t, s, x, in, patches, evidence))
			}
			for i, alloc := range overlays {
				func() {
					fresh, err := openSource(f.source, 16, 16)
					must(t, err)
					defer func() { must(t, fresh.close()) }()
					indexes, err := fresh.prepareSnapshot(t.Context(), 2, l, in.events)
					must(t, err)
					indexes.metadata.ToolVersion = "test"
					fresh.db = snapshotNoReads{fresh.db}
					nodes := fresh.nodes
					fresh.nodes = nil
					defer func() { fresh.nodes = nodes }()
					converted, err := fresh.convert(t.Context(), indexes, in, 2, l)
					must(t, err)
					in.alloc = alloc
					got := encodeWalk(t, fresh, indexes, in, nil, converted)
					if !bytes.Equal(got, want[i]) {
						t.Fatalf("snapshot changed bytes for overlay %d", i)
					}
					if fresh.memory.accounts.Len() != 0 || fresh.memory.phase != memoryConsumed {
						t.Fatal("state retained after output")
					}
				}()
			}
		})
	}
}

func TestSnapshotCodeBlocksAndOwnership(t *testing.T) {
	m := newStateSnapshot()
	// Cross code blob block boundaries while preserving deduplication and ownership.
	code := bytes.Repeat([]byte{0x60}, snapshotBlockBytes+17)
	h := crypto.Keccak256Hash(code)
	m.addCode(h, code)
	m.addCode(h, code)
	if m.codes.length != 1 || len(m.blobs) != 2 {
		t.Fatal("code not deduplicated or chunked")
	}
	code[0] = 0
	got, err := m.code(h)
	must(t, err)
	if crypto.Keccak256Hash(got) != h {
		t.Fatal("capture borrowed source bytes")
	}
	got[0] = 0
	got, err = m.code(h)
	must(t, err)
	if crypto.Keccak256Hash(got) != h {
		t.Fatal("consumer mutated frozen code")
	}

}

func TestSnapshotFailureAndCancellation(t *testing.T) {
	f := newFixture(t, 1025, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	bad := f.events(t)
	bad.Progress.BlockNumber++
	badPath := filepath.Join(t.TempDir(), "events.json")
	must(t, ovmevents.Save(badPath, bad))
	frozen, _, err := ovmevents.ReadValidatedFile(t.Context(), badPath)
	must(t, err)
	if _, err = s.prepareSnapshot(t.Context(), 2, make(limiter, 2), frozen); err == nil || s.memory != nil {
		t.Fatal("failed preparation installed snapshot")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.prepareSnapshot(ctx, 2, make(limiter, 2), f.validatedEvents(t)); !errors.Is(err, context.Canceled) || s.memory != nil {
		t.Fatal("canceled preparation installed snapshot")
	}
	x, err := s.prepareSnapshot(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	h := crypto.Keccak256Hash(ordinary[:])
	a, _ := s.memory.find(h)
	if err = s.memory.storage(ctx, h, a.storageRoot(), func(common.Hash, common.Hash) error { t.Fatal("canceled storage emitted"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = s.walk(ctx, 2, make(limiter, 2), x, nil, nil, func(statefile.Record) error { t.Fatal("canceled walk emitted"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = s.memory.storageValue(h, common.Hash{}, common.Hash{}); err == nil {
		t.Fatal("wrong storage root accepted")
	}
}

func TestSnapshotWriterStillValidates(t *testing.T) {
	f := newFixture(t, 3, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	x, err := s.prepareSnapshot(t.Context(), 2, make(limiter, 2), f.validatedEvents(t))
	must(t, err)
	x.metadata.ToolVersion = "test"
	h := crypto.Keccak256Hash(ordinary[:])
	a, _ := s.memory.find(h)
	// Simulate corrupted frozen memory while leaving the validated root intact.
	a.storage.Set(sortedHashes(treeMap(&a.storage))[0], common.HexToHash("0xffff"))
	var out bytes.Buffer
	w, err := statefile.NewWriter(t.Context(), &out, s.header, x.metadata, "none")
	must(t, err)
	defer w.Abort()
	if _, err = s.walk(t.Context(), 2, make(limiter, 2), x, nil, nil, w.WriteRecord); err == nil {
		t.Fatal("Writer accepted corrupted snapshot storage")
	}
}

func (m *codeStore) addCode(h common.Hash, code []byte) {
	m.codeMu.Lock()
	defer m.codeMu.Unlock()
	if _, ok := m.codeIDs[h]; ok {
		return
	}
	m.complete(m.reserve(h), code)
}
