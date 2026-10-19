package migration

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/metis-devops/metis-l2state/statefile"
)

func frozenOutputFixture(t *testing.T) (*source, *indexes) {
	t.Helper()
	m := newStateSnapshot()
	address := common.HexToAddress("0x1234")
	hash := crypto.Keccak256Hash(address[:])
	must(t, m.accounts.Append(hash, &memoryAccount{}))
	m.phase = memoryFrozen
	s := &source{memory: m}
	x := &indexes{addresses: map[common.Hash]common.Address{hash: address}}
	x.validation = &sourceValidation{source: s, head: s.head, policy: ovmSource}
	return s, x
}

func awaitOutputSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("output did not finish while its sibling Writer remained blocked")
	}
}

func TestConcurrentOutputRejectedWithoutWaiting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "owner-completes", true: "owner-fails"}[fail], func(t *testing.T) {
			s, x := frozenOutputFixture(t)
			entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseOwner := sync.OnceFunc(func() { close(release) })
			var ownerErr error
			sentinel := errors.New("owner Writer failed")
			go func() {
				defer close(ownerDone)
				_, ownerErr = s.writeMigration(t.Context(), 2, make(limiter, 2), x, nil, func(r statefile.Record) error {
					if r.Kind == statefile.AccountStart {
						close(entered)
						<-release
						if fail {
							return sentinel
						}
					}
					return nil
				})
			}()
			t.Cleanup(func() {
				releaseOwner()
				awaitOutputSignal(t, ownerDone)
			})
			awaitOutputSignal(t, entered)
			checkOutputContenders(t, s, x)
			releaseOwner()
			awaitOutputSignal(t, ownerDone)
			if fail && !errors.Is(ownerErr, sentinel) || !fail && ownerErr != nil {
				t.Errorf("contender changed owner's result: %v", ownerErr)
			}
			want := memoryConsumed
			if fail {
				want = memoryFailed
			}
			if s.memory.phase != want || s.memory.accounts.root != nil {
				t.Error("owner failed to finish consuming state")
			}
		})
	}
}

func checkOutputContenders(t *testing.T, s *source, x *indexes) {
	t.Helper()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	for _, ctx := range []context.Context{t.Context(), canceled, expired} {
		done := make(chan struct{})
		var err error
		wrote := false
		go func() {
			defer close(done)
			_, err = s.writeMigration(ctx, 2, make(limiter, 2), x, nil, func(statefile.Record) error { wrote = true; return nil })
		}()
		awaitOutputSignal(t, done)
		if err == nil || wrote {
			t.Fatal("concurrent output was admitted")
		}
		if ctx.Err() != nil && !errors.Is(err, ctx.Err()) {
			t.Fatalf("lost contender context error: %v", err)
		}
	}
}

func TestCanceledOutputAdmissionPreservesState(t *testing.T) {
	s, x := frozenOutputFixture(t)
	root := s.memory.accounts.root
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.writeMigration(ctx, 2, make(limiter, 2), x, nil, func(statefile.Record) error {
		t.Fatal("canceled admission wrote output")
		return nil
	})
	if !errors.Is(err, context.Canceled) || s.memory.phase != memoryFrozen || s.memory.accounts.root != root {
		t.Fatalf("canceled admission consumed state: %v", err)
	}
	_, err = s.writeMigration(t.Context(), 2, make(limiter, 2), x, nil, func(statefile.Record) error { return nil })
	must(t, err)
	if s.memory.phase != memoryConsumed || s.memory.accounts.root != nil {
		t.Fatal("valid output could not consume state after canceled admission")
	}
}
