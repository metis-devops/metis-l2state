package ovmevents

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func boundarySync(b *fakeBackend) *synchronizer {
	d := New(b.id, b.headers[0].Hash())
	d.Progress = Position{0, b.headers[0].Hash()}
	return &synchronizer{rpc: b, doc: d, haveProgress: true, log: progressLogger(nil), o: Options{FlushInterval: time.Hour}, lastFlush: time.Now(), allowances: map[common.Hash]Allowance{}, members: map[common.Address]struct{}{}}
}

func TestBatchHeaderRequestsConstant(t *testing.T) {
	for _, size := range []int{3, 100, 10000} {
		for _, density := range []string{"empty", "sparse", "dense"} {
			t.Run(fmt.Sprintf("%d/%s", size, density), func(t *testing.T) {
				b := fakeChain()
				for i := 4; i <= size; i++ {
					b.headers = append(b.headers, &types.Header{Number: big.NewInt(int64(i)), Root: common.HexToHash("0x01"), Difficulty: big.NewInt(1)})
				}
				// Interior headers deliberately need not form a chain: only boundaries are queried.
				for i := 1; i <= size; i++ {
					if density == "dense" || (density == "sparse" && i == size/2) {
						b.events = append(b.events, b.event(uint64(i), 0, TransferTopic, common.Address{}, common.Address{}, 1))
					}
				}
				var calls []string
				b.onHeader = func(tag string) error { calls = append(calls, tag); return nil }
				s := boundarySync(b)
				check(t, s.batch(t.Context(), 1, uint64(size)))
				end := fmt.Sprintf("0x%x", size)
				if !slices.Equal(calls, []string{"0x0", end, end, "0x0"}) {
					t.Fatal(calls)
				}
				if s.doc.Progress != (Position{uint64(size), b.headers[size].Hash()}) {
					t.Fatal(s.doc.Progress)
				}
			})
		}
	}
}

type unfilteredBackend struct{ *fakeBackend }

func (b unfilteredBackend) logs(ctx context.Context, from, to uint64) ([]types.Log, error) {
	if _, err := b.fakeBackend.logs(ctx, from, to); err != nil {
		return nil, err
	}
	return slices.Clone(b.events), nil
}

func TestBoundaryFailuresDoNotCommit(t *testing.T) {
	for _, name := range []string{"checkpoint-before", "checkpoint-after", "endpoint", "empty-endpoint", "genesis", "zero-hash", "below", "above", "same-block", "genesis-log", "endpoint-log", "canceled", "cancel-final-check"} {
		t.Run(name, func(t *testing.T) {
			b := fakeChain()
			s := boundarySync(b)
			from, to := uint64(1), uint64(3)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			l := b.event(2, 0, TransferTopic, common.HexToAddress("0x01"), common.Address{}, 1)
			switch name {
			case "checkpoint-before":
				b.headers[0].Extra = []byte{1}
			case "checkpoint-after":
				b.onLogs = func(uint64, uint64) error { b.headers[0].Extra = []byte{1}; return nil }
			case "endpoint", "empty-endpoint":
				b.onLogs = func(uint64, uint64) error { b.headers[3].Extra = []byte{1}; return nil }
			case "genesis":
				s.haveProgress = false
				from = 0
				to = 0
				b.headers[0].Extra = []byte{1}
			case "zero-hash":
				l.BlockHash = common.Hash{}
			case "below":
				l.BlockNumber = 0
			case "above":
				l.BlockNumber = 4
			case "same-block":
				other := l
				other.Index = 1
				other.BlockHash = common.HexToHash("0x99")
				b.events = append(b.events, other)
			case "genesis-log":
				s.haveProgress = false
				from = 0
				l.BlockNumber = 0
			case "endpoint-log":
				l.BlockNumber = 3
			case "cancel-final-check":
				calls := 0
				b.onHeader = func(tag string) error {
					if tag == "0x0" {
						calls++
						if calls == 2 {
							cancel()
						}
					}
					return nil
				}
			case "canceled":
				b.onLogs = func(uint64, uint64) error { cancel(); return nil }
			}
			before := s.doc.Progress
			hadProgress := s.haveProgress
			// Inject out-of-range responses too, bypassing the normal fake RPC filter.
			b.events = append(b.events, l)
			if name == "empty-endpoint" {
				b.events = nil
			}
			s.rpc = unfilteredBackend{b}
			err := s.batch(ctx, from, to)
			if err == nil {
				t.Fatal("accepted invalid batch")
			}
			if s.doc.Progress != before || s.haveProgress != hadProgress || s.dirty || len(s.doc.Members) != 0 || len(s.doc.Allowances) != 0 || len(s.members) != 0 || len(s.allowances) != 0 {
				t.Fatal("partial commit")
			}
		})
	}
}

func TestRetryAndRangeReductionReconfirmCheckpoint(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprint(limited), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := fakeChain()
				s := boundarySync(b)
				s.o.BatchSize = 3
				calls := 0
				b.onLogs = func(uint64, uint64) error {
					calls++
					b.headers[0].Extra = []byte{1}
					if limited {
						return errors.New("block range limit")
					}
					return context.DeadlineExceeded
				}
				err := s.retry(t.Context(), func() error { _, err := s.advance(t.Context()); return err })
				if err == nil || temporary(err) || calls != 1 || s.dirty || s.doc.Progress.BlockNumber != 0 {
					t.Fatalf("calls=%d err=%v", calls, err)
				}
			})
		})
	}
}
