package migration

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/trie"
)

func partitionFixture(t *testing.T, count int) (*source, *trie.ID) {
	t.Helper()
	f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
		clear(storage)
		for n := range count {
			var key common.Hash
			key[0] = 0x30
			binary.BigEndian.PutUint64(key[24:], uint64(n))
			var value common.Hash
			binary.BigEndian.PutUint64(value[24:], uint64(n+1))
			storage[key] = value
		}
	})
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	t.Cleanup(func() { must(t, s.close()) })
	r := accountReader{s: s}
	a, err := r.get(ovmHash)
	must(t, err)
	return s, trie.StorageTrieID(s.head.StateRoot, ovmHash, a.Root)
}

func TestPartitionBatchBoundaries(t *testing.T) {
	for _, count := range []int{0, 1, 31, 32, 33, 64, 65, 257} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, id := partitionFixture(t, count)
			l := make(limiter, 2)
			got := 0
			must(t, s.partitionStream(t.Context(), id, l, func(v leaf) error {
				if v.key[0] != 0x30 || binary.BigEndian.Uint64(v.key[24:]) != uint64(got) {
					return errors.New("batch reordered storage")
				}
				value, err := decodeSlot(v.value)
				if err != nil {
					return err
				}
				var want common.Hash
				binary.BigEndian.PutUint64(want[24:], uint64(got+1))
				if value != want {
					return errors.New("batch aliased iterator value")
				}
				got++
				return nil
			}))
			if got != count || len(l) != 0 {
				t.Fatalf("count=%d permits=%d", got, len(l))
			}
		})
	}
}

func TestPartitionBatchBlockedQueueReleasesPermits(t *testing.T) {
	s, id := partitionFixture(t, 4*streamAhead)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	l := make(limiter, 2)
	out := make(chan leaf, streamAhead)
	done := make(chan error, 1)
	go func() { done <- s.producePartition(ctx, id, 3, l, out) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(out) != cap(out) || len(l) != 0 {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("blocked partition retained worker permits")
		}
	}
	// All permits remain independently available while the full queue blocks.
	for range cap(l) {
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("permit unavailable")
		}
	}
	for range cap(l) {
		<-l
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(l) != 0 {
		t.Fatal("leaked worker permit")
	}
}

func TestPartitionBatchConsumerFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		s, id := partitionFixture(t, 129)
		ctx, cancel := context.WithCancel(t.Context())
		l := make(limiter, 2)
		sentinel := errors.New("consumer failed")
		calls := 0
		err := s.partitionStream(ctx, id, l, func(leaf) error {
			calls++
			if cancelled {
				cancel()
				return ctx.Err()
			}
			return sentinel
		})
		cancel()
		want := sentinel
		if cancelled {
			want = context.Canceled
		}
		if !errors.Is(err, want) || calls != 1 || len(l) != 0 {
			t.Fatalf("err=%v calls=%d permits=%d", err, calls, len(l))
		}
	}
}

func TestIndexBatchCancellationAndVisitorFailure(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		s, _ := partitionFixture(t, 0)
		ctx, cancel := context.WithCancel(t.Context())
		l := make(limiter, 2)
		sentinel := errors.New("index consumer failed")
		calls := 0
		err := s.indexRecords(ctx, l, []byte(preimageKeyPrefix), func([]byte, []byte) error {
			calls++
			if cancelled {
				cancel()
				return nil
			}
			return sentinel
		})
		cancel()
		want := sentinel
		if cancelled {
			want = context.Canceled
		}
		if !errors.Is(err, want) || calls != 1 || len(l) != 0 {
			t.Fatalf("err=%v calls=%d permits=%d", err, calls, len(l))
		}
	}
}
