package migration

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"reflect"
	"testing"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

func metadataChunk(slot common.Hash, n uint64) common.Hash {
	base := new(uint256.Int).SetBytes(crypto.Keccak256(slot[:]))
	key := base.Add(base, new(uint256.Int).SetUint64(n)).Bytes32()
	return crypto.Keccak256Hash(key[:])
}

func TestOVMStringExpansionBound(t *testing.T) {
	slot := common.HexToHash("0x03")
	if unsafe.Sizeof(metadataLayout{}) != 240 {
		t.Fatal("metadata layout no longer has fixed storage")
	}
	for _, length := range []uint64{32, 33, 1024, (1 << 20) - 1, 1 << 20, (1 << 20) + 1, 1 << 40, ^uint64(0)} {
		r, err := recognizeString(slot, stringWord(length))
		must(t, err)
		if r.chunks != (length-1)/32+1 {
			t.Fatal("incorrect chunk count")
		}
		m := metadataLayout{strings: [2]metadataString{r}}
		// Recognition of an actual early chunk does not expand the declared tail.
		matched, err := m.contains(t.Context(), metadataChunk(slot, 0))
		must(t, err)
		if !matched {
			t.Fatal("lost first chunk")
		}
	}
	// Original invalid domains remain invalid, independently of resource policy.
	for _, word := range []common.Hash{stringWord(31), common.BigToHash(new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 65), big.NewInt(1)))} {
		_, err := recognizeString(slot, word)
		legacyErr := legacyRecognizeString(t.Context(), slot, word, make(map[common.Hash]struct{}))
		if err == nil || legacyErr == nil {
			t.Fatal("accepted invalid original encoding")
		}
	}
}

func TestMetadataBoundaryDifferential(t *testing.T) {
	for _, length := range []uint64{0, 31, 32, 33, 2048, (1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/unknown=%v", length, invalid), func(t *testing.T) {
				f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
					for _, n := range []byte{3, 4} {
						var slot common.Hash
						slot[31] = n
						if length < 32 {
							if length != 0 {
								var word common.Hash
								word[31] = byte(length * 2)
								storage[crypto.Keccak256Hash(slot[:])] = word
							}
							if invalid {
								storage[common.HexToHash("0xff")] = common.HexToHash("0x01")
							}
							continue
						}
						storage[crypto.Keccak256Hash(slot[:])] = stringWord(length)
						chunks := (length-1)/32 + 1
						// Exercise both ends, partial final chunks and absent zero chunks.
						for i := uint64(0); i < min(chunks, 2); i++ {
							storage[metadataChunk(slot, i)] = common.HexToHash("0x01")
						}
						storage[metadataChunk(slot, chunks-1)] = common.HexToHash("0x02")
						if invalid {
							storage[metadataChunk(slot, chunks)] = common.HexToHash("0x03")
						}
					}
				})
				s, err := openSource(f.source, 16, 16)
				must(t, err)
				defer func() { must(t, s.close()) }()
				l := make(limiter, 2)
				x, err := s.prepareSnapshot(t.Context(), 2, l, f.validatedEvents(t))
				must(t, err)
				root := s.memory.get(ovmHash).Root
				old, err := s.legacyMetadataSlots(t.Context(), root)
				must(t, err)
				m, err := s.metadataSlots(t.Context(), root)
				must(t, err)
				want, oldErr := s.legacyBalanceJobs(t.Context(), x, root, old, l)
				got, newErr := s.balanceJobs(t.Context(), x, root, m, l)
				if (oldErr != nil) != invalid || (newErr != nil) != invalid || !reflect.DeepEqual(got, want) {
					t.Fatalf("classification changed: old=%v new=%v", oldErr, newErr)
				}
			})
		}
	}
}

func TestOversizedOVMMetadataMigratesAndDumpsUnchanged(t *testing.T) {
	for _, length := range []uint64{(1 << 20) + 1} {
		for _, populated := range []bool{false, true} {
			f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
				for _, n := range []byte{3, 4} {
					var slot common.Hash
					slot[31] = n
					storage[crypto.Keccak256Hash(slot[:])] = stringWord(length)
					if populated {
						storage[metadataChunk(slot, 0)] = common.HexToHash("0x01")
					}
				}
			})
			before := filesDigest(t, f.source)
			migration := filepath.Join(t.TempDir(), "migration")
			_, err := Migrate(t.Context(), f.options(migration))
			must(t, err)
			dump := filepath.Join(t.TempDir(), "dump")
			_, err = Dump(t.Context(), dumpOptions(f, dump))
			must(t, err)
			_, migrated, _ := readArtifact(t, migration)
			_, raw, _ := readArtifact(t, dump)
			for _, n := range []byte{3, 4} {
				var slot common.Hash
				slot[31] = n
				k := crypto.Keccak256Hash(slot[:])
				if raw[ovmAddress][k] != stringWord(length) || migrated[ovmAddress][k] != raw[ovmAddress][k] {
					t.Fatal("metadata changed")
				}
				k = metadataChunk(slot, 0)
				if migrated[ovmAddress][k] != raw[ovmAddress][k] {
					t.Fatal("chunk changed")
				}
			}
			if filesDigest(t, f.source) != before {
				t.Fatal("source changed")
			}
		}
	}
}

type metadataCancelContext struct {
	context.Context
	checks int
}

func (c *metadataCancelContext) Err() error {
	c.checks++
	if c.checks >= 100 {
		return context.Canceled
	}
	return nil
}

func TestMetadataRangeCancellationAndWrap(t *testing.T) {
	slot := common.HexToHash("0x03")
	r, err := recognizeString(slot, stringWord(^uint64(0)))
	must(t, err)
	m := metadataLayout{strings: [2]metadataString{r}}
	ctx := &metadataCancelContext{Context: t.Context()}
	if _, err := m.contains(ctx, common.HexToHash("0xff")); !errors.Is(err, context.Canceled) || ctx.checks != 100 {
		t.Fatalf("long search ignored cancellation: %v", err)
	}
	m.strings[0] = metadataString{base: *new(uint256.Int).Sub(new(uint256.Int), uint256.NewInt(2)), chunks: 4}
	for _, key := range []common.Hash{common.HexToHash("0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffe"), {}, common.HexToHash("0x01"), common.HexToHash("0x02")} {
		found, err := m.contains(t.Context(), crypto.Keccak256Hash(key[:]))
		must(t, err)
		if found != (key != common.HexToHash("0x02")) {
			t.Fatal("modular range membership changed")
		}
	}
}

func TestMetadataSearchAllocationBound(t *testing.T) {
	slot := common.HexToHash("0x03")
	r, err := recognizeString(slot, stringWord(1<<40))
	must(t, err)
	m := metadataLayout{strings: [2]metadataString{r}}
	allocations := func(offset uint64) float64 {
		key := metadataChunk(slot, offset)
		return testing.AllocsPerRun(5, func() {
			found, err := m.contains(t.Context(), key)
			must(t, err)
			if !found {
				t.Fatal("lost chunk")
			}
		})
	}
	if early, late := allocations(0), allocations(4096); late > early {
		t.Fatalf("search allocations grew: early=%v late=%v", early, late)
	}
}

func TestMetadataAmbiguityMatchesExpansion(t *testing.T) {
	f := newFixture(t, 0, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	x, err := s.prepareSnapshot(t.Context(), 2, l, f.validatedEvents(t))
	must(t, err)
	root := s.memory.get(ovmHash).Root
	for _, key := range []common.Hash{balanceKey(eoa), allowanceKey(eoa, automatic)} {
		m, err := s.metadataSlots(t.Context(), root)
		must(t, err)
		old, err := s.legacyMetadataSlots(t.Context(), root)
		must(t, err)
		// Force known mapping/metadata overlap; neither classifier may prioritize
		// one category and silently discard the ambiguity.
		m.fixed[4] = key
		old[key] = struct{}{}
		_, oldErr := s.legacyBalanceJobs(t.Context(), x, root, old, l)
		_, newErr := s.balanceJobs(t.Context(), x, root, m, l)
		if oldErr == nil || newErr == nil || oldErr.Error() != newErr.Error() {
			t.Fatalf("ambiguity changed: old=%v new=%v", oldErr, newErr)
		}
	}
}
