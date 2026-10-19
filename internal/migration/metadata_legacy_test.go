package migration

// Frozen pre-refactor expansion/classification oracle (27c3d879). Keep this
// independent of compact range decoding and bounded matching in production.
import (
	"context"
	"errors"
	"fmt"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"math/big"
)

func (s *source) legacyMetadataSlots(ctx context.Context, root common.Hash) (map[common.Hash]struct{}, error) {
	result := make(map[common.Hash]struct{})
	for n := uint64(0); n <= 6; n++ {
		slot := common.BigToHash(new(big.Int).SetUint64(n))
		v, err := s.storageValue(ovmHash, root, slot)
		if err != nil {
			return nil, err
		}
		if n < 2 {
			if v != (common.Hash{}) {
				return nil, errors.New("nonzero OVM mapping base slot")
			}
			continue
		}
		result[crypto.Keccak256Hash(slot[:])] = struct{}{}
		if n == 3 || n == 4 {
			if err := legacyRecognizeString(ctx, slot, v, result); err != nil {
				return nil, err
			}
		}
	}
	return result, nil
}
func legacyRecognizeString(ctx context.Context, slot, word common.Hash, result map[common.Hash]struct{}) error {
	if word[31]&1 == 0 {
		length := int(word[31] / 2)
		if length > 31 {
			return errors.New("invalid short OVM string")
		}
		for _, b := range word[length:31] {
			if b != 0 {
				return errors.New("noncanonical OVM string padding")
			}
		}
		return nil
	}
	length := new(uint256.Int).Rsh(new(uint256.Int).SetBytes(word[:]), 1)
	if !length.IsUint64() || length.Uint64() < 32 {
		return errors.New("invalid long OVM string")
	}
	base := new(uint256.Int).SetBytes(crypto.Keccak256(slot[:]))
	chunks := (length.Uint64()-1)/32 + 1
	for n := range chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := new(uint256.Int).Add(base, new(uint256.Int).SetUint64(n)).Bytes32()
		result[crypto.Keccak256Hash(key[:])] = struct{}{}
	}
	return nil
}

func (s *source) legacyBalanceJobs(ctx context.Context, x *indexes, root common.Hash, metadata map[common.Hash]struct{}, l limiter) (map[common.Hash]*balanceJob, error) {
	jobs := make(map[common.Hash]*balanceJob)
	err := s.storage(ctx, ovmHash, root, l, func(k, v common.Hash) error {
		address, balance := x.balances[k]
		_, allowance := x.allowances[k]
		_, meta := metadata[k]
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
