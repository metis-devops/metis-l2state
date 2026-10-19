package migration

import (
	"context"
	"errors"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
)

type metadataString struct {
	base   uint256.Int
	chunks uint64
}

// Only the two string ranges and five fixed metadata keys are retained.
// Length declarations never allocate a derived-key index.
type metadataLayout struct {
	fixed   [5]common.Hash
	strings [2]metadataString
}

func (s *source) metadataSlots(ctx context.Context, root common.Hash) (metadataLayout, error) {
	var result metadataLayout
	for n := byte(0); n <= 6; n++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		var slot common.Hash
		slot[31] = n
		v, err := s.storageValue(ovmHash, root, slot)
		if err != nil {
			return result, err
		}
		if n < 2 {
			if v != (common.Hash{}) {
				return result, errors.New("nonzero OVM mapping base slot")
			}
			continue
		}
		result.fixed[n-2] = crypto.Keccak256Hash(slot[:])
		if n == 3 || n == 4 {
			result.strings[n-3], err = recognizeString(slot, v)
			if err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func recognizeString(slot, word common.Hash) (metadataString, error) {
	var result metadataString
	if word[31]&1 == 0 {
		length := int(word[31] / 2)
		if length > 31 {
			return result, errors.New("invalid short OVM string")
		}
		for _, b := range word[length:31] {
			if b != 0 {
				return result, errors.New("noncanonical OVM string padding")
			}
		}
		return result, nil
	}
	length := new(uint256.Int).Rsh(new(uint256.Int).SetBytes(word[:]), 1)
	if !length.IsUint64() || length.Uint64() < 32 {
		return result, errors.New("invalid long OVM string")
	}
	result.base.SetBytes(crypto.Keccak256(slot[:]))
	result.chunks = (length.Uint64()-1)/32 + 1
	return result, nil
}

// contains keeps the usual short-string path to five fixed-key comparisons.
// Long strings retain the original accepted length domain without allocating
// an expanded index. Their hash search is cancellable, but not CPU-bounded.
func (m *metadataLayout) contains(ctx context.Context, key common.Hash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	for _, h := range m.fixed {
		if h == key {
			return true, nil
		}
	}
	if m.strings[0].chunks == 0 && m.strings[1].chunks == 0 {
		return false, nil
	}
	hasher := crypto.NewKeccakState()
	var offset, slot uint256.Int
	var hash common.Hash
	var raw [32]byte
	for _, r := range m.strings {
		for n := uint64(0); n < r.chunks; n++ {
			if err := ctx.Err(); err != nil {
				return false, err
			}
			offset.SetUint64(n)
			raw = slot.Add(&r.base, &offset).Bytes32()
			hasher.Reset()
			_, _ = hasher.Write(raw[:])
			_, _ = hasher.Read(hash[:])
			if hash == key {
				return true, nil
			}
		}
	}
	return false, nil
}
