package migration

// This pre-map conversion is retained only as a byte-parity test oracle.
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/metis-devops/metis-l2state/statefile"
)

func (s *source) convertLegacy(ctx context.Context, x *indexes, in *inputs, workers int, l limiter) (map[common.Hash]*accountPatch, statefile.Conversion, error) {
	patches := make(map[common.Hash]*accountPatch)
	var evidence statefile.Conversion
	r := accountReader{s: s}
	ovm, err := r.get(ovmHash)
	if err != nil {
		return nil, evidence, err
	}
	if bytes.Equal(ovm.CodeHash, types.EmptyCodeHash[:]) {
		return nil, evidence, errors.New("OVM_ETH has no code")
	}
	if err := s.validateRetain(ctx, x, in, &r); err != nil {
		return nil, evidence, err
	}
	metadata, err := s.metadataSlots(ctx, ovm.Root)
	if err != nil {
		return nil, evidence, err
	}
	supplyWord, err := s.storageValue(ovmHash, ovm.Root, common.HexToHash("0x02"))
	if err != nil {
		return nil, evidence, err
	}
	supply := new(uint256.Int).SetBytes(supplyWord[:])
	jobs, err := s.balanceJobs(ctx, x, ovm.Root, metadata, l)
	if err != nil {
		return nil, evidence, err
	}
	keys := sortedHashes(jobs)
	if err := s.classifyBalances(ctx, x, in, jobs, keys, workers, l); err != nil {
		return nil, evidence, err
	}
	total, migrated, remaining, self := new(uint256.Int), new(uint256.Int), new(uint256.Int), new(uint256.Int)
	storage := make(map[common.Hash]common.Hash)
	for _, h := range keys {
		if err := ctx.Err(); err != nil {
			return nil, evidence, err
		}
		j := jobs[h]
		if _, overflow := total.AddOverflow(total, &j.value); overflow {
			return nil, evidence, errors.New("balance sum overflows uint256")
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
		patches[h] = &accountPatch{Address: j.address, Balance: new(uint256.Int).Set(&j.value)}
		storage[j.slot] = common.Hash{}
	}
	if in.audit == nil {
		if !total.Eq(supply) {
			return nil, evidence, fmt.Errorf("OVM totalSupply mismatch: balances %s supply %s", total, supply)
		}
	} else {
		if err := in.audit.matchAccounting(s.head.BlockNumber, common.BytesToHash(ovm.CodeHash), uint64(len(jobs)), total, supply); err != nil {
			return nil, evidence, err
		}
		evidence.AuditHeight = in.audit.Identity.To
		evidence.AuditHash = in.audit.Identity.ToHash
	}
	if !new(uint256.Int).Add(migrated, remaining).Eq(total) {
		return nil, evidence, errors.New("conversion conservation mismatch")
	}
	supplySlot := common.HexToHash("0x02")
	storage[crypto.Keccak256Hash(supplySlot[:])] = common.Hash(remaining.Bytes32())
	runtime := in.runtime
	patches[ovmHash] = &accountPatch{Address: ovmAddress, Balance: remaining, Code: &runtime, Storage: storage}
	evidence.OriginalSupply = supply.Dec()
	evidence.BalanceSum = total.Dec()
	evidence.MigratedNative = migrated.Dec()
	evidence.RemainingSupply = remaining.Dec()
	evidence.SelfBalance = self.Dec()
	return patches, evidence, nil
}
