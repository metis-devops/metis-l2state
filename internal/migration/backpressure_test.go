package migration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
)

// Hold the ordered consumer while a large account fills its output queue.
// All iterator producers must release the shared permits even under backpressure.
func TestFullAccountQueueReleasesWorkers(t *testing.T) {
	for _, mode := range []string{"streaming", "snapshot", "dump"} {
		t.Run(mode, func(t *testing.T) { testFullAccountQueue(t, mode) })
	}
}
func testFullAccountQueue(t *testing.T, mode string) {
	f := newFixture(t, 100000, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	l := make(limiter, 2)
	var x *indexes
	switch mode {
	case "dump":
		// Raw dump resolves addresses per admitted account without indexes.
	case "snapshot":
		x, err = s.prepareSnapshot(t.Context(), 2, l, f.validatedEvents(t))
	default:
		x, err = s.prepare(t.Context(), 2, l, f.validatedEvents(t))
	}
	must(t, err)
	// Give the backpressure checks their full timeout after source validation.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	window := make(chan struct{}, 4)
	queue := make(chan accountStream, 4)
	producer := make(chan error, 1)
	failure := &runFailure{cancel: cancel}
	var jobs sync.WaitGroup
	plan, err := s.planWalk(convertState, x)
	must(t, err)
	if mode == "dump" {
		plan.policy = rawSource
	}
	go s.produceAccounts(ctx, failure, l, x, nil, nil, plan, window, queue, producer, &jobs)
	var streams []accountStream
	for stream := range queue {
		streams = append(streams, stream)
	}
	producerErr := <-producer
	defer func() {
		cancel()
		jobs.Wait()
	}()
	must(t, producerErr)
	if len(streams) != 4 || len(window) != 4 {
		t.Fatal("account admission window changed")
	}
	// Wait for an actually full queue, not a timing assumption about producers.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		full := false
		for _, stream := range streams {
			if cap(stream.events) != streamAhead {
				t.Fatal("unbounded account queue")
			}
			full = full || len(stream.events) == streamAhead
		}
		if full && len(l) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("full queue held workers or failed to fill")
		case <-ticker.C:
		}
	}
	// A blocked consumer cannot prevent unrelated source work from obtaining both permits.
	for range cap(l) {
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("worker permit retained by blocked output")
		}
	}
	for range cap(l) {
		<-l
	}
	cancel()
	jobs.Wait()
	for _, stream := range streams {
		for range stream.events {
		}
		err := <-stream.result
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if len(l) != 0 {
		t.Fatal("leaked worker permit")
	}
}

func TestWalkBeyondAdmissionWindow(t *testing.T) {
	f := newFixture(t, 10000, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	alloc := make(map[common.Hash]*accountPatch)
	for n := range 64 {
		a := common.BigToAddress(big.NewInt(int64(0x1000 + n)))
		alloc[crypto.Keccak256Hash(a[:])] = &accountPatch{Address: a, Balance: new(uint256.Int)}
	}
	for _, workers := range []int{2, 4} {
		for _, mode := range []string{"complete", "cancel", "error"} {
			t.Run(fmt.Sprintf("%d/%s", workers, mode), func(t *testing.T) {
				testBlockedWalk(t, f, s, alloc, workers, mode, false)
			})
		}
	}
}

func testBlockedWalk(t *testing.T, f fixture, s *source, alloc map[common.Hash]*accountPatch, workers int, mode string, snapshot bool) {
	t.Helper()
	l := make(limiter, workers)
	prepare := s.prepare
	if snapshot {
		prepare = s.prepareSnapshot
	}
	x, err := prepare(t.Context(), workers, l, f.validatedEvents(t))
	must(t, err)
	expectedAccounts := uint64(68)
	if snapshot {
		in, err := loadInputs(t.Context(), f.options("unused"))
		must(t, err)
		_, err = s.convert(t.Context(), x, in, workers, l)
		must(t, err)
		expectedAccounts++ // Conversion creates the missing EOA.
	}

	// Source validation is setup, not part of the blocked-consumer deadline.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	sentinel := errors.New("blocked consumer failed")
	run := func(write func(statefile.Record) error) (walkResult, error) {
		if snapshot {
			return s.writeMigration(ctx, workers, l, x, alloc, write)
		}
		return s.walk(ctx, workers, l, x, nil, alloc, write)
	}

	go func() {
		first := true
		var last common.Hash
		count := uint64(0)
		result, err := run(func(r statefile.Record) error {
			if first {
				first = false
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
				if mode == "error" {
					return sentinel
				}
			}
			if r.Kind == statefile.AccountStart {
				h := crypto.Keccak256Hash(r.Account.Address[:])
				if count > 0 && bytes.Compare(last[:], h[:]) >= 0 {
					return errors.New("account output reordered across admission windows")
				}
				last, count = h, count+1
			}
			return nil
		})
		if err == nil && (count != expectedAccounts || result.counts.Accounts != count || result.sourceRoot != s.head.StateRoot) {
			err = errors.New("incomplete multi-window output")
		}
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("walk stopped before first output: %v", err)
	case <-ctx.Done():
		t.Fatal("walk never produced first account")
	}
	// Source work must remain schedulable while the earliest output is blocked.
	for range cap(l) {
		select {
		case l <- struct{}{}:
		case <-ctx.Done():
			t.Fatal("blocked output retained worker permits")
		}
	}
	for range cap(l) {
		<-l
	}
	if mode == "cancel" {
		cancel()
	}
	close(release)
	select {
	case err := <-done:
		switch mode {
		case "complete":
			must(t, err)
		case "error":
			if !errors.Is(err, sentinel) {
				t.Fatalf("lost consumer error: %v", err)
			}
		case "cancel":
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("walk did not join its producers")
	}
	if len(l) != 0 {
		t.Fatal("leaked worker permits")
	}
}

func TestSlowConsumerAndWriteFailure(t *testing.T) {
	f := newFixture(t, 10000, 0)
	s, err := openSource(f.source, 16, 16)
	must(t, err)
	defer func() { must(t, s.close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	l := make(limiter, 2)
	x, err := s.loadIndexes(ctx, l, f.validatedEvents(t))
	must(t, err)
	sentinel := errors.New("output failed")
	for _, fail := range []bool{false, true} {
		calls := 0
		_, err := s.walk(ctx, 2, l, x, nil, nil, func(statefile.Record) error {
			calls++
			if calls == 1 {
				// Later account queues fill while the first output is delayed.
				time.Sleep(20 * time.Millisecond)
			}
			if fail && calls == 70 {
				return sentinel
			}
			return nil
		})
		if fail {
			if !errors.Is(err, sentinel) {
				t.Fatalf("lost original write failure: %v", err)
			}
		} else {
			must(t, err)
		}
		if len(l) != 0 {
			t.Fatal("walk returned with worker permits held")
		}
	}
}

// Account queues retain the original record-count and channel-element bounds.
// Even if every element carried a record, queued payload bytes cannot increase.
func TestAccountQueueByteBound(t *testing.T) {
	const oldElement = 2 * unsafe.Sizeof(uintptr(0))
	if unsafe.Sizeof(accountEvent{}) != oldElement {
		t.Fatal("account channel element grew")
	}
	if unsafe.Sizeof(accountRecord{}) > unsafe.Sizeof(statefile.Record{}) {
		t.Fatal("queued account record exceeds original payload bound")
	}
}

func TestSnapshotWalkBeyondAdmissionWindow(t *testing.T) {
	f := newFixture(t, 10000, 0)
	alloc := make(map[common.Hash]*accountPatch)
	for n := range 64 {
		a := common.BigToAddress(big.NewInt(int64(0x1000 + n)))
		alloc[crypto.Keccak256Hash(a[:])] = &accountPatch{Address: a, Balance: new(uint256.Int)}
	}
	for _, workers := range []int{2, 4} {
		for _, mode := range []string{"complete", "cancel", "error"} {
			t.Run(fmt.Sprintf("%d/%s", workers, mode), func(t *testing.T) {
				s, err := openSource(f.source, 16, 16)
				must(t, err)
				defer func() { must(t, s.close()) }()
				testBlockedWalk(t, f, s, alloc, workers, mode, true)
			})
		}
	}
}
