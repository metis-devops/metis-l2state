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
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
)

type balanceJob struct {
	address          common.Address
	slot             common.Hash
	value            uint256.Int
	contract, retain bool
}

func (s *source) convert(ctx context.Context, x *indexes, in *inputs, workers int, l limiter) (statefile.Conversion, error) {
	var evidence statefile.Conversion
	m, err := s.beginConversion(x)
	if err != nil {
		return evidence, err
	}
	defer func() {
		if m.phase != memoryFrozen {
			m.phase = memoryFailed
		}
	}()
	r := accountReader{s: s}
	ovm, err := r.get(ovmHash)
	if err != nil {
		return evidence, err
	}
	if bytes.Equal(ovm.CodeHash, types.EmptyCodeHash[:]) {
		return evidence, errors.New("OVM_ETH has no code")
	}
	if err := s.validateRetain(ctx, x, in, &r); err != nil {
		return evidence, err
	}
	metadata, err := s.metadataSlots(ctx, ovm.Root)
	if err != nil {
		return evidence, err
	}
	supplyWord, err := s.storageValue(ovmHash, ovm.Root, common.HexToHash("0x02"))
	if err != nil {
		return evidence, err
	}
	supply := new(uint256.Int).SetBytes(supplyWord[:])
	jobs, err := s.balanceJobs(ctx, x, ovm.Root, metadata, l)
	if err != nil {
		return evidence, err
	}
	keys := sortedHashes(jobs)
	if err := s.classifyBalances(ctx, x, in, jobs, keys, workers, l); err != nil {
		return evidence, err
	}
	total, migrated, remaining, self := new(uint256.Int), new(uint256.Int), new(uint256.Int), new(uint256.Int)
	for _, h := range keys {
		if err := ctx.Err(); err != nil {
			return evidence, err
		}
		j := jobs[h]
		if _, overflow := total.AddOverflow(total, &j.value); overflow {
			return evidence, errors.New("balance sum overflows uint256")
		}
		if j.address == ovmAddress {
			self.Set(&j.value)
			remaining.Add(remaining, &j.value)
			continue
		}
		if j.contract && j.retain {
			remaining.Add(remaining, &j.value)
			evidence.RetainedContracts++
			continue
		}
		if j.contract {
			evidence.ConvertedContracts++
		} else {
			evidence.EOAs++
		}
		migrated.Add(migrated, &j.value)
	}
	if in.audit == nil {
		if !total.Eq(supply) {
			return evidence, fmt.Errorf("OVM totalSupply mismatch: balances %s supply %s", total, supply)
		}
	} else {
		if err := in.audit.matchAccounting(s.head.BlockNumber, common.BytesToHash(ovm.CodeHash), uint64(len(jobs)), total, supply); err != nil {
			return evidence, err
		}
		evidence.AuditHeight = in.audit.Identity.To
		evidence.AuditHash = in.audit.Identity.ToHash
	}
	if !new(uint256.Int).Add(migrated, remaining).Eq(total) {
		return evidence, errors.New("conversion conservation mismatch")
	}
	// All source-policy and accounting checks precede the first mutation.
	if err := m.applyConversion(ctx, jobs, keys, remaining, in.runtime); err != nil {
		return evidence, err
	}
	evidence.OriginalSupply = supply.Dec()
	evidence.BalanceSum = total.Dec()
	evidence.MigratedNative = migrated.Dec()
	evidence.RemainingSupply = remaining.Dec()
	evidence.SelfBalance = self.Dec()
	return evidence, nil
}

func (s *source) beginConversion(x *indexes) (*stateSnapshot, error) {
	if s.memory == nil || x == nil || x.validation == nil || x.validation.source != s || x.validation.head != s.head || x.validation.policy != ovmSource {
		return nil, errors.New("conversion requires source validation and memory state")
	}
	m := s.memory
	if m.phase != memoryReady {
		return nil, errors.New("memory state is not ready for conversion")
	}
	m.phase = memoryConverting
	return m, nil
}

func (m *stateSnapshot) applyConversion(ctx context.Context, jobs map[common.Hash]*balanceJob, keys []common.Hash, remaining *uint256.Int, runtime []byte) error {
	ovm := m.accounts.Value(ovmHash)
	for _, h := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		j := jobs[h]
		if j.address == ovmAddress || (j.contract && j.retain) {
			continue
		}
		a := m.accounts.Value(h)
		if a == nil {
			a = &memoryAccount{}
			m.accounts.Set(h, a)
		}
		a.setBalance(&j.value)
		ovm.storage.Delete(j.slot)
	}
	ovm.setBalance(remaining)
	supplySlot := common.HexToHash("0x02")
	ovm.setStorage(crypto.Keccak256Hash(supplySlot[:]), common.Hash(remaining.Bytes32()))
	code := crypto.Keccak256Hash(runtime)
	ovm.code = &code
	return m.finishConversion(ctx)
}

func (s *source) classifyBalances(ctx context.Context, x *indexes, in *inputs, jobs map[common.Hash]*balanceJob, keys []common.Hash, workers int, l limiter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan common.Hash, 2*workers)
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for range workers {
		wg.Go(func() {
			r := accountReader{s: s}
			for h := range queue {
				err := l.run(ctx, func() error {
					a, err := r.get(h)
					if err != nil {
						return err
					}
					j := jobs[h]
					j.contract = !bytes.Equal(a.CodeHash, types.EmptyCodeHash[:])
					if j.contract {
						_, automatic := x.members[j.address]
						_, manual := in.retain[j.address]
						j.retain = automatic || manual
					}
					return nil
				})
				if err != nil {
					once.Do(func() { first = err; cancel() })
					return
				}
			}
		})
	}
send:
	for _, h := range keys {
		select {
		case queue <- h:
		case <-ctx.Done():
			break send
		}
	}
	close(queue)
	wg.Wait()
	if first != nil {
		return first
	}
	return ctx.Err()
}

func (s *source) validateRetain(ctx context.Context, x *indexes, in *inputs, r *accountReader) error {
	for a := range in.retain {
		if err := ctx.Err(); err != nil {
			return err
		}
		h := crypto.Keccak256Hash(a[:])
		// Retention selects policy for an identified source account. Operator
		// input must not repair missing source/node-index identity evidence.
		if known, ok := x.addresses[h]; !ok || known != a {
			return fmt.Errorf("retain address %s has no source address preimage", a)
		}
		account, err := r.get(h)
		if err != nil {
			return err
		}
		if bytes.Equal(account.CodeHash, types.EmptyCodeHash[:]) {
			return fmt.Errorf("retain address %s is not a source contract", a)
		}
	}
	return nil
}

func (s *source) balanceJobs(ctx context.Context, x *indexes, root common.Hash, metadata metadataLayout, l limiter) (map[common.Hash]*balanceJob, error) {
	jobs := make(map[common.Hash]*balanceJob)
	err := s.storage(ctx, ovmHash, root, l, func(k, v common.Hash) error {
		address, balance := x.balances[k]
		_, allowance := x.allowances[k]
		meta, err := metadata.contains(ctx, k)
		if err != nil {
			return err
		}
		found := 0
		for _, ok := range []bool{balance, allowance, meta} {
			if ok {
				found++
			}
		}
		if found != 1 {
			return fmt.Errorf("OVM storage %s ownership is unknown or ambiguous", k)
		}
		if balance {
			h := crypto.Keccak256Hash(address[:])
			jobs[h] = &balanceJob{address: address, slot: k, value: *new(uint256.Int).SetBytes(v[:])}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}
