package ovmevents

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

func TestSyncFirstOccurrenceOrder(t *testing.T) {
	b := fakeChain()
	a, c, e := common.HexToAddress("0x01"), common.HexToAddress("0x02"), common.HexToAddress("0x03")
	logs := []types.Log{
		b.event(0, 0, ApprovalTopic, e, a, 0),
		b.event(0, 1, TransferTopic, a, c, 0), // Zero amounts do not establish membership.
		b.event(0, 2, TransferTopic, e, a, 1),
		b.event(0, 3, ApprovalTopic, a, c, 1),
		b.event(0, 4, ApprovalTopic, e, a, 2), // Nonadjacent duplicate in the same block.
		b.event(0, 5, TransferTopic, e, c, 1),
		b.event(1, 0, TransferTopic, a, e, 1),
		b.event(1, 1, ApprovalTopic, c, e, 0),
		b.event(1, 2, ApprovalTopic, a, c, 0),
		b.event(2, 0, TransferTopic, c, a, 1),
		b.event(2, 1, ApprovalTopic, e, a, 1),
		b.event(3, 0, TransferTopic, e, c, 1),
		b.event(3, 1, ApprovalTopic, a, c, 2),
	}
	want := New("1088", b.headers[0].Hash())
	want.Progress = Position{3, b.headers[3].Hash()}
	want.Allowances = []Allowance{{e, a}, {a, c}, {c, e}}
	want.Members = []common.Address{e, a, c}
	var encoded bytes.Buffer
	check(t, Encode(&encoded, want))
	for _, order := range []string{"forward", "reverse", "interleaved"} {
		for _, batchSize := range []uint64{1, 2, 4} {
			for _, flush := range []time.Duration{time.Nanosecond, time.Hour} {
				for _, resume := range []int{-1, 0, 1} {
					t.Run(fmt.Sprintf("%s/batch%d/flush%s/resume%d", order, batchSize, flush, resume), func(t *testing.T) {
						b := fakeChain()
						b.events = slices.Clone(logs)
						switch order {
						case "reverse":
							slices.Reverse(b.events)
						case "interleaved":
							b.events = nil
							for parity := range 2 {
								for i := parity; i < len(logs); i += 2 {
									b.events = append(b.events, logs[i])
								}
							}
						}
						o := syncOptions(t, 3)
						o.BatchSize, o.FlushInterval = batchSize, flush
						if resume >= 0 {
							prefix := uint64(resume)
							o.ToBlock = &prefix
							_, err := synchronize(t.Context(), o, b, Save)
							check(t, err)
							// Resume with a different batch size and response order.
							o.BatchSize = 5 - batchSize
							slices.Reverse(b.events)
						}
						target := uint64(3)
						o.ToBlock = &target
						_, err := synchronize(t.Context(), o, b, Save)
						check(t, err)
						raw, err := os.ReadFile(o.Output)
						check(t, err)
						if !bytes.Equal(raw, encoded.Bytes()) {
							t.Fatalf("checkpoint lost first-occurrence order:\n%s", raw)
						}
						d, err := Decode(t.Context(), bytes.NewReader(raw))
						check(t, err)
						if d.IndexDigest() != want.IndexDigest() {
							t.Fatal("batching, response order or resume changed digest")
						}
					})
				}
			}
		}
	}
}

func TestAllowanceKeyConflictsFailClosed(t *testing.T) {
	a, b := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	key := AllowanceKey(a, b)
	for _, restored := range []bool{false, true} {
		s := &synchronizer{allowances: make(map[common.Hash]Allowance)}
		batch := make(map[common.Hash]Allowance)
		if restored {
			s.allowances[key] = Allowance{b, a}
		} else {
			batch[key] = Allowance{b, a}
		}
		if added, err := s.addAllowance(batch, a, b); err == nil || added {
			t.Fatalf("accepted conflicting key (restored=%v)", restored)
		}
	}
}
