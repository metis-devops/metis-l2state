package migration

import (
	"encoding/binary"
	"maps"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/holiman/uint256"
)

func overlayCodeAccount(hash common.Hash) *types.StateAccount {
	return &types.StateAccount{Balance: new(uint256.Int), Root: types.EmptyRootHash, CodeHash: hash.Bytes()}
}

func TestOverlayCodeMembership(t *testing.T) {
	first, second := common.HexToHash("0x11"), common.HexToHash("0x22")
	for _, tc := range []struct {
		name                   string
		converted, final       common.Hash
		hasConverted, hasFinal bool
		want                   map[common.Hash]codeUse
	}{
		{"unchanged", first, first, true, true, map[common.Hash]codeUse{first: convertedCode | finalCode}},
		{"replaced", first, second, true, true, map[common.Hash]codeUse{first: convertedCode, second: finalCode}},
		{"cleared", first, types.EmptyCodeHash, true, true, map[common.Hash]codeUse{first: convertedCode}},
		{"added", types.EmptyCodeHash, second, true, true, map[common.Hash]codeUse{second: finalCode}},
		{"empty", types.EmptyCodeHash, types.EmptyCodeHash, true, true, map[common.Hash]codeUse{}},
		{"created-account", types.EmptyCodeHash, second, false, true, map[common.Hash]codeUse{second: finalCode}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAccountAccumulator(walkPlan{mode: overlayState}, common.Hash{})
			d := &accountDone{
				converted: overlayCodeAccount(tc.converted), final: overlayCodeAccount(tc.final),
				hasConverted: tc.hasConverted, hasFinal: tc.hasFinal,
				convertedSlots: 2, finalSlots: 3,
			}
			must(t, a.accept(d))
			if !maps.Equal(codeIndexEntries(&a.result.codes), tc.want) {
				t.Fatal("overlay changed code membership")
			}
			if a.result.counts.Accounts != 1 || a.result.counts.StorageSlots != 3 {
				t.Fatal("lost final account/storage inventory")
			}
			if tc.hasConverted {
				if a.result.convertedCounts.Accounts != 1 || a.result.convertedCounts.StorageSlots != 2 {
					t.Fatal("lost converted account/storage inventory")
				}
			} else if a.result.convertedCounts.Accounts != 0 || a.result.convertedCounts.StorageSlots != 0 {
				t.Fatal("alloc-created account counted before alloc")
			}
		})
	}
}

// Shared stage membership must occupy one stable index per code hash.
func TestOverlaySharedCodeUsesOneIndex(t *testing.T) {
	a := newAccountAccumulator(walkPlan{mode: overlayState}, common.Hash{})
	for i := 1; i <= codeNodeMax; i++ {
		var hash common.Hash
		binary.BigEndian.PutUint64(hash[24:], uint64(i))
		must(t, a.accept(&accountDone{
			job:       accountJob{hash: hash},
			converted: overlayCodeAccount(hash), final: overlayCodeAccount(hash),
			hasConverted: true, hasFinal: true,
		}))
	}
	if a.result.codes.items.length != codeNodeMax {
		t.Fatal("duplicate code index")
	}
	entries := codeIndexEntries(&a.result.codes)
	if len(entries) != codeNodeMax {
		t.Fatal("lost distinct code references")
	}
	for _, use := range entries {
		if use != convertedCode|finalCode {
			t.Fatal("lost shared membership")
		}
	}
}
