package ovmevents

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/metis-devops/metis-l2state/internal/strictio"
)

func check(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func sample() *Document {
	d := New("1088", common.HexToHash("0x1234"))
	d.Progress = Position{2, common.HexToHash("0xabcd")}
	a, b := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	d.Allowances = []Allowance{{a, b}}
	d.Members = []common.Address{a}
	return d
}

func TestDocumentStrictness(t *testing.T) {
	d := sample()
	var out bytes.Buffer
	check(t, Encode(&out, d))
	raw := out.String()
	got, err := Decode(t.Context(), strings.NewReader(raw))
	check(t, err)
	if got.IndexDigest() != d.IndexDigest() {
		t.Fatal("digest mismatch")
	}
	for name, change := range map[string]func(string) string{
		"unknown":   func(s string) string { return strings.Replace(s, "\"version\":", "\"other\":", 1) },
		"case":      func(s string) string { return strings.Replace(s, "\"version\":", "\"Version\":", 1) },
		"duplicate": func(s string) string { return strings.Replace(s, "\"version\":", "\"version\":1,\"version\":", 1) },
		"escaped duplicate": func(s string) string {
			return strings.Replace(s, "\"version\":", "\"vers\\u0069on\":1,\"version\":", 1)
		},
		"missing":     func(s string) string { return strings.Replace(s, "\"version\":1\n,", "", 1) },
		"null":        func(s string) string { return strings.Replace(s, "\"version\":1", "\"version\":null", 1) },
		"negative":    func(s string) string { return strings.Replace(s, "\"version\":1", "\"version\":-1", 1) },
		"trailing":    func(s string) string { return s + "{}" },
		"large token": func(s string) string { return strings.Replace(s, d.ChainID, strings.Repeat("a", 5000), 1) },
		"bad owner": func(s string) string {
			return strings.Replace(s, d.Allowances[0].Owner.Hex(), "0x01", 1)
		},
		"null array": func(s string) string { return strings.Replace(s, "\"members\":[", "\"members\":null,\"ignored\":[", 1) },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(t.Context(), strings.NewReader(change(raw))); err == nil {
				t.Fatal("accepted malformed document")
			}
		})
	}
	for _, modify := range []func(*Document){func(d *Document) { d.Allowances = append(d.Allowances, d.Allowances[0]) }, func(d *Document) { d.Members = append(d.Members, d.Members[0]) }, func(d *Document) { d.ChainID = "01088" }} {
		d := sample()
		modify(d)
		if d.Validate() == nil {
			t.Fatal("accepted invalid document")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Decode(ctx, strings.NewReader(raw)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type fakeBackend struct {
	headers  []*types.Header
	events   []types.Log
	head     uint64
	requests [][2]uint64
	onLogs   func(uint64, uint64) error
	onHeader func(string) error
	id       string
}

func fakeChain() *fakeBackend {
	b := &fakeBackend{id: "1088", head: 3}
	for i := range 4 {
		h := &types.Header{Number: big.NewInt(int64(i)), Difficulty: big.NewInt(1), Root: common.HexToHash("0x01"), Time: uint64(i)}
		if i > 0 {
			h.ParentHash = b.headers[i-1].Hash()
		}
		b.headers = append(b.headers, h)
	}
	return b
}
func (b *fakeBackend) chainID(context.Context) (string, error) { return b.id, nil }
func (b *fakeBackend) header(_ context.Context, tag string) (*types.Header, error) {
	if b.onHeader != nil {
		if err := b.onHeader(tag); err != nil {
			return nil, err
		}
	}
	if tag == "latest" {
		return b.headers[b.head], nil
	}
	n, err := hexutil.DecodeUint64(tag)
	if err != nil {
		return nil, err
	}
	return b.headers[n], nil
}
func (b *fakeBackend) logs(_ context.Context, from, to uint64) ([]types.Log, error) {
	b.requests = append(b.requests, [2]uint64{from, to})
	if b.onLogs != nil {
		if err := b.onLogs(from, to); err != nil {
			return nil, err
		}
	}
	logs := []types.Log{}
	for _, l := range b.events {
		if l.BlockNumber >= from && l.BlockNumber <= to {
			logs = append(logs, l)
		}
	}
	return logs, nil
}
func (b *fakeBackend) event(block uint64, index uint, topic common.Hash, from, to common.Address, amount byte) types.Log {
	data := make([]byte, 32)
	data[31] = amount
	return types.Log{Address: Contract, Topics: []common.Hash{topic, common.BytesToHash(from[:]), common.BytesToHash(to[:])}, Data: data, BlockNumber: block, BlockHash: b.headers[block].Hash(), TxHash: common.HexToHash("0x01"), Index: index}
}
func syncOptions(t *testing.T, target uint64) Options {
	return Options{Output: filepath.Join(t.TempDir(), "events.json"), ToBlock: &target, BatchSize: 2, PollInterval: time.Millisecond, FlushInterval: time.Millisecond, RPCTimeout: time.Second}
}
func TestSyncResumeTargetAndSemantics(t *testing.T) {
	b := fakeChain()
	a, c := common.HexToAddress("0x01"), common.HexToAddress("0x02")
	b.events = []types.Log{b.event(1, 0, ApprovalTopic, a, c, 0), b.event(1, 1, TransferTopic, a, c, 0), b.event(1, 2, ApprovalTopic, a, c, 5), b.event(2, 0, TransferTopic, c, common.Address{}, 1), b.event(2, 1, TransferTopic, c, a, 2), b.event(3, 0, TransferTopic, c, a, 2), b.event(3, 1, ApprovalTopic, a, c, 1)}
	o := syncOptions(t, 1)
	r, err := synchronize(t.Context(), o, b, Save)
	check(t, err)
	if r.Allowances != 1 || r.Members != 0 || r.Progress.BlockNumber != 1 {
		t.Fatal(r)
	}
	n := uint64(2)
	o.ToBlock = &n
	for ; n <= 3; n++ {
		b.requests = nil
		r, err = synchronize(t.Context(), o, b, Save)
		check(t, err)
		if len(b.requests) != 1 || b.requests[0] != [2]uint64{n, n} || r.Allowances != 1 || r.Members != 1 {
			t.Fatal(r, b.requests)
		}
	}
	n = 3
	d, _, err := ReadFile(t.Context(), o.Output)
	check(t, err)
	if d.Members[0] != c {
		t.Fatal("lost Transfer-from membership")
	}
	b.requests = nil
	_, err = synchronize(t.Context(), o, b, Save)
	check(t, err)
	if len(b.requests) != 0 {
		t.Fatal("target replayed")
	}
	n = 2
	if _, err := synchronize(t.Context(), o, b, Save); err == nil {
		t.Fatal("accepted lower target")
	}
	n = 3
	b.id = "999"
	if _, err := synchronize(t.Context(), o, b, Save); err == nil {
		t.Fatal("accepted wrong chain")
	}
	b.id = "1088"
	b.headers[3].Extra = []byte{1}
	if _, err := synchronize(t.Context(), o, b, Save); err == nil {
		t.Fatal("accepted reorg")
	}
}
func TestBatchRejectsMalformedAndDoesNotAdvance(t *testing.T) {
	for _, name := range []string{"duplicate", "removed", "height", "hash", "topics", "unexpected-topic", "padding", "data", "contract", "txhash", "reorg", "canceled"} {
		t.Run(name, func(t *testing.T) {
			b := fakeChain()
			a := common.HexToAddress("0x01")
			l := b.event(1, 2, TransferTopic, a, common.Address{}, 1)
			switch name {
			case "removed":
				l.Removed = true
			case "height":
				l.BlockNumber = 9
			case "hash":
				l.BlockHash = common.HexToHash("0x99")
			case "topics":
				l.Topics = l.Topics[:2]
			case "unexpected-topic":
				l.Topics[0] = common.HexToHash("0x99")
			case "padding":
				l.Topics[1][0] = 1
			case "data":
				l.Data = nil
			case "contract":
				l.Address = a
			case "txhash":
				l.TxHash = common.Hash{}
			}
			b.events = []types.Log{
				b.event(1, 0, ApprovalTopic, a, common.Address{}, 1),
				b.event(1, 1, TransferTopic, common.Address{}, a, 1),
				l,
			}
			if name == "duplicate" {
				b.events = append(b.events, l)
			}
			if name == "reorg" {
				b.onLogs = func(uint64, uint64) error { b.headers[1].Extra = []byte{1}; return nil }
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if name == "canceled" {
				b.onLogs = func(uint64, uint64) error { cancel(); return nil }
			}
			s := &synchronizer{rpc: b, log: progressLogger(nil), doc: New("1088", b.headers[0].Hash()), allowances: map[common.Hash]Allowance{}, members: map[common.Address]struct{}{}}
			if name == "height" {
				if err := validateLog(l, 0, 1, b.headers[0].Hash(), b.headers[1].Hash()); err == nil {
					t.Fatal("bad height")
				}
				return
			}
			if err := s.batch(ctx, 0, 1); err == nil {
				t.Fatal("accepted malformed batch")
			}
			if s.haveProgress || s.dirty || s.doc.Progress != (Position{}) || len(s.members) != 0 || len(s.doc.Members) != 0 || len(s.allowances) != 0 || len(s.doc.Allowances) != 0 {
				t.Fatal("partial commit")
			}
		})
	}
}
func TestFollowWaitAndCancellationFlush(t *testing.T) {
	b := fakeChain()
	b.head = 0
	o := syncOptions(t, 2)
	o.BatchSize = 1
	o.FlushInterval = time.Hour
	calls := 0
	b.onHeader = func(tag string) error {
		if tag == "latest" {
			calls++
			if calls == 3 {
				b.head = 2
			}
		}
		return nil
	}
	r, err := synchronize(t.Context(), o, b, Save)
	check(t, err)
	if r.Progress.BlockNumber != 2 || calls < 3 {
		t.Fatal(r)
	}
	o = syncOptions(t, 0)
	o.ToBlock = nil
	o.BatchSize = 1
	o.FlushInterval = time.Hour
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b = fakeChain()
	b.onLogs = func(from, to uint64) error {
		if from == 1 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	_, err = synchronize(ctx, o, b, Save)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	d, _, err := ReadFile(t.Context(), o.Output)
	check(t, err)
	if d.Progress.BlockNumber != 0 {
		t.Fatal("partial block persisted")
	}
}

type rpcFault struct {
	code    int
	message string
}

func (e rpcFault) Error() string  { return e.message }
func (e rpcFault) ErrorCode() int { return e.code }
func TestRangeSplittingAndRetry(t *testing.T) {
	b := fakeChain()
	o := syncOptions(t, 3)
	o.BatchSize = 4
	calls := 0
	b.onLogs = func(from, to uint64) error {
		calls++
		if calls == 1 {
			return rpc.HTTPError{StatusCode: 429}
		}
		if to > from {
			return rpcFault{-32005, "too many results"}
		}
		return nil
	}
	r, err := synchronize(t.Context(), o, b, Save)
	check(t, err)
	if r.Progress.BlockNumber != 3 || len(b.requests) < 5 {
		t.Fatal(r, b.requests)
	}
	b = fakeChain()
	b.onLogs = func(uint64, uint64) error { return rpcFault{-32601, "method not found"} }
	o = syncOptions(t, 1)
	if _, err := synchronize(t.Context(), o, b, Save); err == nil {
		t.Fatal("ignored permanent error")
	}
	if len(b.requests) != 1 {
		t.Fatal("retried permanent failure")
	}
	if _, err := os.Stat(o.Output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("persisted failure")
	}
}
func TestLockAndCheckpointRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.json")
	_, lock, err := lockOutput(path)
	check(t, err)
	if _, _, err := lockOutput(path); err == nil {
		t.Fatal("accepted concurrent writer")
	}
	check(t, lock.Close())
	_, lock, err = lockOutput(path)
	check(t, err)
	check(t, lock.Close())
	d := sample()
	check(t, Save(path, d))
	before, err := os.ReadFile(path)
	check(t, err)
	check(t, os.WriteFile(filepath.Join(filepath.Dir(path), ".ovm-events.partial-crash"), []byte("broken"), 0600))
	loaded, _, err := ReadFile(t.Context(), path)
	check(t, err)
	var out bytes.Buffer
	check(t, Encode(&out, loaded))
	if !bytes.Equal(before, out.Bytes()) {
		t.Fatal("partial changed checkpoint")
	}
	link := filepath.Join(t.TempDir(), "link")
	check(t, os.Symlink(path, link))
	if _, _, err := ReadFile(t.Context(), link); err == nil {
		t.Fatal("followed symlink")
	}
}
func FuzzDocument(f *testing.F) {
	var b bytes.Buffer
	check(f, Encode(&b, sample()))
	f.Add(b.Bytes())
	f.Fuzz(func(t *testing.T, raw []byte) {
		d, err := Decode(t.Context(), bytes.NewReader(raw))
		if err != nil {
			return
		}
		var b bytes.Buffer
		check(t, Encode(&b, d))
		v, err := Decode(t.Context(), &b)
		check(t, err)
		if v.IndexDigest() != d.IndexDigest() {
			t.Fatal("unstable document")
		}
	})
}

var _ io.Reader = strictio.TokenReader(context.Background(), strings.NewReader(""), 4096, 4096, "events")

func TestRepeatedMembersStillValidateLogs(t *testing.T) {
	for _, restored := range []bool{false, true} {
		b := fakeChain()
		address := common.HexToAddress("0x01")
		s := &synchronizer{members: make(map[common.Address]struct{})}
		logs := []types.Log{b.event(1, 0, TransferTopic, address, common.Address{}, 1)}
		if restored {
			s.members[address] = struct{}{}
		}
		bad := b.event(1, 1, TransferTopic, address, common.Address{}, 2)
		bad.BlockHash = common.HexToHash("0x99")
		logs = append(logs, bad)
		if _, _, err := s.validateLogs(logs, 0, 3, b.headers[0].Hash(), b.headers[3].Hash()); err == nil {
			t.Fatalf("deduplication bypassed log validation (restored=%v)", restored)
		}
	}
}
