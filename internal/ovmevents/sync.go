package ovmevents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"slices"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rpc"
)

// Options configures one sequential, resumable RPC synchronizer.
type Options struct {
	RPCURL, Output                          string
	ToBlock                                 *uint64
	BatchSize                               uint64
	PollInterval, FlushInterval, RPCTimeout time.Duration
	Progress                                io.Writer
}

// Result identifies the last durably saved complete prefix.
type Result struct {
	Output     string   `json:"output"`
	Progress   Position `json:"progress"`
	Allowances int      `json:"allowances"`
	Members    int      `json:"members"`
}

func (o Options) validate() error {
	if o.RPCURL == "" || o.Output == "" {
		return errors.New("rpc-url and out are required")
	}
	if o.BatchSize == 0 || o.BatchSize > 10000 {
		return errors.New("batch-size must be between 1 and 10000")
	}
	if o.PollInterval <= 0 || o.FlushInterval <= 0 || o.RPCTimeout <= 0 {
		return errors.New("intervals and rpc-timeout must be positive")
	}
	return nil
}

// Sync holds an exclusive checkpoint lock throughout synchronization.
func Sync(ctx context.Context, o Options) (result Result, retErr error) {
	if ctx == nil {
		return result, errors.New("nil context")
	}
	if err := o.validate(); err != nil {
		return result, err
	}
	path, lock, err := lockOutput(o.Output)
	if err != nil {
		return result, err
	}
	defer func() { retErr = errors.Join(retErr, lock.Close()) }()
	o.Output = path
	client, err := rpc.DialContext(ctx, o.RPCURL)
	if err != nil {
		return result, err
	}
	defer client.Close()
	return synchronize(ctx, o, &rpcBackend{client: client, timeout: o.RPCTimeout}, Save)
}

type synchronizer struct {
	o                   Options
	rpc                 backend
	save                func(string, *Document) error
	log                 *slog.Logger
	doc                 *Document
	haveProgress, dirty bool
	lastFlush           time.Time
	allowances          map[common.Hash]Allowance
	members             map[common.Address]struct{}
}

func synchronize(ctx context.Context, o Options, b backend, save func(string, *Document) error) (result Result, retErr error) {
	out := o.Progress
	if out == nil {
		out = io.Discard
	}
	s := &synchronizer{o: o, rpc: b, save: save, log: slog.New(slog.NewTextHandler(out, nil)), lastFlush: time.Now(), allowances: make(map[common.Hash]Allowance), members: make(map[common.Address]struct{})}
	if err := s.restore(ctx); err != nil {
		return result, err
	}
	// Cancellation publishes only already validated batches, never partial responses.
	defer func() {
		if ctx.Err() != nil {
			retErr = errors.Join(retErr, s.flush())
		}
	}()
	if err := s.retry(ctx, func() error { return s.identify(ctx) }); err != nil {
		return result, err
	}
	if err := s.run(ctx); err != nil {
		return result, err
	}
	return Result{o.Output, s.doc.Progress, len(s.doc.Allowances), len(s.doc.Members)}, nil
}
func (s *synchronizer) restore(ctx context.Context) error {
	d, _, err := ReadFile(ctx, s.o.Output)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	s.doc = d
	s.haveProgress = true
	if s.o.ToBlock != nil && *s.o.ToBlock < d.Progress.BlockNumber {
		return errors.New("target is below checkpoint; use a new output file")
	}
	for _, a := range d.Allowances {
		s.allowances[AllowanceKey(a.Owner, a.Spender)] = a
	}
	for _, m := range d.Members {
		s.members[m] = struct{}{}
	}
	return nil
}
func (s *synchronizer) identify(ctx context.Context) error {
	id, err := s.rpc.chainID(ctx)
	if err != nil {
		return err
	}
	g, err := s.rpc.header(ctx, "0x0")
	if err != nil {
		return err
	}
	if s.doc == nil {
		s.doc = New(id, g.Hash())
		return nil
	}
	if s.doc.ChainID != id || s.doc.Genesis != g.Hash() {
		return errors.New("RPC chain identity differs from checkpoint")
	}
	return s.confirm(ctx)
}
func (s *synchronizer) confirm(ctx context.Context) error {
	if !s.haveProgress {
		return nil
	}
	h, err := s.rpc.header(ctx, hexutil.EncodeUint64(s.doc.Progress.BlockNumber))
	if err != nil {
		return err
	}
	if h.Hash() != s.doc.Progress.BlockHash {
		return errors.New("canonical checkpoint changed (reorg); rebuild into a new file")
	}
	return nil
}
func (s *synchronizer) retry(ctx context.Context, op func() error) error {
	delay := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op()
		if err == nil {
			return nil
		}
		if !temporary(err) {
			return err
		}
		s.log.Warn("RPC request failed; retrying", "error", err, "delay", delay)
		if err := s.flushDue(); err != nil {
			return err
		}
		if err := pause(ctx, delay); err != nil {
			return err
		}
		delay = min(30*time.Second, delay*2)
	}
}
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (s *synchronizer) run(ctx context.Context) error {
	for {
		if err := s.retry(ctx, func() error { return s.confirm(ctx) }); err != nil {
			return err
		}
		if s.haveProgress && s.o.ToBlock != nil && s.doc.Progress.BlockNumber == *s.o.ToBlock {
			return s.flush()
		}
		if s.haveProgress && s.doc.Progress.BlockNumber == math.MaxUint64 {
			return errors.New("block height overflow")
		}
		advanced := false
		if err := s.retry(ctx, func() error { var err error; advanced, err = s.advance(ctx); return err }); err != nil {
			return err
		}
		if err := s.flushDue(); err != nil {
			return err
		}
		if !advanced {
			if err := pause(ctx, min(s.o.PollInterval, s.o.FlushInterval)); err != nil {
				return err
			}
		}
	}
}
func (s *synchronizer) advance(ctx context.Context) (bool, error) {
	head, err := s.rpc.header(ctx, "latest")
	if err != nil {
		return false, err
	}
	from := uint64(0)
	if s.haveProgress {
		from = s.doc.Progress.BlockNumber + 1
	}
	to := head.Number.Uint64()
	if s.o.ToBlock != nil {
		to = min(to, *s.o.ToBlock)
	}
	if from > to {
		return false, nil
	}
	if to-from >= s.o.BatchSize {
		to = from + s.o.BatchSize - 1
	}
	for {
		err = s.batch(ctx, from, to)
		if err == nil {
			return true, nil
		}
		if !rangeLimited(err) || from == to {
			return false, err
		}
		to = from + (to-from)/2
		s.log.Warn("RPC range limit; reducing batch", "from", from, "to", to)
	}
}
func (s *synchronizer) batch(ctx context.Context, from, to uint64) error {
	headers := make(map[uint64]common.Hash, int(to-from+1))
	previous := s.doc.Progress.BlockHash
	for n := from; ; n++ {
		if err := s.flushDue(); err != nil {
			return err
		}
		h, err := s.rpc.header(ctx, hexutil.EncodeUint64(n))
		if err != nil {
			return err
		}
		if h.Number.Uint64() != n || (n > 0 && h.ParentHash != previous) || (n == 0 && h.Hash() != s.doc.Genesis) {
			return errors.New("non-contiguous canonical RPC headers")
		}
		previous = h.Hash()
		headers[n] = previous
		if n == to {
			break
		}
	}
	logs, err := s.rpc.logs(ctx, from, to)
	if err != nil {
		return err
	}
	a, m, err := s.validateLogs(logs, headers)
	if err != nil {
		return err
	}
	end, err := s.rpc.header(ctx, hexutil.EncodeUint64(to))
	if err != nil {
		return err
	}
	if end.Hash() != headers[to] {
		return errors.New("canonical batch changed (reorg); rebuild into a new file")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for key, v := range a {
		s.allowances[key] = v
		s.doc.Allowances = append(s.doc.Allowances, v)
	}
	for address := range m {
		s.members[address] = struct{}{}
		s.doc.Members = append(s.doc.Members, address)
	}
	s.doc.Progress = Position{to, headers[to]}
	s.haveProgress = true
	s.dirty = true
	s.log.Info("Synchronized OVM events", "height", to, "allowances", len(s.allowances), "members", len(s.members))
	return nil
}
func (s *synchronizer) validateLogs(logs []types.Log, headers map[uint64]common.Hash) (map[common.Hash]Allowance, map[common.Address]struct{}, error) {
	a := make(map[common.Hash]Allowance)
	m := make(map[common.Address]struct{})
	slices.SortFunc(logs, func(a, b types.Log) int {
		if a.BlockNumber < b.BlockNumber {
			return -1
		}
		if a.BlockNumber > b.BlockNumber {
			return 1
		}
		if a.Index < b.Index {
			return -1
		}
		if a.Index > b.Index {
			return 1
		}
		return 0
	})
	for i, l := range logs {
		if err := validateLog(l, headers); err != nil {
			return nil, nil, err
		}
		if i > 0 && logs[i-1].BlockNumber == l.BlockNumber && logs[i-1].Index == l.Index {
			return nil, nil, errors.New("duplicate RPC log position")
		}
		from, to := common.BytesToAddress(l.Topics[1][12:]), common.BytesToAddress(l.Topics[2][12:])
		if l.Topics[0] == ApprovalTopic {
			if err := s.addAllowance(a, from, to); err != nil {
				return nil, nil, err
			}

		} else if common.BytesToHash(l.Data) != (common.Hash{}) {
			if _, ok := s.members[from]; ok {
				continue
			}
			if _, ok := m[from]; ok {
				continue
			}
			m[from] = struct{}{}
		}
	}
	return a, m, nil
}
func validateLog(l types.Log, headers map[uint64]common.Hash) error {
	h, ok := headers[l.BlockNumber]
	if !ok || l.BlockHash != h || l.Removed || l.TxHash == (common.Hash{}) {
		return errors.New("invalid RPC log position")
	}
	if l.Address != Contract || len(l.Topics) != 3 || len(l.Data) != 32 {
		return errors.New("malformed OVM event")
	}
	if l.Topics[0] != TransferTopic && l.Topics[0] != ApprovalTopic {
		return errors.New("unexpected OVM event topic")
	}
	if !bytes.Equal(l.Topics[1][:12], make([]byte, 12)) || !bytes.Equal(l.Topics[2][:12], make([]byte, 12)) {
		return errors.New("malformed OVM event address")
	}
	return nil
}
func (s *synchronizer) flushDue() error {
	if time.Since(s.lastFlush) >= s.o.FlushInterval {
		return s.flush()
	}
	return nil
}
func (s *synchronizer) flush() error {
	if !s.dirty {
		return nil
	}
	s.doc.Sort()
	if err := s.save(s.o.Output, s.doc); err != nil {
		return fmt.Errorf("save checkpoint: %w", err)
	}
	s.dirty = false
	s.lastFlush = time.Now()
	s.log.Info("Persisted OVM events", "height", s.doc.Progress.BlockNumber)
	return nil
}

func (s *synchronizer) addAllowance(batch map[common.Hash]Allowance, from, to common.Address) error {
	v := Allowance{from, to}
	key := AllowanceKey(from, to)
	if old, ok := s.allowances[key]; ok {
		if old != v {
			return errors.New("conflicting allowance")
		}
		return nil
	}
	if old, ok := batch[key]; ok && old != v {
		return errors.New("conflicting batch allowance")
	}
	batch[key] = v
	return nil
}
