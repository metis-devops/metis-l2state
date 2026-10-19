package migration

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/metis-devops/metis-l2state/internal/ovmevents"
	"github.com/metis-devops/metis-l2state/statefile"
)

// Historical streaming validation/recomputation entrypoints are test oracles.
func (s *source) prepare(ctx context.Context, workers int, l limiter, events *ovmevents.Validated) (*indexes, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var x *indexes
	var indexErr, stateErr error
	wg.Go(func() {
		x, indexErr = s.loadIndexes(ctx, l, events)
		if indexErr != nil {
			cancel()
		}
	})
	wg.Go(func() {
		_, stateErr = s.walk(ctx, workers, l, nil, nil, nil, nil)
		if stateErr != nil {
			cancel()
		}
	})
	wg.Wait()
	if err := errors.Join(indexErr, stateErr); err != nil {
		return nil, err
	}
	x.validation = &sourceValidation{source: s, head: s.head}
	return x, nil
}
func (s *source) planWalk(mode walkMode, x *indexes) (walkPlan, error) {
	p := walkPlan{mode: mode}
	if mode == validateSource || x == nil || x.validation == nil {
		return p, nil
	}
	if x.validation.source != s || x.validation.head != s.head {
		return p, errors.New("source validation belongs to a different source or head")
	}
	p.reuseSource = true
	return p, nil
}

// Admission follows source hash order. At most 2*workers accounts have storage
// queues, so slow early accounts cannot be starved by later partitions.
func (s *source) walk(ctx context.Context, workers int, l limiter, x *indexes, converted, alloc map[common.Hash]*accountPatch, write func(statefile.Record) error) (walkResult, error) {
	return s.walkWithPolicy(ctx, workers, l, x, converted, alloc, write, ovmSource)
}

func (s *source) walkWithPolicy(ctx context.Context, workers int, l limiter, x *indexes, converted, alloc map[common.Hash]*accountPatch, write func(statefile.Record) error, policy sourcePolicy) (walkResult, error) {
	return s.walkCapture(ctx, workers, l, x, converted, alloc, write, policy, nil)
}

func (s *source) walkCapture(ctx context.Context, workers int, l limiter, x *indexes, converted, alloc map[common.Hash]*accountPatch, write func(statefile.Record) error, policy sourcePolicy, capture *stateSnapshot) (walkResult, error) {
	if capture != nil && (write != nil || policy != ovmSource) {
		return walkResult{}, errors.New("snapshot capture requires OVM source validation")
	}
	if err := validateWalkPolicy(policy, x, converted, alloc); err != nil {
		return walkResult{}, err
	}

	mode := convertState
	if write == nil {
		if len(converted) != 0 || len(alloc) != 0 {
			return walkResult{}, errors.New("source validation cannot apply patches")
		}
		mode = validateSource
	} else if len(alloc) != 0 {
		mode = overlayState
	}
	plan, err := s.planWalk(mode, x)
	if err != nil {
		return walkResult{}, err
	}
	plan.policy = policy
	plan.capture = capture
	return s.runWalk(ctx, workers, l, walkRequest{plan: plan, indexes: x, converted: converted, alloc: alloc, write: write})
}
func slotRLP(v common.Hash) []byte { b, _ := rlp.EncodeToBytes(bytes.TrimLeft(v[:], "\x00")); return b }
func (f fixture) validatedEvents(t testing.TB) *ovmevents.Validated {
	t.Helper()
	v, _, err := ovmevents.ReadValidatedFile(t.Context(), f.eventPath())
	must(t, err)
	return v
}
