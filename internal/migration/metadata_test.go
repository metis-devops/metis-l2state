package migration

import (
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestOVMStorageClassification(t *testing.T) {
	for _, name := range []string{"unknown", "mapping-base", "short-padding", "short-length", "long-length", "long-zero", "short-valid"} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureWithStorage(t, 0, 0, func(storage map[common.Hash]common.Hash) {
				slot := common.HexToHash("0x03")
				var word common.Hash
				switch name {
				case "unknown":
					slot = common.HexToHash("0x90")
					word[31] = 1
				case "mapping-base":
					slot = common.Hash{}
					word[31] = 1
				case "short-padding":
					word[0], word[1], word[31] = 'a', 'b', 2
				case "short-length":
					word[31] = 64
				case "long-length":
					word[31] = 63
				case "long-zero":
					// Zero chunks are absent from the source trie but still recognized.
					word = common.BigToHash(big.NewInt(1024*2 + 1))
				case "short-valid":
					word[0], word[31] = 'a', 2
				}
				storage[crypto.Keccak256Hash(slot[:])] = word
			})
			_, err := Migrate(t.Context(), f.options(filepath.Join(t.TempDir(), "state")))
			if name == "long-zero" || name == "short-valid" {
				must(t, err)
			} else if err == nil {
				t.Fatal("accepted unrecognized or malformed OVM storage")
			}
		})
	}
}
