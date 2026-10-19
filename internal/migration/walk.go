package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/trie"
	"github.com/metis-devops/metis-l2state/internal/storageword"
	"github.com/metis-devops/metis-l2state/statefile"
)

type accountJob struct {
	hash             common.Hash
	original         *types.StateAccount
	converted, final *accountPatch
	memory           *memoryAccount
}
type accountDone struct {
	job                        accountJob
	converted, final           *types.StateAccount
	convertedSlots, finalSlots uint64
	hasConverted, hasFinal     bool
	ack                        chan error
}

// Queued account records omit the public code/footer fields. Keep the same
// two-pointer channel element and capacity, including for storage-free accounts.
type accountRecord struct {
	Kind         statefile.Kind
	Account      statefile.Account
	Storage      statefile.Storage
	StorageRoot  common.Hash
	StorageCount uint64
}

func (r *accountRecord) fileRecord() statefile.Record {
	return statefile.Record{Kind: r.Kind, Account: r.Account, Storage: r.Storage,
		StorageRoot: r.StorageRoot, StorageCount: r.StorageCount}
}

type accountEvent struct {
	record *accountRecord
	done   *accountDone
}
type accountStream struct {
	hash   common.Hash
	events chan accountEvent
	result chan error
	ack    chan error
}
type walkResult struct {
	sourceRoot, convertedRoot, finalRoot common.Hash
	convertedCounts, counts              statefile.Counts
	codes                                codeHashIndex
}

// Validation keeps only the source accumulator. Prepared output walks reuse its
// result. Without alloc, converted and final state share an accumulator.
type walkMode uint8

const (
	validateSource walkMode = iota
	convertState
	overlayState
)

// Source policy is independent of validation/output traversal stages.
type sourcePolicy uint8

const (
	ovmSource sourcePolicy = iota
	rawSource
)

// Preparation creates evidence only after a complete source validation walk.
// It belongs to one open, stopped source, selected head and validation policy.
type sourceValidation struct {
	policy sourcePolicy
	source *source
	head   head
}

type walkPlan struct {
	capture     *stateSnapshot
	policy      sourcePolicy
	mode        walkMode
	reuseSource bool
	consume     bool
}

func patchAccount(a *types.StateAccount, p *accountPatch) *types.StateAccount {
	a = copyAccount(a)
	if p == nil {
		return a
	}
	if p.Balance != nil {
		a.Balance.Set(p.Balance)
	}
	if p.Nonce != nil {
		a.Nonce = *p.Nonce
	}
	if p.Code != nil {
		a.CodeHash = crypto.Keccak256(*p.Code)
	}
	return a
}

// captureSource validates the OVM source before installing its snapshot.
func (s *source) captureSource(ctx context.Context, workers int, l limiter, capture *stateSnapshot) (walkResult, error) {
	if capture == nil {
		return walkResult{}, errors.New("missing snapshot capture")
	}
	return s.runWalk(ctx, workers, l, walkRequest{plan: walkPlan{mode: validateSource, policy: ovmSource, capture: capture}})
}

// writeMigration accepts only the prepared snapshot belonging to this source.
func (s *source) writeMigration(ctx context.Context, workers int, l limiter, x *indexes, alloc map[common.Hash]*accountPatch, write func(statefile.Record) error) (walkResult, error) {
	if err := ctx.Err(); err != nil {
		return walkResult{}, err
	}
	if x == nil || x.validation == nil || s.memory == nil {
		return walkResult{}, errors.New("migration output requires validated source snapshot")
	}
	if x.validation.source != s || x.validation.head != s.head || x.validation.policy != ovmSource {
		return walkResult{}, errors.New("source validation belongs to a different source, head or policy")
	}
	if !s.memory.outputMu.TryLock() {
		return walkResult{}, errors.New("migration output already in progress")
	}
	defer s.memory.outputMu.Unlock()
	if s.memory.phase != memoryFrozen {
		return walkResult{}, errors.New("migration output requires frozen converted state")
	}
	mode := convertState
	if len(alloc) != 0 {
		mode = overlayState
	}
	s.memory.phase = memoryDraining
	result, err := s.runWalk(ctx, workers, l, walkRequest{plan: walkPlan{mode: mode, policy: ovmSource, reuseSource: true, consume: true}, indexes: x, alloc: alloc, write: write})
	s.memory.accounts = stateTree[*memoryAccount]{}
	s.memory.phase = memoryConsumed
	if err != nil {
		s.memory.phase = memoryFailed
	}
	return result, err
}

// writeDump streams original state without conversion indexes or overlays.
func (s *source) writeDump(ctx context.Context, workers int, l limiter, write func(statefile.Record) error) (walkResult, error) {
	return s.runWalk(ctx, workers, l, walkRequest{plan: walkPlan{mode: convertState, policy: rawSource}, write: write})
}

type walkRequest struct {
	plan             walkPlan
	indexes          *indexes
	converted, alloc map[common.Hash]*accountPatch
	write            func(statefile.Record) error
}

func (s *source) validateWalk(r walkRequest, workers int, l limiter) error {
	p := r.plan
	if workers < 2 || workers > 16 || cap(l) != workers {
		return errors.New("invalid walk worker limits")
	}
	if err := validateWalkPolicy(p.policy, r.indexes, r.converted, r.alloc); err != nil {
		return err
	}
	if p.mode > overlayState {
		return errors.New("invalid walk mode")
	}
	if p.mode == validateSource {
		if r.write != nil || len(r.converted) != 0 || len(r.alloc) != 0 || p.reuseSource {
			return errors.New("source validation cannot write or apply patches")
		}
	} else if r.write == nil || (p.policy == ovmSource && r.indexes == nil) {
		return errors.New("output walk requires writer and migration indexes")
	}
	if p.mode != overlayState && len(r.alloc) != 0 {
		return errors.New("alloc requires overlay mode")
	}
	if p.capture != nil && (p.mode != validateSource || p.policy != ovmSource) {
		return errors.New("snapshot capture requires OVM source validation")
	}
	if p.policy == rawSource && (p.capture != nil || s.memory != nil) {
		return errors.New("dump requires original streaming source")
	}
	if p.reuseSource {
		if r.indexes == nil || r.indexes.validation == nil {
			return errors.New("missing source validation")
		}
		v := r.indexes.validation
		if v.source != s || v.head != s.head || v.policy != p.policy {
			return errors.New("source validation belongs to a different source, head or policy")
		}
	}
	if s.memory != nil && s.memory.phase >= memoryDraining && !p.consume {
		return errors.New("memory state is being consumed or no longer available")
	}
	if s.memory != nil && !p.reuseSource {
		return errors.New("snapshot requires source validation")
	}
	return nil
}

// Admission follows source hash order, with at most 2*workers storage queues.
// Production entrypoints and test-only recomputation oracles share this scheduler.
func (s *source) runWalk(ctx context.Context, workers int, l limiter, request walkRequest) (walkResult, error) {
	if err := ctx.Err(); err != nil {
		return walkResult{}, err
	}
	if err := s.validateWalk(request, workers, l); err != nil {
		return walkResult{}, err
	}
	plan, x, converted, alloc, write := request.plan, request.indexes, request.converted, request.alloc, request.write
	capture := plan.capture
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failure := &runFailure{cancel: cancel}
	window := make(chan struct{}, 2*workers)
	queue := make(chan accountStream, 2*workers)
	producer := make(chan error, 1)
	var jobs sync.WaitGroup
	go s.produceAccounts(ctx, failure, l, x, converted, alloc, plan, window, queue, producer, &jobs)
	acc := newAccountAccumulator(plan, s.head.StateRoot)
	var ret error
	for stream := range queue {
		for e := range stream.events {
			if e.done != nil && e.done.ack != nil {
				e.done.ack <- ret
				continue
			}
			if ret != nil {
				continue
			}
			ret = acc.acceptEvent(e, capture, write)
			if ret != nil {
				ret = fmt.Errorf("account %s during %s: %w", stream.hash, plan.mode, ret)
			}
			failure.record(ret)
		}
		if err := <-stream.result; ret == nil && err != nil {
			ret = err
		}
		if stream.ack != nil {
			stream.ack <- ret
		}
		<-window
	}
	if err := <-producer; ret == nil && err != nil {
		ret = err
	}
	cancel()
	jobs.Wait()
	if failure.err != nil {
		return acc.result, failure.err
	}
	if ret != nil {
		return acc.result, ret
	}
	result := acc.finish()
	if result.sourceRoot != s.head.StateRoot {
		return result, fmt.Errorf("source state root mismatch: expected %s, actual %s", s.head.StateRoot, result.sourceRoot)
	}
	return result, nil
}

type accountAccumulator struct {
	mode                     walkMode
	source, converted, final *trie.StackTrie
	result                   walkResult
}

func newAccountAccumulator(plan walkPlan, sourceRoot common.Hash) *accountAccumulator {
	mode := plan.mode
	a := &accountAccumulator{mode: mode}
	if plan.reuseSource {
		a.result.sourceRoot = sourceRoot
	} else {
		a.source = trie.NewStackTrie(nil)
	}
	if mode != validateSource {
		a.converted = trie.NewStackTrie(nil)
		a.final = a.converted
		if mode == overlayState {
			a.final = trie.NewStackTrie(nil)
		}
	}
	return a
}

func (a *accountAccumulator) finish() walkResult {
	if a.source != nil {
		a.result.sourceRoot = a.source.Hash()
	}
	if a.mode != validateSource {
		a.result.convertedRoot = a.converted.Hash()
		a.result.finalRoot = a.result.convertedRoot
		if a.mode == convertState {
			a.result.counts = a.result.convertedCounts
		} else {
			a.result.finalRoot = a.final.Hash()
		}
	}
	return a.result
}

func (a *accountAccumulator) acceptEvent(e accountEvent, capture *stateSnapshot, write func(statefile.Record) error) error {
	if capture != nil {
		if err := capture.capture(e); err != nil {
			return err
		}
	}
	if e.done != nil {
		return a.accept(e.done)
	}
	if capture == nil {
		return write(e.record.fileRecord())
	}
	return nil
}

func (a *accountAccumulator) accept(d *accountDone) error {
	if a.source != nil && d.job.original != nil {
		if err := updateAccount(a.source, d.job.hash, d.job.original); err != nil {
			return err
		}
	}
	if a.mode == validateSource {
		return nil
	}
	r := &a.result
	// Overlay accounts can retain the same code in both stages. Register both
	// memberships together, while keeping their account/storage counts separate.
	sharedCode := a.mode == overlayState && d.hasConverted && d.hasFinal && bytes.Equal(d.converted.CodeHash, d.final.CodeHash)
	if d.hasConverted {
		if err := updateAccount(a.converted, d.job.hash, d.converted); err != nil {
			return err
		}
		r.convertedCounts.Accounts++
		r.convertedCounts.StorageSlots += d.convertedSlots
		if !bytes.Equal(d.converted.CodeHash, types.EmptyCodeHash[:]) {
			use := convertedCode
			if a.mode == convertState || sharedCode {
				use |= finalCode
			}
			r.codes.Add(common.BytesToHash(d.converted.CodeHash), use)
		}
	}
	if a.mode == overlayState && d.hasFinal {
		if err := updateAccount(a.final, d.job.hash, d.final); err != nil {
			return err
		}
		r.counts.Accounts++
		r.counts.StorageSlots += d.finalSlots
		if !sharedCode && !bytes.Equal(d.final.CodeHash, types.EmptyCodeHash[:]) {
			r.codes.Add(common.BytesToHash(d.final.CodeHash), finalCode)
		}
	}
	return nil
}
func (s *source) accountJobs(ctx context.Context, l limiter, converted, alloc map[common.Hash]*accountPatch, plan walkPlan, submit func(accountJob) error, ack func() error) error {
	patchKeys := make(map[common.Hash]struct{})
	for h, p := range converted {
		if p.active() {
			patchKeys[h] = struct{}{}
		}
	}
	for h, p := range alloc {
		if p.active() {
			patchKeys[h] = struct{}{}
		}
	}
	keys := sortedHashes(patchKeys)
	offset := 0
	makeJob := func(h common.Hash, a *types.StateAccount, owned *memoryAccount) error {
		return submit(accountJob{hash: h, original: a, converted: converted[h], final: alloc[h], memory: owned})
	}
	visit := func(h common.Hash, a *types.StateAccount, owned *memoryAccount) error {
		for offset < len(keys) && bytes.Compare(keys[offset][:], h[:]) < 0 {
			h := keys[offset]
			offset++
			if err := makeJob(h, nil, nil); err != nil {
				return err
			}
		}
		if offset < len(keys) && keys[offset] == h {
			offset++
		}
		return makeJob(h, a, owned)
	}
	var err error
	if plan.consume {
		err = s.memory.accounts.Drain(func(h common.Hash, a *memoryAccount) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return visit(h, s.memory.account(a), a)
		}, ack)
	} else {
		err = s.eachAccount(ctx, l, plan, func(h common.Hash, a *types.StateAccount) error { return visit(h, a, nil) })
	}
	if err != nil {
		return err
	}
	for offset < len(keys) {
		h := keys[offset]
		offset++
		if err := makeJob(h, nil, nil); err != nil {
			return err
		}
	}
	return nil
}
func (s *source) validateAccountStorage(ctx context.Context, job accountJob, l limiter, capture bool, emit func(accountEvent) error) error {
	stack := trie.NewStackTrie(nil)
	if err := s.storage(ctx, job.hash, job.original.Root, l, func(k, v common.Hash) error {
		if err := storageword.Update(stack, k, v); err != nil {
			return err
		}
		if capture {
			return emit(accountEvent{record: &accountRecord{Kind: statefile.StorageEntry, Storage: statefile.Storage{Key: statefile.StorageKeyHash(k), Value: v}}})
		}
		return nil
	}); err != nil {
		return err
	}
	if stack.Hash() != job.original.Root {
		return fmt.Errorf("source storage root mismatch: expected %s, actual %s", job.original.Root, stack.Hash())
	}
	return emit(accountEvent{done: &accountDone{job: job}})
}

func (s *source) processAccount(ctx context.Context, job accountJob, l limiter, x *indexes, plan walkPlan, emit func(accountEvent) error) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = fmt.Errorf("account %s during %s: %w", job.hash, plan.mode, retErr)
		}
	}()
	mode := plan.mode
	var address common.Address
	if plan.policy == rawSource {
		var err error
		address, err = s.dumpAddress(ctx, l, job.hash)
		if err != nil {
			return err
		}
	}
	if mode == validateSource {
		return s.validateAccountStorage(ctx, job, l, plan.capture != nil, emit)
	}
	original := job.original
	if original == nil {
		original = emptyAccount()
	}
	converted := patchAccount(original, job.converted)
	final := converted
	if mode == overlayState {
		final = patchAccount(converted, job.final)
	}
	d := &accountDone{job: job, converted: converted, final: final, hasConverted: job.original != nil || (job.converted != nil && job.converted.active())}
	d.hasFinal = d.hasConverted || (job.final != nil && job.final.active())
	if plan.policy == rawSource {
		if err := emitAddressStart(address, final, emit); err != nil {
			return err
		}
	} else if err := emitAccountStart(job, x, final, emit); err != nil {
		return err
	}
	var cp, fp map[common.Hash]common.Hash
	if job.converted != nil {
		cp = job.converted.Storage
	}
	if job.final != nil {
		fp = job.final.Storage
	}
	sourceStack, convertedStack, finalStack := storageStacks(plan.reuseSource, len(cp) != 0, len(fp) != 0)
	finalMerge := newSlotMerger(fp, func(k, v common.Hash) error {
		if err := updateDistinct(finalStack, convertedStack, k, v); err != nil {
			return err
		}
		d.finalSlots++
		return emit(accountEvent{record: &accountRecord{Kind: statefile.StorageEntry, Storage: statefile.Storage{Key: statefile.StorageKeyHash(k), Value: v}}})
	})
	convertedMerge := newSlotMerger(cp, func(k, v common.Hash) error {
		if err := updateDistinct(convertedStack, sourceStack, k, v); err != nil {
			return err
		}
		d.convertedSlots++
		return finalMerge.accept(k, v)
	})
	err := s.accountStorage(ctx, job, original.Root, l, plan, emit, func(k, v common.Hash) error {
		if err := updateDistinct(sourceStack, nil, k, v); err != nil {
			return err
		}
		return convertedMerge.accept(k, v)
	})
	if err != nil {
		return err
	}
	if err := convertedMerge.finish(); err != nil {
		return err
	}
	if err := finalMerge.finish(); err != nil {
		return err
	}
	if sourceStack != nil && sourceStack.Hash() != original.Root {
		return fmt.Errorf("source storage root mismatch: expected %s, actual %s", original.Root, sourceStack.Hash())
	}
	converted.Root = storageRoot(convertedStack, original.Root)
	final.Root = storageRoot(finalStack, converted.Root)
	if err := emit(accountEvent{record: &accountRecord{Kind: statefile.AccountEnd, StorageRoot: final.Root, StorageCount: d.finalSlots}}); err != nil {
		return err
	}
	return emit(accountEvent{done: d})
}

func (s *source) produceAccounts(ctx context.Context, failure *runFailure, l limiter, x *indexes, converted, alloc map[common.Hash]*accountPatch, plan walkPlan, window chan struct{}, queue chan accountStream, producer chan error, jobs *sync.WaitGroup) {
	defer close(queue)
	var loader *codeLoader
	if !plan.reuseSource {
		store := newCodeStore(s.codeCache == "memory")
		if plan.capture != nil {
			store = plan.capture.codeStore
		}
		s.codes = store
		loader = newCodeLoader(ctx, s, store, l, failure, jobs)
		defer close(loader.queue)
	}
	var lastAck <-chan error
	err := s.accountJobs(ctx, l, converted, alloc, plan, func(job accountJob) error {
		select {
		case window <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		var loaded *codeLoad
		if loader != nil && job.original != nil {
			var err error
			loaded, err = loader.submit(ctx, common.BytesToHash(job.original.CodeHash))
			if err != nil {
				return err
			}
		}
		stream := accountStream{hash: job.hash, events: make(chan accountEvent, streamAhead), result: make(chan error, 1)}
		if plan.consume {
			stream.ack = make(chan error, 1)
			lastAck = stream.ack
		}
		jobs.Go(func() {
			defer close(stream.events)
			err := s.processAccount(ctx, job, l, x, plan, func(e accountEvent) error {
				if e.done != nil {
					if err := loaded.wait(ctx); err != nil {
						return err
					}
				}
				select {
				case stream.events <- e:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			failure.record(err)
			stream.result <- err
		})
		select {
		case queue <- stream:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}, func() error { return waitOutput(ctx, lastAck) })
	failure.record(err)
	producer <- err
}

func emitAccountStart(job accountJob, x *indexes, final *types.StateAccount, emit func(accountEvent) error) error {
	a, ok := x.addresses[job.hash]
	if !ok && job.original == nil {
		if job.converted != nil {
			a, ok = job.converted.Address, true
		} else if job.final != nil {
			a, ok = job.final.Address, true
		}
	}
	if !ok {
		return fmt.Errorf("missing account address preimage %s", job.hash)
	}
	return emitAddressStart(a, final, emit)
}

func emitAddressStart(address common.Address, final *types.StateAccount, emit func(accountEvent) error) error {
	return emit(accountEvent{record: &accountRecord{Kind: statefile.AccountStart, Account: statefile.Account{Address: address, CodeHash: common.BytesToHash(final.CodeHash), Balance: final.Balance.ToBig(), Nonce: final.Nonce}}})
}

func validateSourceAccount(v leaf, policy sourcePolicy) (*types.StateAccount, error) {
	a, err := decodeAccount(v.value)
	if err != nil {
		return nil, fmt.Errorf("account %s: decode source: %w", v.key, err)
	}
	if policy == ovmSource && !a.Balance.IsZero() {
		return nil, fmt.Errorf("account %s: OVM conversion requires zero source native balances", v.key)
	}
	return a, nil
}

// Nil stacks reuse previously validated roots. Changed stages still compute
// their own roots; Writer independently hashes every final storage record.
func storageStacks(reuseSource, convertedChanged, finalChanged bool) (source, converted, final *trie.StackTrie) {
	if !reuseSource {
		source = trie.NewStackTrie(nil)
	}
	converted = source
	if convertedChanged {
		converted = trie.NewStackTrie(nil)
	}
	final = converted
	if finalChanged {
		final = trie.NewStackTrie(nil)
	}
	return
}
func storageRoot(t *trie.StackTrie, validated common.Hash) common.Hash {
	if t == nil {
		return validated
	}
	return t.Hash()
}
func updateDistinct(t, previous *trie.StackTrie, k, v common.Hash) error {
	if t == nil || t == previous {
		return nil
	}
	return storageword.Update(t, k, v)
}

func validateWalkPolicy(policy sourcePolicy, x *indexes, converted, alloc map[common.Hash]*accountPatch) error {
	if policy != ovmSource && policy != rawSource {
		return errors.New("invalid source policy")
	}
	if policy == rawSource && (len(converted) != 0 || len(alloc) != 0) {
		return errors.New("dump cannot apply patches")
	}
	if x != nil && x.validation != nil && x.validation.policy != policy {
		return errors.New("source validation belongs to a different policy")
	}

	return nil
}

// Frozen snapshots bypass source trie reads and account RLP decoding. The
// original streaming path remains for dump and independent migration oracles.
func (s *source) eachAccount(ctx context.Context, l limiter, plan walkPlan, visit func(common.Hash, *types.StateAccount) error) error {
	if s.memory != nil {
		if !plan.reuseSource {
			return errors.New("snapshot requires source validation")
		}
		return s.memory.accounts.Visit(func(h common.Hash, a *memoryAccount) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			return visit(h, s.memory.account(a))
		})
	}
	return s.partitionStream(ctx, trie.StateTrieID(s.head.StateRoot), l, func(v leaf) error {
		a, err := validateSourceAccount(v, plan.policy)
		if err != nil {
			return err
		}
		return visit(v.key, a)
	})
}

func (m walkMode) String() string {
	switch m {
	case validateSource:
		return "source validation"
	case convertState:
		return "state output"
	case overlayState:
		return "overlay output"
	default:
		return "invalid stage"
	}
}

// A consuming worker owns its account directly; it never looks up a tree being
// drained by the scheduler. A leaf's barrier follows every resulting output
// record, so a slow or failed Writer cannot release unacknowledged storage.
func (s *source) accountStorage(ctx context.Context, job accountJob, root common.Hash, l limiter, plan walkPlan, emit func(accountEvent) error, visit func(common.Hash, common.Hash) error) error {
	if !plan.consume {
		return s.storage(ctx, job.hash, root, l, visit)
	}
	if job.memory == nil {
		return nil
	} // alloc-only account
	if job.memory.storageRoot() != root {
		return errors.New("consumed storage identity mismatch")
	}
	return job.memory.storage.Drain(func(k, v common.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return visit(k, v)
	}, func() error {
		ack := make(chan error, 1)
		if err := emit(accountEvent{done: &accountDone{ack: ack}}); err != nil {
			return err
		}
		return waitOutput(ctx, ack)
	})
}
func waitOutput(ctx context.Context, ack <-chan error) error {
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
